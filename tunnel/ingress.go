package tunnel

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codigate/agent/config"
	"github.com/codigate/agent/protocol"
)

// HeaderError tells the edge why the agent couldn't serve a request, so it can show a useful page.
const HeaderError = "Codigate-Error"

// Ingress routes each request to the local service of the matching rule. Routing uses the hostname
// from the stream header, never the request's Host header, which the edge forwards unchanged from
// the browser. Rules from the config file are fixed; rules found at runtime (Docker labels) can be
// replaced with SetDynamic. For an exact hostname in both, the config file wins.
type Ingress struct {
	static  *config.Config
	dynamic atomic.Pointer[map[string]*config.Rule]
	logger  *slog.Logger

	mu      sync.Mutex
	proxies map[string]*httputil.ReverseProxy // by rule key, so a refresh keeps origin connections
}

// NewIngress serves the rules of cfg.
func NewIngress(cfg *config.Config, logger *slog.Logger) *Ingress {
	in := &Ingress{static: cfg, logger: logger, proxies: map[string]*httputil.ReverseProxy{}}
	in.dynamic.Store(&map[string]*config.Rule{})
	return in
}

// IngressHandler is NewIngress for a fixed configuration.
func IngressHandler(cfg *config.Config, logger *slog.Logger) http.Handler {
	return NewIngress(cfg, logger)
}

// SetDynamic replaces the runtime rules (exact hostnames only).
func (in *Ingress) SetDynamic(rules []config.Rule) {
	m := make(map[string]*config.Rule, len(rules))
	for i := range rules {
		m[rules[i].Hostname] = &rules[i]
	}
	in.dynamic.Store(&m)
	in.mu.Lock()
	defer in.mu.Unlock()
	live := map[string]bool{}
	for i := range in.static.Ingress {
		live[ruleKey(&in.static.Ingress[i])] = true
	}
	for _, r := range m {
		live[ruleKey(r)] = true
	}
	for k := range in.proxies {
		if !live[k] {
			delete(in.proxies, k)
		}
	}
}

// Match returns the rule for host: an exact rule from the file, then a runtime rule, then the
// file's wildcard and catch-all rules.
func (in *Ingress) Match(host string) *config.Rule {
	for i := range in.static.Ingress {
		if in.static.Ingress[i].Hostname == host {
			return &in.static.Ingress[i]
		}
	}
	if r := (*in.dynamic.Load())[host]; r != nil {
		return r
	}
	return in.static.Match(host)
}

func (in *Ingress) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	host := HostnameFromContext(req.Context())
	rule := in.Match(host)
	switch {
	case rule == nil:
		in.logger.Warn("no ingress rule for hostname", "host", host)
		w.Header().Set(HeaderError, "no-ingress-rule")
		http.Error(w, "codigate: this agent has no ingress rule for "+host, http.StatusBadGateway)
	case rule.Status() != 0:
		w.WriteHeader(rule.Status())
	case rule.TCPAddr() != "":
		in.serveTCP(w, req, rule)
	default:
		in.proxy(rule).ServeHTTP(w, req)
	}
}

func ruleKey(r *config.Rule) string {
	o := r.OriginRequest
	return strings.Join([]string{r.Service, o.HTTPHostHeader, o.OriginServerName, map[bool]string{true: "1"}[o.NoTLSVerify]}, "\x00")
}

func (in *Ingress) proxy(r *config.Rule) *httputil.ReverseProxy {
	k := ruleKey(r)
	in.mu.Lock()
	defer in.mu.Unlock()
	p := in.proxies[k]
	if p == nil {
		p = newProxy(r, in.logger)
		in.proxies[k] = p
	}
	return p
}

// serveTCP answers a codigate-tcp/1 upgrade by connecting to the rule's TCP service and copying
// bytes both ways until either side closes.
func (in *Ingress) serveTCP(w http.ResponseWriter, req *http.Request, rule *config.Rule) {
	if !strings.EqualFold(req.Header.Get("Upgrade"), protocol.TCPUpgradeToken) {
		w.Header().Set(HeaderError, "tcp-service")
		http.Error(w, "codigate: this is a TCP service; connect with: codigate-agent access tcp "+HostnameFromContext(req.Context()), http.StatusUpgradeRequired)
		return
	}
	origin, err := net.DialTimeout("tcp", rule.TCPAddr(), 10*time.Second)
	if err != nil {
		in.logger.Warn("origin unreachable", "service", rule.Service, "err", err)
		w.Header().Set(HeaderError, "origin-unreachable")
		http.Error(w, "codigate: the origin service is unreachable", http.StatusBadGateway)
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		origin.Close()
		http.Error(w, "codigate: cannot switch protocols", http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	defer origin.Close()
	_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + protocol.TCPUpgradeToken + "\r\n\r\n")
	if err := brw.Flush(); err != nil {
		return
	}
	Pipe(conn, io.MultiReader(brw.Reader, conn), origin)
}

// Pipe copies a→b and b→a until both directions end, half-closing where the connection allows.
// ar is what to read from a (a reader that may hold already-buffered bytes).
func Pipe(a io.Writer, ar io.Reader, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(b, ar)
		closeWrite(b)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(a, b)
		closeWrite(a)
		done <- struct{}{}
	}()
	<-done
	<-done
}

func closeWrite(w any) {
	if cw, ok := w.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else if c, ok := w.(io.Closer); ok {
		_ = c.Close()
	}
}

func newProxy(r *config.Rule, logger *slog.Logger) *httputil.ReverseProxy {
	target := r.Target()
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	// Many tunnel streams share one origin; Go's default of 2 idle connections per host forces a
	// new origin connection for most concurrent requests.
	tr.MaxIdleConnsPerHost = 64
	if target.Scheme == "https" {
		tr.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: r.OriginRequest.NoTLSVerify, //nolint:gosec // opt-in per rule, as in cloudflared
			ServerName:         r.OriginRequest.OriginServerName,
		}
	}
	hostHeader := r.OriginRequest.HTTPHostHeader
	return &httputil.ReverseProxy{
		Transport: tr,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Like cloudflared, pass the public hostname to the origin unless overridden.
			pr.Out.Host = pr.In.Host
			if hostHeader != "" {
				pr.Out.Host = hostHeader
			}
			// The edge is the only peer and sets these from the real client; keep them.
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if v := pr.In.Header.Values(h); len(v) > 0 {
					pr.Out.Header[h] = v
				}
			}
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			logger.Warn("origin unreachable", "service", target.String(), "err", err)
			w.Header().Set(HeaderError, "origin-unreachable")
			http.Error(w, "codigate: the origin service is unreachable", http.StatusBadGateway)
		},
	}
}
