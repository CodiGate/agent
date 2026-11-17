package tunnel

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codigate/agent/config"
	"github.com/codigate/agent/protocol"
)

const (
	// HealthInterval is how often the agent checks each origin the edge asked about (P3-12).
	HealthInterval = 30 * time.Second
	// HealthTimeout bounds one check.
	HealthTimeout = 5 * time.Second
	// healthWatch is how long a hostname stays checked after the edge last asked about it.
	healthWatch = 3 * time.Minute
)

// Health checks the origins behind the hostnames the edge routes to this agent. An HTTP(S) origin
// is up when a GET answers with any status below 500; a TCP origin is up when it accepts a
// connection. http_status rules have no origin and report unknown. The edge asks over a health
// stream (see protocol.KindHealth); the agent never pushes, so older edges see nothing new.
type Health struct {
	Ingress  *Ingress
	Logger   *slog.Logger
	Interval time.Duration // zero means HealthInterval
	Timeout  time.Duration // zero means HealthTimeout

	mu      sync.Mutex
	results map[string]protocol.HealthResult
	asked   map[string]time.Time
	clients map[string]*http.Client // by rule key, so checks reuse origin connections
	now     func() time.Time
}

// NewHealth returns a checker for the rules of in.
func NewHealth(in *Ingress, logger *slog.Logger) *Health {
	return &Health{Ingress: in, Logger: logger}
}

func (h *Health) init() {
	if h.results == nil {
		h.results = map[string]protocol.HealthResult{}
		h.asked = map[string]time.Time{}
		h.clients = map[string]*http.Client{}
	}
	if h.now == nil {
		h.now = time.Now
	}
}

func (h *Health) timeout() time.Duration {
	if h.Timeout > 0 {
		return h.Timeout
	}
	return HealthTimeout
}

// Run re-checks every watched hostname each interval until ctx is done.
func (h *Health) Run(ctx context.Context) {
	iv := h.Interval
	if iv <= 0 {
		iv = HealthInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.checkAll(ctx, h.watched())
		}
	}
}

func (h *Health) watched() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.init()
	cutoff := h.now().Add(-healthWatch)
	var hosts []string
	for host, at := range h.asked {
		if at.Before(cutoff) {
			delete(h.asked, host)
			delete(h.results, host)
			continue
		}
		hosts = append(hosts, host)
	}
	return hosts
}

// Report returns the latest result for each host, checking hosts it has not seen yet first.
func (h *Health) Report(ctx context.Context, hosts []string) []protocol.HealthResult {
	h.mu.Lock()
	h.init()
	var fresh []string
	now := h.now()
	for _, host := range hosts {
		h.asked[host] = now
		if _, ok := h.results[host]; !ok {
			fresh = append(fresh, host)
		}
	}
	h.mu.Unlock()
	h.checkAll(ctx, fresh)
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]protocol.HealthResult, 0, len(hosts))
	for _, host := range hosts {
		r, ok := h.results[host]
		if !ok {
			r = protocol.HealthResult{Host: host, Status: protocol.HealthUnknown}
		}
		out = append(out, r)
	}
	return out
}

func (h *Health) checkAll(ctx context.Context, hosts []string) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for _, host := range hosts {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r := h.Check(ctx, host)
			h.mu.Lock()
			prev, seen := h.results[host]
			h.results[host] = r
			h.mu.Unlock()
			if h.Logger != nil && seen && prev.Status != r.Status {
				h.Logger.Info("origin health changed", "host", host, "status", r.Status, "err", r.Error)
			}
		}()
	}
	wg.Wait()
}

// Check probes the origin of host once.
func (h *Health) Check(ctx context.Context, host string) protocol.HealthResult {
	res := protocol.HealthResult{Host: host, Status: protocol.HealthUp}
	rule := h.Ingress.Match(host)
	if rule == nil {
		res.Status, res.Error = protocol.HealthDown, "no ingress rule for this hostname"
		return res
	}
	if rule.Status() != 0 {
		res.Status = protocol.HealthUnknown
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, h.timeout())
	defer cancel()
	var err error
	if addr := rule.TCPAddr(); addr != "" {
		var c net.Conn
		c, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err == nil {
			c.Close()
		}
	} else {
		err = h.checkHTTP(ctx, rule, host)
	}
	if err != nil {
		res.Status, res.Error = protocol.HealthDown, protocol.TruncateHealthError(healthError(err))
	}
	return res
}

func (h *Health) checkHTTP(ctx context.Context, rule *config.Rule, host string) error {
	target := rule.Target()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	req.Host = host
	if hh := rule.OriginRequest.HTTPHostHeader; hh != "" {
		req.Host = hh
	}
	req.Header.Set("User-Agent", "codigate-agent-health")
	resp, err := h.client(rule).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 500 {
		return fmt.Errorf("origin answered %d", resp.StatusCode)
	}
	return nil
}

func (h *Health) client(rule *config.Rule) *http.Client {
	k := ruleKey(rule)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.init()
	if c := h.clients[k]; c != nil {
		return c
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 2
	if rule.Target().Scheme == "https" {
		tr.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: rule.OriginRequest.NoTLSVerify, //nolint:gosec // opt-in per rule, as in cloudflared
			ServerName:         rule.OriginRequest.OriginServerName,
		}
	}
	c := &http.Client{
		Transport: tr,
		// A redirect is an answer: the origin is up.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	h.clients[k] = c
	return c
}

// healthError turns a probe error into a short message without the request URL repeated.
func healthError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var ue interface{ Unwrap() error }
	msg := err.Error()
	if errors.As(err, &ue) && strings.HasPrefix(msg, "Get ") {
		if inner := ue.Unwrap(); inner != nil {
			msg = inner.Error()
		}
	}
	return msg
}

// serveHealth answers one health stream: read a request, write the results.
func (h *Health) serveHealth(ctx context.Context, stream io.ReadWriteCloser) error {
	defer stream.Close()
	req, err := protocol.ReadHealthRequest(stream)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, h.timeout()+2*time.Second)
	defer cancel()
	return json.NewEncoder(stream).Encode(protocol.HealthResponse{Results: h.Report(ctx, req.Hosts)})
}
