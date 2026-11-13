// Package tunnel connects the agent to the edge and serves proxied requests from it.
package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/codigate/agent/protocol"
)

// ErrUnauthorized means the edge rejected the agent token. Retrying won't help until the token is
// fixed, so the client backs off to the maximum delay.
var ErrUnauthorized = errors.New("tunnel: edge rejected the agent token")

// Client keeps one tunnel session to an edge open and serves streams with Handler.
type Client struct {
	EdgeURL string
	Token   string
	Version string
	// Handler serves every proxied request. The request context carries the stream's hostname
	// (see HostnameFromContext).
	Handler http.Handler
	Logger  *slog.Logger

	// DialContext overrides how the TCP connection to the edge is made (tests). Proxy settings
	// from the environment (HTTPS_PROXY, NO_PROXY) are used when nil.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// TLSConfig overrides the TLS client config (tests, private CAs).
	TLSConfig *tls.Config

	// Backoff tuning; zero values use the defaults.
	MinBackoff, MaxBackoff time.Duration
	// OnSession is called after each successful connect (tests and metrics).
	OnSession func(agentID, edgeID string)
}

// Run connects, serves and reconnects until ctx is cancelled.
func (c *Client) Run(ctx context.Context) error {
	minB, maxB := c.MinBackoff, c.MaxBackoff
	if minB == 0 {
		minB = 500 * time.Millisecond
	}
	if maxB == 0 {
		maxB = 30 * time.Second
	}
	attempt := 0
	for {
		started := time.Now()
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		// A session that lived a while was healthy; start the backoff from scratch.
		if time.Since(started) > 30*time.Second {
			attempt = 0
		}
		delay := backoff(attempt, minB, maxB)
		if errors.Is(err, ErrUnauthorized) {
			delay = maxB
		}
		attempt++
		c.Logger.Warn("tunnel disconnected, reconnecting", "err", err, "in", delay.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// backoff is exponential with full jitter: a random delay in [min, min(max, min*2^attempt)].
func backoff(attempt int, minB, maxB time.Duration) time.Duration {
	ceil := maxB
	if attempt < 30 {
		if d := minB << attempt; d > 0 && d < maxB {
			ceil = d
		}
	}
	return minB + rand.N(ceil-minB+1)
}

func (c *Client) runOnce(ctx context.Context) error {
	conn, agentID, edgeID, err := c.connect(ctx)
	if err != nil {
		return err
	}
	session, err := yamux.Server(conn, protocol.YamuxConfig())
	if err != nil {
		conn.Close()
		return err
	}
	c.Logger.Info("tunnel connected", "agent_id", agentID, "edge_id", edgeID)
	if c.OnSession != nil {
		c.OnSession(agentID, edgeID)
	}
	return c.serve(ctx, session)
}

// connect performs the HTTP Upgrade and returns the raw bidirectional connection.
func (c *Client) connect(ctx context.Context) (io.ReadWriteCloser, string, string, error) {
	u, err := url.Parse(c.EdgeURL)
	if err != nil {
		return nil, "", "", err
	}
	u.Path = protocol.ConnectPath
	tlsCfg := c.TLSConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tlsCfg = tlsCfg.Clone()
	// HTTP/2 can't be upgraded; force HTTP/1.1 via ALPN.
	tlsCfg.NextProtos = []string{"http/1.1"}
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         c.DialContext,
		TLSClientConfig:     tlsCfg,
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   false,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		DisableKeepAlives:   true,
	}
	if tr.DialContext == nil {
		tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, u.String(), nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", protocol.UpgradeToken)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set(protocol.HeaderAgentVersion, c.Version)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("tunnel: connect: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, "", "", fmt.Errorf("%w (%d)", ErrUnauthorized, resp.StatusCode)
		}
		return nil, "", "", fmt.Errorf("tunnel: edge answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, "", "", errors.New("tunnel: upgraded body is not writable")
	}
	// The upgrade request's context must not cancel the long-lived connection.
	cancel()
	return rwc, resp.Header.Get(protocol.HeaderAgentID), resp.Header.Get(protocol.HeaderEdgeID), nil
}

// serve accepts streams until the session dies or ctx is cancelled. Each stream's header is read
// in its own goroutine so a slow or malicious stream can't block the accept loop.
func (c *Client) serve(ctx context.Context, session *yamux.Session) error {
	ln := newStreamListener(session.LocalAddr())
	srv := &http.Server{
		Handler:           c.Handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
		ErrorLog:          slog.NewLogLogger(c.Logger.Handler(), slog.LevelDebug),
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			if sc, ok := conn.(*streamConn); ok {
				return context.WithValue(ctx, hostnameKey{}, sc.hostname)
			}
			return ctx
		},
	}
	go srv.Serve(ln)

	acceptErr := make(chan error, 1)
	go func() {
		for {
			stream, err := session.AcceptStream()
			if err != nil {
				acceptErr <- err
				return
			}
			go func() {
				stream.SetReadDeadline(time.Now().Add(10 * time.Second))
				h, err := protocol.ReadStreamHeader(stream)
				if err != nil {
					c.Logger.Warn("rejecting stream with bad header", "err", err)
					stream.Close()
					return
				}
				stream.SetReadDeadline(time.Time{})
				ln.push(&streamConn{Conn: stream, hostname: h.Hostname})
			}()
		}
	}()

	var err error
	select {
	case <-ctx.Done():
		// Let in-flight requests finish, then close the session.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		srv.Shutdown(shutdownCtx)
		cancel()
		session.Close()
		err = ctx.Err()
	case err = <-acceptErr:
		srv.Close()
	case <-session.CloseChan():
		srv.Close()
		err = errors.New("tunnel: session closed")
	}
	ln.Close()
	return err
}

type hostnameKey struct{}

// HostnameFromContext returns the public hostname the edge routed a request for.
func HostnameFromContext(ctx context.Context) string {
	h, _ := ctx.Value(hostnameKey{}).(string)
	return h
}

type streamConn struct {
	net.Conn
	hostname string
}

// streamListener hands already-validated streams to http.Server.
type streamListener struct {
	addr      net.Addr
	conns     chan net.Conn
	done      chan struct{}
	closeOnce sync.Once
}

func newStreamListener(addr net.Addr) *streamListener {
	return &streamListener{addr: addr, conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *streamListener) push(c net.Conn) {
	select {
	case l.conns <- c:
	case <-l.done:
		c.Close()
	}
}

func (l *streamListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close is idempotent: http.Server closes its listeners too.
func (l *streamListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return nil
}

func (l *streamListener) Addr() net.Addr { return l.addr }
