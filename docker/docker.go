// Package docker finds ingress rules in container labels.
//
//	labels:
//	  codigate.hostname: grafana.acme.codigate.app   # required; several separated by commas
//	  codigate.port: "3000"                         # default: the container's lowest exposed TCP port
//	  codigate.scheme: http                         # http (default), https, tcp or ssh
//	  codigate.network: frontend                    # which network's address to use
//	  codigate.no_tls_verify: "true"                # https only
//	  codigate.host_header: grafana.internal        # http/https only
//
// Only running containers count. The agent needs read access to the Docker API; a socket proxy
// that allows only GET /containers and /events is enough.
package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codigate/agent/config"
)

// Label names.
const (
	LabelHostname    = "codigate.hostname"
	LabelPort        = "codigate.port"
	LabelScheme      = "codigate.scheme"
	LabelNetwork     = "codigate.network"
	LabelNoTLSVerify = "codigate.no_tls_verify"
	LabelHostHeader  = "codigate.host_header"
)

// Container is the part of the Docker API's container list we use.
type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
	Ports  []struct {
		PrivatePort int    `json:"PrivatePort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// Name is the container name without the leading slash.
func (c *Container) Name() string {
	if len(c.Names) == 0 {
		return c.ID[:min(12, len(c.ID))]
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// Client talks to the Docker Engine API.
type Client struct {
	http *http.Client
	base string
}

// NewClient connects to endpoint: unix:///var/run/docker.sock or tcp://host:port.
func NewClient(endpoint string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second}
	switch u.Scheme {
	case "unix":
		path := u.Path
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}
		return &Client{http: &http.Client{Transport: tr}, base: "http://docker"}, nil
	case "tcp":
		return &Client{http: &http.Client{Transport: tr}, base: "http://" + u.Host}, nil
	}
	return nil, fmt.Errorf("docker: unsupported endpoint %q", endpoint)
}

// Containers lists running containers that have a codigate.hostname label.
func (c *Client) Containers(ctx context.Context) ([]Container, error) {
	f, _ := json.Marshal(map[string][]string{"label": {LabelHostname}, "status": {"running"}})
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/containers/json?filters="+url.QueryEscape(string(f)), nil)
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker: list containers: %s", res.Status)
	}
	var out []Container
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	return out, nil
}

// events calls changed for every container start, stop or rename until the stream ends.
func (c *Client) events(ctx context.Context, changed func()) error {
	f, _ := json.Marshal(map[string][]string{"type": {"container"}, "event": {"start", "die", "stop", "destroy", "rename", "unpause", "pause"}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/events?filters="+url.QueryEscape(string(f)), nil)
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("docker: events: %s", res.Status)
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		changed()
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("docker: event stream ended")
}

// Rules turns labelled containers into ingress rules. Problems are reported per container and
// that container is skipped; a hostname claimed by two containers goes to neither.
func Rules(containers []Container, network string) ([]config.Rule, []error) {
	var rules []config.Rule
	var errs []error
	owner := map[string]string{}
	conflict := map[string]bool{}
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name() < containers[j].Name() })
	for i := range containers {
		c := &containers[i]
		rs, err := rulesFor(c, network)
		if err != nil {
			errs = append(errs, fmt.Errorf("container %s: %w", c.Name(), err))
			continue
		}
		for _, r := range rs {
			if o, ok := owner[r.Hostname]; ok {
				conflict[r.Hostname] = true
				errs = append(errs, fmt.Errorf("hostname %s is on containers %s and %s; ignored", r.Hostname, o, c.Name()))
				continue
			}
			owner[r.Hostname] = c.Name()
			rules = append(rules, r)
		}
	}
	rules = slices.DeleteFunc(rules, func(r config.Rule) bool { return conflict[r.Hostname] })
	return rules, errs
}

func rulesFor(c *Container, network string) ([]config.Rule, error) {
	l := c.Labels
	if v := l[LabelNetwork]; v != "" {
		network = v
	}
	ip, err := address(c, network)
	if err != nil {
		return nil, err
	}
	port := l[LabelPort]
	if port == "" {
		for _, p := range c.Ports {
			if p.Type == "tcp" && (port == "" || p.PrivatePort < atoi(port)) {
				port = strconv.Itoa(p.PrivatePort)
			}
		}
	}
	if n := atoi(port); n < 1 || n > 65535 {
		return nil, fmt.Errorf("set %s (no exposed TCP port found)", LabelPort)
	}
	scheme := strings.ToLower(l[LabelScheme])
	if scheme == "" {
		scheme = "http"
	}
	switch scheme {
	case "http", "https", "tcp", "ssh":
	default:
		return nil, fmt.Errorf("%s %q: use http, https, tcp or ssh", LabelScheme, scheme)
	}
	service := scheme + "://" + net.JoinHostPort(ip, port)
	or := config.OriginRequest{HTTPHostHeader: l[LabelHostHeader]}
	or.NoTLSVerify, _ = strconv.ParseBool(l[LabelNoTLSVerify])
	var rules []config.Rule
	for h := range strings.SplitSeq(l[LabelHostname], ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if strings.Contains(h, "*") {
			return nil, fmt.Errorf("%s %q: wildcards are only allowed in the config file", LabelHostname, h)
		}
		r, err := config.NewRule(h, service, or)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", h, err)
		}
		rules = append(rules, r)
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("%s is empty", LabelHostname)
	}
	return rules, nil
}

func address(c *Container, network string) (string, error) {
	nets := c.NetworkSettings.Networks
	if network != "" {
		if n, ok := nets[network]; ok && n.IPAddress != "" {
			return n.IPAddress, nil
		}
		return "", fmt.Errorf("not on network %q", network)
	}
	names := make([]string, 0, len(nets))
	for name, n := range nets {
		if n.IPAddress != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "", errors.New("no IP address (network_mode host or none?); use a rule in the config file")
	}
	sort.Strings(names)
	if len(names) > 1 {
		return "", fmt.Errorf("on several networks (%s): set %s", strings.Join(names, ", "), LabelNetwork)
	}
	return nets[names[0]].IPAddress, nil
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// Watch keeps apply up to date with the labelled containers: at start, after every container
// event (debounced) and every minute. It returns when ctx ends.
func Watch(ctx context.Context, c *Client, network string, logger *slog.Logger, apply func([]config.Rule)) {
	trigger := make(chan struct{}, 1)
	poke := func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}
	go func() {
		delay := time.Second
		for ctx.Err() == nil {
			start := time.Now()
			err := c.events(ctx, poke)
			if ctx.Err() != nil {
				return
			}
			if time.Since(start) > time.Minute {
				delay = time.Second
			}
			logger.Warn("docker events interrupted; retrying", "err", err, "in", delay)
			poke()
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			delay = min(delay*2, 30*time.Second)
		}
	}()
	var last string
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		list, err := c.Containers(ctx)
		if err != nil {
			logger.Warn("docker discovery failed", "err", err)
		} else {
			rules, problems := Rules(list, network)
			sig := signature(rules, problems)
			if sig != last {
				last = sig
				for _, p := range problems {
					logger.Warn("docker label ignored", "err", p)
				}
				for _, r := range rules {
					logger.Info("docker rule", "hostname", r.Hostname, "service", r.Service)
				}
				apply(rules)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-trigger:
			// Let a burst of events (compose up) settle.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

func signature(rules []config.Rule, problems []error) string {
	var b strings.Builder
	for _, r := range rules {
		fmt.Fprintf(&b, "%s=%s|%v|%s;", r.Hostname, r.Service, r.OriginRequest.NoTLSVerify, r.OriginRequest.HTTPHostHeader)
	}
	for _, p := range problems {
		b.WriteString(p.Error())
	}
	return b.String()
}
