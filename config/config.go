// Package config loads the agent configuration.
//
// The ingress section deliberately mirrors cloudflared's (hostname, service, originRequest), so
// most existing cloudflared configs work after changing only the credentials (see the importer).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/codigate/agent/protocol"
)

// Config is the agent configuration file.
type Config struct {
	// Edge is the connect URL, e.g. https://connect.codigate.nl. Overridden by CODIGATE_EDGE.
	Edge string `yaml:"edge"`
	// Token is the agent token from the dashboard. Prefer CODIGATE_TOKEN or token_file.
	Token     string `yaml:"token"`
	TokenFile string `yaml:"token_file"`
	// AllowInsecureEdge permits an http:// edge URL. Only for local development and tests.
	AllowInsecureEdge bool `yaml:"allow_insecure_edge"`
	// EdgeCAFile is a PEM file with extra CAs to trust for the edge (local development, private CAs).
	EdgeCAFile string `yaml:"edge_ca_file"`
	// LogLevel is debug, info, warn or error.
	LogLevel string `yaml:"log_level"`
	Ingress  []Rule `yaml:"ingress"`
	// Docker adds ingress rules from container labels (see the docker package).
	Docker Docker `yaml:"docker"`
	// API and APIToken let the agent create apps for hostnames it finds (docker.create_apps).
	// APIToken is overridden by CODIGATE_API_TOKEN.
	API      string `yaml:"api"`
	APIToken string `yaml:"api_token"`
}

// Docker configures label discovery.
type Docker struct {
	Enabled bool `yaml:"enabled"`
	// Endpoint is the Docker API: unix:///var/run/docker.sock (default, or DOCKER_HOST) or
	// tcp://host:port (a read-only socket proxy).
	Endpoint string `yaml:"endpoint"`
	// Network picks the container address when a container is on several networks.
	Network string `yaml:"network"`
	// CreateApps creates a protected app (no access yet) for every new hostname; needs api and
	// api_token.
	CreateApps bool `yaml:"create_apps"`
}

// Rule maps a public hostname to a local service.
type Rule struct {
	// Hostname is matched exactly, or "*.example.com" for one label. Empty means catch-all, which is
	// only allowed as the last rule.
	Hostname string `yaml:"hostname"`
	// Service is http://host:port, https://host:port, tcp://host:port, ssh://host[:port] or
	// http_status:<code>.
	Service       string        `yaml:"service"`
	OriginRequest OriginRequest `yaml:"originRequest"`

	target *url.URL
	status int
	tcp    string
}

// NewRule validates a rule built in code (Docker labels).
func NewRule(hostname, service string, or OriginRequest) (Rule, error) {
	r := Rule{Hostname: hostname, Service: service, OriginRequest: or}
	if r.Hostname == "" {
		return r, errors.New("hostname is required")
	}
	return r, r.prepare()
}

// OriginRequest holds per-rule upstream options, named as in cloudflared.
type OriginRequest struct {
	NoTLSVerify      bool   `yaml:"noTLSVerify"`
	HTTPHostHeader   string `yaml:"httpHostHeader"`
	OriginServerName string `yaml:"originServerName"`
}

// Target returns the parsed upstream URL, or nil for http_status rules.
func (r *Rule) Target() *url.URL { return r.target }

// Status returns the fixed status code for http_status rules, or 0.
func (r *Rule) Status() int { return r.status }

// TCPAddr returns host:port for tcp:// and ssh:// rules, or "".
func (r *Rule) TCPAddr() string { return r.tcp }

