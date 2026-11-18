// Package importer converts a cloudflared config.yml into a codigate-agent config and lists the
// apps to create. The conversion is pure; see api.go for creating the apps.
package importer

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"net"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/codigate/agent/config"
	"github.com/codigate/agent/protocol"
)

// Level of a Note.
const (
	Warn = "warn" // Behaviour changes or something was dropped; read before switching over.
	Info = "info" // Harmless difference.
)

// Note is one finding of the conversion, tied to a cloudflared rule when Rule >= 0.
type Note struct {
	Level   string
	Rule    int
	Message string
}

func (n Note) String() string {
	if n.Rule < 0 {
		return n.Level + ": " + n.Message
	}
	return fmt.Sprintf("%s: ingress[%d]: %s", n.Level, n.Rule, n.Message)
}

// Options are the values cloudflared's config doesn't have.
type Options struct {
	Edge      string // connect URL, e.g. https://connect.codigate.nl
	TokenFile string // where the agent token will be
}

// Result of a conversion.
type Result struct {
	Tunnel string   // cloudflared tunnel name or id, if any
	Config []byte   // codigate-agent YAML, with the notes as a header comment
	Apps   []string // concrete hostnames to create as apps, sorted
	Notes  []Note
}

// Warnings counts the notes at Warn level.
func (r *Result) Warnings() int {
	n := 0
	for _, note := range r.Notes {
		if note.Level == Warn {
			n++
		}
	}
	return n
}

