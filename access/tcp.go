// Package access connects to a TCP app (tcp:// or ssh:// rule) through the Codigate edge.
//
// The client opens TLS to the app's hostname on port 443, sends an HTTP/1.1 upgrade to
// codigate-tcp/1 with the service token headers, and after "101 Switching Protocols" the
// connection carries the raw TCP stream. Protected apps need a service token.
package access

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/codigate/agent/protocol"
)

// Headers carrying a service token, as checked by the edge.
const (
	HeaderClientID     = "Codigate-Client-Id"
	HeaderClientSecret = "Codigate-Client-Secret"
)

// Dialer opens TCP connections to one app.
type Dialer struct {
	Hostname     string      // the app's public hostname
	Address      string      // host:port to connect to; default Hostname:443
	ClientID     string      // service token id (cgs_…)
	ClientSecret string      // service token secret
	TLSConfig    *tls.Config // optional (private CA in development)
}

// Dial returns a connection to the app's TCP service.
func (d *Dialer) Dial(ctx context.Context) (net.Conn, error) {
	addr := d.Address
	if addr == "" {
		addr = net.JoinHostPort(d.Hostname, "443")
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if d.TLSConfig != nil {
		cfg = d.TLSConfig.Clone()
	}
	cfg.ServerName = d.Hostname
	cfg.NextProtos = []string{"http/1.1"} // an upgrade needs HTTP/1.1
	td := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: cfg}
	conn, err := td.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return d.Upgrade(ctx, conn)
}

// Upgrade asks for the TCP service on an open connection to the edge. It closes conn on failure.
func (d *Dialer) Upgrade(ctx context.Context, conn net.Conn) (net.Conn, error) {
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+d.Hostname+"/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", protocol.TCPUpgradeToken)
	req.Header.Set("User-Agent", "codigate-access")
	if d.ClientID != "" {
		req.Header.Set(HeaderClientID, d.ClientID)
		req.Header.Set(HeaderClientSecret, d.ClientSecret)
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if res.StatusCode != http.StatusSwitchingProtocols || !strings.EqualFold(res.Header.Get("Upgrade"), protocol.TCPUpgradeToken) {
		defer conn.Close()
		return nil, refusal(res)
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}

func refusal(res *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	reason := res.Header.Get("Codigate-Error")
	switch {
	case reason == "login-required" || res.StatusCode == http.StatusUnauthorized:
		return errors.New("the app needs a service token: pass --client-id/--client-secret or set CODIGATE_CLIENT_ID/CODIGATE_CLIENT_SECRET")
	case res.StatusCode == http.StatusForbidden:
		return errors.New("the service token has no access to this app")
	case reason == "agent-offline":
		return errors.New("the agent for this app is offline")
	case reason == "origin-unreachable":
		return errors.New("the agent cannot reach the TCP service")
	case res.StatusCode == http.StatusOK || res.StatusCode == http.StatusBadGateway && reason == "":
		return errors.New("the app is not a TCP service (its agent rule must be tcp:// or ssh://)")
	}
	msg := strings.TrimSpace(string(body))
	if reason != "" {
		msg = reason + ": " + msg
	}
	return fmt.Errorf("edge answered %s: %s", res.Status, msg)
}

// bufferedConn reads first what the response parser already buffered.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
