package tunnel

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/codigate/agent/config"
	"github.com/codigate/agent/protocol"
)

// fakeEdge accepts agent upgrades and exposes the yamux sessions to the test.
type fakeEdge struct {
	*httptest.Server
	token    string
	sessions chan *yamux.Session
}

func newFakeEdge(t *testing.T, token string) *fakeEdge {
	e := &fakeEdge{token: token, sessions: make(chan *yamux.Session, 8)}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != protocol.ConnectPath || r.Header.Get("Upgrade") != protocol.UpgradeToken {
			http.Error(w, "bad upgrade", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+e.token {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n%s: agent-1\r\n%s: edge-test\r\n\r\n",
			protocol.UpgradeToken, protocol.HeaderAgentID, protocol.HeaderEdgeID)
		brw.Flush()
		s, err := yamux.Client(conn, protocol.YamuxConfig())
		if err != nil {
			t.Error(err)
			return
		}
		e.sessions <- s
	}))
	t.Cleanup(e.Close)
	return e
}

// get sends one HTTP request for host through a session, as the real edge does.
func get(t *testing.T, s *yamux.Session, host, path string) *http.Response {
	t.Helper()
	stream, err := s.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })
	if err := protocol.WriteStreamHeader(stream, protocol.StreamHeader{Hostname: host}); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(stream, "GET %s HTTP/1.1\r\nHost: %s\r\nX-Forwarded-For: 203.0.113.7\r\n\r\n", path, host)
	resp, err := http.ReadResponse(bufio.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func startAgent(t *testing.T, edgeURL, token, upstream string) context.CancelFunc {
	t.Helper()
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
edge: %s
allow_insecure_edge: true
token: %s
ingress:
  - hostname: app.example.com
    service: %s
  - hostname: down.example.com
    service: http://127.0.0.1:1
  - hostname: teapot.example.com
    service: http_status:418
`, edgeURL, token, upstream)), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{EdgeURL: cfg.Edge, Token: cfg.Token, Version: "test", Handler: IngressHandler(cfg, testLogger()),
		Logger: testLogger(), MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond}
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return cancel
}

func waitSession(t *testing.T, e *fakeEdge) *yamux.Session {
	t.Helper()
	select {
	case s := <-e.sessions:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not connect")
		return nil
	}
}

func TestProxiesThroughTunnel(t *testing.T) {
	var gotHost, gotXFF string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotXFF = r.Host, r.Header.Get("X-Forwarded-For")
		io.WriteString(w, "hello from "+r.URL.Path)
	}))
	defer upstream.Close()
	edge := newFakeEdge(t, "tok")
	startAgent(t, edge.URL, "tok", upstream.URL)
	s := waitSession(t, edge)

	resp := get(t, s, "app.example.com", "/path")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "hello from /path" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if gotHost != "app.example.com" {
		t.Errorf("origin saw Host %q, want the public hostname", gotHost)
	}
	if !strings.HasPrefix(gotXFF, "203.0.113.7") {
		t.Errorf("origin saw X-Forwarded-For %q, want the edge's value first", gotXFF)
	}

	if resp := get(t, s, "teapot.example.com", "/"); resp.StatusCode != 418 {
		t.Errorf("http_status rule: got %d", resp.StatusCode)
	}
	if resp := get(t, s, "down.example.com", "/"); resp.StatusCode != 502 || resp.Header.Get(HeaderError) != "origin-unreachable" {
		t.Errorf("unreachable origin: got %d %q", resp.StatusCode, resp.Header.Get(HeaderError))
	}
	if resp := get(t, s, "unknown.example.com", "/"); resp.StatusCode != 502 || resp.Header.Get(HeaderError) != "no-ingress-rule" {
		t.Errorf("no rule: got %d %q", resp.StatusCode, resp.Header.Get(HeaderError))
	}
}

func TestRoutesByStreamHeaderNotHostHeader(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "app") }))
	defer upstream.Close()
	edge := newFakeEdge(t, "tok")
	startAgent(t, edge.URL, "tok", upstream.URL)
	s := waitSession(t, edge)

	stream, _ := s.OpenStream()
	defer stream.Close()
	protocol.WriteStreamHeader(stream, protocol.StreamHeader{Hostname: "teapot.example.com"})
	io.WriteString(stream, "GET / HTTP/1.1\r\nHost: app.example.com\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 418 {
		t.Fatalf("a forged Host header changed routing: got %d", resp.StatusCode)
	}
}

func TestBadStreamHeaderIsDropped(t *testing.T) {
	edge := newFakeEdge(t, "tok")
	startAgent(t, edge.URL, "tok", "http://127.0.0.1:1")
	s := waitSession(t, edge)
	stream, _ := s.OpenStream()
	io.WriteString(stream, "GET / HTTP/1.1\r\nHost: app.example.com\r\n\r\n")
	stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := stream.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("expected the stream to be closed, got n=%d err=%v", n, err)
	}
	// The session must survive one bad stream.
	if resp := get(t, s, "teapot.example.com", "/"); resp.StatusCode != 418 {
		t.Fatalf("session broken after bad stream: %d", resp.StatusCode)
	}
}

func TestReconnectsAfterSessionLoss(t *testing.T) {
	edge := newFakeEdge(t, "tok")
	startAgent(t, edge.URL, "tok", "http://127.0.0.1:1")
	s := waitSession(t, edge)
	s.Close()
	s2 := waitSession(t, edge)
	if resp := get(t, s2, "teapot.example.com", "/"); resp.StatusCode != 418 {
		t.Fatalf("after reconnect: %d", resp.StatusCode)
	}
}

func TestUnauthorizedKeepsRetrying(t *testing.T) {
	edge := newFakeEdge(t, "right")
	c := &Client{EdgeURL: edge.URL, Token: "wrong", Handler: http.NotFoundHandler(), Logger: testLogger()}
	_, _, _, err := c.connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestConcurrentStreams(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		io.WriteString(w, r.URL.Path)
	}))
	defer upstream.Close()
	edge := newFakeEdge(t, "tok")
	startAgent(t, edge.URL, "tok", upstream.URL)
	s := waitSession(t, edge)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := get(t, s, "app.example.com", fmt.Sprintf("/%d", i))
			b, _ := io.ReadAll(resp.Body)
			if string(b) != fmt.Sprintf("/%d", i) {
				t.Errorf("stream %d got %q", i, b)
			}
		}()
	}
	wg.Wait()
}

func TestBackoffBounds(t *testing.T) {
	for attempt := range 40 {
		d := backoff(attempt, 100*time.Millisecond, 5*time.Second)
		if d < 100*time.Millisecond || d > 5*time.Second {
			t.Fatalf("attempt %d: %v out of bounds", attempt, d)
		}
	}
}