type cfConfig struct {
	Tunnel        string         `yaml:"tunnel"`
	Credentials   string         `yaml:"credentials-file"`
	OriginRequest map[string]any `yaml:"originRequest"`
	Ingress       []cfRule       `yaml:"ingress"`
	WarpRouting   *struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"warp-routing"`
}

type cfRule struct {
	Hostname      string         `yaml:"hostname"`
	Path          string         `yaml:"path"`
	Service       string         `yaml:"service"`
	OriginRequest map[string]any `yaml:"originRequest"`
}

type outConfig struct {
	Edge      string    `yaml:"edge"`
	TokenFile string    `yaml:"token_file"`
	LogLevel  string    `yaml:"log_level"`
	Ingress   []outRule `yaml:"ingress"`
}

type outRule struct {
	Hostname      string     `yaml:"hostname,omitempty"`
	Service       string     `yaml:"service"`
	OriginRequest *outOrigin `yaml:"originRequest,omitempty"`
}

type outOrigin struct {
	NoTLSVerify      bool   `yaml:"noTLSVerify,omitempty"`
	HTTPHostHeader   string `yaml:"httpHostHeader,omitempty"`
	OriginServerName string `yaml:"originServerName,omitempty"`
}

// Origin options that only tune connections; Codigate's defaults apply instead.
var tuningOptions = map[string]bool{
	"connectTimeout": true, "tlsTimeout": true, "tcpKeepAlive": true, "noHappyEyeballs": true,
	"keepAliveConnections": true, "keepAliveTimeout": true, "disableChunkedEncoding": true, "http2Origin": true,
}

// Cloudflared converts the contents of a cloudflared config.yml.
func Cloudflared(data []byte, opts Options) (*Result, error) {
	var cf cfConfig
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("cloudflared config: %w", err)
	}
	if len(cf.Ingress) == 0 {
		return nil, errors.New("cloudflared config has no ingress rules; for a tunnel run with --token, import it with -cf")
	}
	if opts.Edge == "" {
		opts.Edge = "https://connect.codigate.nl"
	}
	if opts.TokenFile == "" {
		opts.TokenFile = "/etc/codigate/token"
	}

	res := &Result{Tunnel: cf.Tunnel}
	note := func(level string, rule int, format string, args ...any) {
		res.Notes = append(res.Notes, Note{Level: level, Rule: rule, Message: fmt.Sprintf(format, args...)})
	}
	if cf.Tunnel != "" || cf.Credentials != "" {
		note(Info, -1, "tunnel %q and its credentials file are not used: the agent authenticates with a Codigate agent token", cf.Tunnel)
	}
	if cf.WarpRouting != nil && cf.WarpRouting.Enabled {
		note(Warn, -1, "warp-routing (private network access) has no Codigate equivalent; it is not imported")
	}

	if len(cf.OriginRequest) > 0 { // report top-level defaults once, not for every rule
		convertOrigin(cf.OriginRequest, nil, "", "", func(level, format string, args ...any) { note(level, -1, "originRequest: "+format, args...) })
	}

	out := outConfig{Edge: opts.Edge, TokenFile: opts.TokenFile, LogLevel: "info"}
	seen := map[string]bool{}
	apps := map[string]bool{}
	var catchAll *outRule
	for i, r := range cf.Ingress {
		host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(r.Hostname)), ".")
		last := i == len(cf.Ingress)-1
		if host == "" && !last {
			note(Warn, i, "a rule without hostname before the last rule matches everything; it is skipped")
			continue
		}
		if r.Path != "" {
			if host == "" {
				note(Warn, i, "path %q on the catch-all rule is not supported; the rule is used for every path", r.Path)
			} else {
				note(Warn, i, "path-based rule %s %s is not supported and is skipped; requests to that path go to the next rule for %s", host, r.Path, host)
				continue
			}
		}
		if host != "" {
			bare := strings.TrimPrefix(host, "*.")
			if strings.Contains(bare, "*") || protocol.ValidateHostname(bare) != nil {
				note(Warn, i, "hostname %q is not valid for Codigate; the rule is skipped", r.Hostname)
				continue
			}
			if seen[host] {
				note(Info, i, "%s already has a rule above; this one was unreachable and is skipped", host)
				continue
			}
		}

		service, ok := convertService(r.Service)
		tcp := ok && (strings.HasPrefix(service, "tcp://") || strings.HasPrefix(service, "ssh://"))
		switch {
		case !ok && host == "":
			note(Warn, i, "catch-all service %q is not supported; it becomes http_status:404", r.Service)
			service = "http_status:404"
		case !ok:
			note(Warn, i, "service %q for %s is not supported (http, https, tcp, ssh and rdp only); the rule is skipped", r.Service, host)
			continue
		case tcp && (host == "" || strings.HasPrefix(host, "*.")):
			if host == "" {
				note(Warn, i, "catch-all service %q needs an exact hostname; it becomes http_status:404", r.Service)
				service, tcp = "http_status:404", false
			} else {
				note(Warn, i, "service %q needs an exact hostname, not %s; the rule is skipped", r.Service, host)
				continue
			}
		case tcp:
			note(Info, i, "%s is a TCP service (%s): clients connect with `codigate-agent access tcp %s` and a service token", host, service, host)
		}
		o := &origin{}
		if !tcp && !strings.HasPrefix(service, "http_status:") { // no origin to tune
			inherited := map[string]bool{}
			for k := range cf.OriginRequest {
				if _, own := r.OriginRequest[k]; !own {
					inherited[k] = true
				}
			}
			o = convertOrigin(merge(cf.OriginRequest, r.OriginRequest), inherited, host, service,
				func(level, format string, args ...any) { note(level, i, format, args...) })
		}
		if o.dropRule {
			continue
		}

		rule := outRule{Hostname: host, Service: service}
		if o.NoTLSVerify || o.HTTPHostHeader != "" || o.OriginServerName != "" {
			rule.OriginRequest = &o.outOrigin
		}
		if host == "" {
			catchAll = &rule
			continue
		}
		seen[host] = true
		out.Ingress = append(out.Ingress, rule)
		if strings.HasPrefix(host, "*.") {
			note(Warn, i, "wildcard %s: Codigate matches one label (a.%s, not a.b.%s), and each name you use needs its own app", host, host[2:], host[2:])
		} else {
			apps[host] = true
		}
	}
	if catchAll == nil {
		note(Info, -1, "no catch-all rule; added http_status:404")
		catchAll = &outRule{Service: "http_status:404"}
	}
	out.Ingress = append(out.Ingress, *catchAll)
	if len(out.Ingress) == 1 {
		note(Warn, -1, "no rule could be imported")
	}

	for h := range apps {
		res.Apps = append(res.Apps, h)
	}
	sort.Strings(res.Apps)

	body, err := yaml.Marshal(out)
	if err != nil {
		return nil, err
	}
	// Self-check: the agent must accept what we write.
	if _, err := config.Parse(body, func(k string) string {
		if k == "CODIGATE_TOKEN" {
			return "check"
		}
		return ""
	}); err != nil {
		return nil, fmt.Errorf("converted config does not validate: %w", err)
	}
	res.Config = append(header(res), body...)
	return res, nil
}

// convertService keeps what the agent supports. rdp:// becomes tcp:// (port 3389 when missing).
// Everything else (smb://, unix:, hello_world, bastion, socks5) is reported by the caller.
func convertService(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, "rdp://"); ok {
		if _, _, err := net.SplitHostPort(rest); err != nil {
			rest = net.JoinHostPort(strings.Trim(rest, "[]/"), "3389")
		}
		return "tcp://" + rest, true
	}
	for _, p := range []string{"http://", "https://", "http_status:", "tcp://", "ssh://"} {
		if strings.HasPrefix(s, p) {
			return s, true
		}
	}
	return "", false
}

type origin struct {
	outOrigin
	dropRule bool
}

// convertOrigin maps originRequest options. Notes for keys in inherited (top-level defaults, already
// reported once) are suppressed unless they depend on the rule.
func convertOrigin(m map[string]any, inherited map[string]bool, host, service string, report func(level, format string, args ...any)) *origin {
	o := &origin{}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var ignored []string
	for _, k := range keys {
		v := m[k]
		note := report
		if inherited[k] && k != "matchSNIToHost" && k != "bastionMode" && k != "proxyType" {
			note = func(string, string, ...any) {}
		}
		switch k {
		case "noTLSVerify":
			o.NoTLSVerify, _ = v.(bool)
		case "httpHostHeader":
			o.HTTPHostHeader, _ = v.(string)
		case "originServerName":
			o.OriginServerName, _ = v.(string)
		case "matchSNIToHost":
			// Keys are sorted, so originServerName isn't applied yet: check the map.
			if b, _ := v.(bool); b && m["originServerName"] == nil && strings.HasPrefix(service, "https://") {
				if host == "" || strings.HasPrefix(host, "*.") {
					note(Warn, "matchSNIToHost can't be expressed as a fixed originServerName for %s; the origin gets its own hostname as SNI", or(host, "the catch-all"))
				} else {
					o.OriginServerName = host
				}
			}
		case "caPool":
			note(Warn, "caPool %v is not supported yet; the origin certificate must be publicly trusted, or set noTLSVerify", v)
		case "access":
			note(Warn, "Cloudflare Access check at the origin: recreate the policy as a Codigate access rule; the origin can verify the X-Codigate-User token instead of Cf-Access-Jwt-Assertion")
		case "bastionMode", "proxyType":
			note(Warn, "%s (a SOCKS/bastion proxy rule) is not supported; the rule is skipped", k)
			o.dropRule = true
		case "proxyAddress", "proxyPort":
			// Only meaningful with proxyType/bastionMode, or for cloudflared's own listener.
		case "ipRules":
			note(Warn, "ipRules are not supported; the rule is imported without them")
		default:
			if tuningOptions[k] {
				if !inherited[k] {
					ignored = append(ignored, k)
				}
			} else {
				note(Warn, "unknown originRequest option %q is ignored", k)
			}
		}
	}
	if len(ignored) > 0 {
		report(Info, "connection tuning (%s) ignored; Codigate's defaults apply", strings.Join(ignored, ", "))
	}
	return o
}

func merge(defaults, rule map[string]any) map[string]any {
	m := maps.Clone(defaults)
	if m == nil {
		m = map[string]any{}
	}
	maps.Copy(m, rule)
	return m
}

func header(r *Result) []byte {
	var b bytes.Buffer
	b.WriteString("# codigate-agent configuration, imported from cloudflared")
	if r.Tunnel != "" {
		b.WriteString(" (tunnel " + r.Tunnel + ")")
	}
	b.WriteString(".\n")
	if len(r.Notes) > 0 {
		b.WriteString("# Review these notes before switching over:\n")
	}
	for _, n := range r.Notes {
		b.WriteString("#   " + strings.ReplaceAll(n.String(), "\n", " ") + "\n")
	}
	b.WriteString("\n")
	return b.Bytes()
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
