package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codigate/agent/config"
	"github.com/codigate/agent/protocol"
)

func healthFor(t *testing.T, yaml string) *Health {
	t.Helper()
	cfg, err := config.Parse([]byte("edge: https://edge.example.com\ntoken: t\ningress:\n"+yaml), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	h := NewHealth(NewIngress(cfg, testLogger()), testLogger())
	h.Timeout = 500 * time.Millisecond
	return h
}

func statusServer(code int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
}

func TestHealthHTTPStatuses(t *testing.T) {
	ok, notFound, broken := statusServer(200), statusServer(404), statusServer(503)
	defer ok.Close()
	defer notFound.Close()
	defer broken.Close()
	h := healthFor(t, fmt.Sprintf(`  - hostname: ok.example.com
    service: %s
  - hostname: nf.example.com
    service: %s
  - hostname: br.example.com
    service: %s
  - hostname: refused.example.com
    service: http://127.0.0.1:1
  - hostname: teapot.example.com
    service: http_status:418
`, ok.URL, notFound.URL, broken.URL))
	want := map[string]string{
		"ok.example.com":      protocol.HealthUp,
		"nf.example.com":      protocol.HealthUp, // any answer below 500 means the origin is there
		"br.example.com":      protocol.HealthDown,
		"refused.example.com": protocol.HealthDown,
		"teapot.example.com":  protocol.HealthUnknown,
		"none.example.com":    protocol.HealthDown,
	}
	for host, status := range want {
		r := h.Check(context.Background(), host)
		if r.Status != status {
			t.Errorf("%s: got %s (%s), want %s", host, r.Status, r.Error, status)
		}
		if status == protocol.HealthDown && r.Error == "" {
			t.Errorf("%s: down without an error", host)
		}
	}
	if r := h.Check(context.Background(), "br.example.com"); r.Error != "origin answered 503" {
		t.Errorf("error text %q", r.Error)
	}
}

func TestHealthHonoursNoTLSVerifyAndHostHeader(t *testing.T) {
	var gotHost string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotHost = r.Host }))
	defer srv.Close()
	h := healthFor(t, fmt.Sprintf(`  - hostname: strict.example.com
    service: %[1]s
  - hostname: lax.example.com
    service: %[1]s
    originRequest:
      noTLSVerify: true
  - hostname: vhost.example.com
    service: %[1]s
    originRequest:
      noTLSVerify: true
      httpHostHeader: internal.lan
`, srv.URL))
	if r := h.Check(context.Background(), "strict.example.com"); r.Status != protocol.HealthDown {
		t.Errorf("self-signed origin without noTLSVerify: got %s", r.Status)
	}
	if r := h.Check(context.Background(), "lax.example.com"); r.Status != protocol.HealthUp {
		t.Errorf("noTLSVerify: got %s (%s)", r.Status, r.Error)
	}
	if gotHost != "lax.example.com" {
		t.Errorf("probe Host %q, want the public hostname", gotHost)
	}
	h.Check(context.Background(), "vhost.example.com")
	if gotHost != "internal.lan" {
		t.Errorf("probe Host %q, want httpHostHeader", gotHost)
	}
}

func TestHealthTCPAndTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer slow.Close()
	h := healthFor(t, fmt.Sprintf(`  - hostname: db.example.com
    service: tcp://%s
  - hostname: gone.example.com
    service: tcp://127.0.0.1:1
  - hostname: slow.example.com
    service: %s
`, ln.Addr(), slow.URL))
	if r := h.Check(context.Background(), "db.example.com"); r.Status != protocol.HealthUp {
		t.Errorf("tcp listener: got %s (%s)", r.Status, r.Error)
	}
	if r := h.Check(context.Background(), "gone.example.com"); r.Status != protocol.HealthDown {
		t.Errorf("closed tcp port: got %s", r.Status)
	}
	start := time.Now()
	r := h.Check(context.Background(), "slow.example.com")
	if r.Status != protocol.HealthDown || r.Error != "timeout" || time.Since(start) > 2*time.Second {
		t.Errorf("slow origin: got %s %q after %s", r.Status, r.Error, time.Since(start))
	}
}

func TestHealthReportCachesAndWatches(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer srv.Close()
	h := healthFor(t, "  - hostname: a.example.com\n    service: "+srv.URL+"\n")
	now := time.Now()
	h.now = func() time.Time { return now }
	for range 3 {
		if got := h.Report(context.Background(), []string{"a.example.com"}); got[0].Status != protocol.HealthUp {
			t.Fatalf("got %+v", got)
		}
	}
	if calls != 1 {
		t.Fatalf("report checked %d times; later reports must use the cached result", calls)
	}
	if w := h.watched(); len(w) != 1 {
		t.Fatalf("watched %v", w)
	}
	now = now.Add(healthWatch + time.Second)
	if w := h.watched(); len(w) != 0 {
		t.Fatalf("hostname still watched after the edge stopped asking: %v", w)
	}
}

func TestHealthStreamThroughTunnel(t *testing.T) {
	upstream := statusServer(204)
	defer upstream.Close()
	edge := newFakeEdge(t, "tok")
	cfg, err := config.Parse([]byte(fmt.Sprintf("edge: %s\nallow_insecure_edge: true\ntoken: tok\ningress:\n  - hostname: app.example.com\n    service: %s\n", edge.URL, upstream.URL)), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	in := NewIngress(cfg, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{EdgeURL: cfg.Edge, Token: "tok", Version: "test", Handler: in, Logger: testLogger(), Health: NewHealth(in, testLogger())}
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	s := waitSession(t, edge)
	if f := <-edge.features; f != protocol.FeatureHealth {
		t.Fatalf("features header %q", f)
	}

	stream, err := s.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := protocol.WriteStreamHeader(stream, protocol.StreamHeader{Kind: protocol.KindHealth}); err != nil {
		t.Fatal(err)
	}
	json.NewEncoder(stream).Encode(protocol.HealthRequest{Hosts: []string{"app.example.com", "other.example.com"}})
	stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := protocol.ReadHealthResponse(stream)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 || resp.Results[0].Status != protocol.HealthUp || resp.Results[1].Status != protocol.HealthDown {
		t.Fatalf("got %+v", resp)
	}
	// Proxied streams keep working on the same session.
	if r := get(t, s, "app.example.com", "/"); r.StatusCode != 204 {
		t.Fatalf("proxy after health stream: %d", r.StatusCode)
	}
}

func TestNoHealthFeatureWithoutChecker(t *testing.T) {
	edge := newFakeEdge(t, "tok")
	startAgent(t, edge.URL, "tok", "http://127.0.0.1:1")
	waitSession(t, edge)
	if f := <-edge.features; strings.Contains(f, protocol.FeatureHealth) {
		t.Fatalf("agent without a checker announced %q", f)
	}
}