// Load reads, applies environment overrides and validates a config file.
// Without a file, CODIGATE_DOCKER=true plus the environment (edge, token) is a complete config.
// Errors name the file and say what to do: the agent often runs unattended in Docker, where the
// log line is all the user sees before the restart loop.
func Load(path string) (*Config, error) {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not a file. Docker creates an empty directory when the file you mount "+
			"does not exist on the host. Create codigate.yml next to compose.yaml (remove the empty codigate.yml directory first), "+
			"then run docker compose up -d --force-recreate", path)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && envBool(os.Getenv("CODIGATE_DOCKER")) {
		data, err = []byte("{}"), nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("config file %s not found. Create it (see codigate.example.yml), or point CODIGATE_CONFIG "+
			"(or -config) at it. In Docker, mount it: ./codigate.yml:/etc/codigate/codigate.yml:ro", path)
	}
	if err != nil {
		return nil, fmt.Errorf("config file %s can't be read: %w", path, err)
	}
	c, err := Parse(data, os.Getenv)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func envBool(v string) bool {
	b, err := strconv.ParseBool(v)
	return err == nil && b
}

// Parse is Load without the file system; getenv is injectable for tests.
func Parse(data []byte, getenv func(string) string) (*Config, error) {
	var c Config
	if strings.TrimSpace(string(data)) == "" {
		if !envBool(getenv("CODIGATE_DOCKER")) {
			return nil, errors.New("the config file is empty. Add edge and your ingress rules (see codigate.example.yml), " +
				"or set CODIGATE_DOCKER=true to take the rules from container labels")
		}
		data = []byte("{}")
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, yamlError(err)
	}
	if v := getenv("CODIGATE_EDGE"); v != "" {
		c.Edge = v
	}
	if v := getenv("CODIGATE_TOKEN"); v != "" {
		c.Token = v
	}
	if v := getenv("CODIGATE_API"); v != "" {
		c.API = v
	}
	if v := getenv("CODIGATE_API_TOKEN"); v != "" {
		c.APIToken = v
	}
	if envBool(getenv("CODIGATE_DOCKER")) {
		c.Docker.Enabled = true
	}
	if envBool(getenv("CODIGATE_CREATE_APPS")) {
		c.Docker.CreateApps = true
	}
	if c.Docker.Enabled && c.Docker.Endpoint == "" {
		c.Docker.Endpoint = getenv("DOCKER_HOST")
		if c.Docker.Endpoint == "" {
			c.Docker.Endpoint = "unix:///var/run/docker.sock"
		}
	}
	if c.Token == "" && c.TokenFile != "" {
		b, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("token_file %s can't be read (%w). Save the agent token from the dashboard in that file, "+
				"or set CODIGATE_TOKEN instead", c.TokenFile, errors.Unwrap(err))
		}
		c.Token = strings.TrimSpace(string(b))
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	return &c, c.validate()
}

func (c *Config) validate() error {
	var errs []error
	u, err := url.Parse(c.Edge)
	switch {
	case c.Edge == "":
		errs = append(errs, errors.New("edge is required"))
	case err != nil || u.Host == "":
		errs = append(errs, fmt.Errorf("edge %q is not a URL", c.Edge))
	case u.Scheme == "http" && !c.AllowInsecureEdge:
		errs = append(errs, errors.New("edge must use https (set allow_insecure_edge only for local development)"))
	case u.Scheme != "https" && u.Scheme != "http":
		errs = append(errs, fmt.Errorf("edge scheme %q not supported", u.Scheme))
	}
	if c.Token == "" {
		errs = append(errs, errors.New("no agent token. Set CODIGATE_TOKEN, or token_file: /path/to/token in the config. "+
			"The token is shown when you create the agent in the dashboard (Agents, New token for a new one)"))
	}
	if len(c.Ingress) == 0 && !c.Docker.Enabled {
		errs = append(errs, errors.New("no ingress rules. Add at least one under ingress:, e.g. "+
			"- hostname: app.example.com, service: http://localhost:8080, and end with a catch-all - service: http_status:404. "+
			"Or set CODIGATE_DOCKER=true to take the rules from container labels"))
	}
	if c.Docker.Enabled {
		if u, err := url.Parse(c.Docker.Endpoint); err != nil || (u.Scheme != "unix" && u.Scheme != "tcp") {
			errs = append(errs, fmt.Errorf("docker.endpoint %q: use unix:///path or tcp://host:port", c.Docker.Endpoint))
		}
		if c.Docker.CreateApps && (c.API == "" || c.APIToken == "") {
			errs = append(errs, errors.New("docker.create_apps needs api and api_token (or CODIGATE_API_TOKEN)"))
		}
	}
	seen := map[string]bool{}
	for i := range c.Ingress {
		r := &c.Ingress[i]
		if err := r.prepare(); err != nil {
			errs = append(errs, fmt.Errorf("ingress[%d]: %w", i, err))
			continue
		}
		if r.Hostname == "" && i != len(c.Ingress)-1 {
			errs = append(errs, fmt.Errorf("ingress[%d]: a catch-all rule (no hostname) must be the last rule; the rules after it are never used", i))
		}
		if seen[r.Hostname] {
			errs = append(errs, fmt.Errorf("ingress[%d]: duplicate hostname %q", i, r.Hostname))
		}
		seen[r.Hostname] = true
	}
	return errors.Join(errs...)
}

// yamlError turns a YAML error into one line that keeps the line number.
func yamlError(err error) error {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		return fmt.Errorf("unknown or wrong setting: %s. Compare with codigate.example.yml", strings.Join(te.Errors, "; "))
	}
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	return fmt.Errorf("not valid YAML: %s. Check the indentation (spaces, not tabs) and quotes around that line", msg)
}

func (r *Rule) prepare() error {
	r.Hostname = strings.ToLower(strings.TrimSpace(r.Hostname))
	if r.Hostname != "" {
		check := strings.TrimPrefix(r.Hostname, "*.")
		if err := protocol.ValidateHostname(check); err != nil {
			return err
		}
	}
	switch {
	case strings.HasPrefix(r.Service, "http_status:"):
		code, err := strconv.Atoi(strings.TrimPrefix(r.Service, "http_status:"))
		if err != nil || code < 100 || code > 599 {
			return fmt.Errorf("invalid service %q", r.Service)
		}
		r.status = code
	case strings.HasPrefix(r.Service, "http://"), strings.HasPrefix(r.Service, "https://"):
		u, err := url.Parse(r.Service)
		if err != nil || u.Host == "" {
			return fmt.Errorf("invalid service %q", r.Service)
		}
		r.target = u
	case strings.HasPrefix(r.Service, "tcp://"), strings.HasPrefix(r.Service, "ssh://"):
		u, err := url.Parse(r.Service)
		if err != nil || u.Hostname() == "" || (u.Path != "" && u.Path != "/") {
			return fmt.Errorf("invalid service %q", r.Service)
		}
		port := u.Port()
		switch {
		case port == "" && u.Scheme == "ssh":
			port = "22"
		case port == "":
			return fmt.Errorf("service %q needs a port", r.Service)
		}
		if r.Hostname == "" || strings.HasPrefix(r.Hostname, "*.") {
			return fmt.Errorf("service %q needs an exact hostname", r.Service)
		}
		r.tcp = net.JoinHostPort(u.Hostname(), port)
	default:
		return fmt.Errorf("service %q is not supported: use http://, https://, tcp://, ssh:// or http_status:", r.Service)
	}
	return nil
}

// Match returns the rule for a hostname: exact match first, then a one-label wildcard, then the
// catch-all. It returns nil when nothing matches.
func (c *Config) Match(host string) *Rule {
	var wildcard, catchAll *Rule
	for i := range c.Ingress {
		r := &c.Ingress[i]
		switch {
		case r.Hostname == host:
			return r
		case r.Hostname == "":
			catchAll = r
		case wildcard == nil && strings.HasPrefix(r.Hostname, "*."):
			if dot := strings.IndexByte(host, '.'); dot > 0 && host[dot:] == r.Hostname[1:] {
				wildcard = r
			}
		}
	}
	if wildcard != nil {
		return wildcard
	}
	return catchAll
}
