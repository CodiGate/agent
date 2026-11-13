package docker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codigate/agent/config"
)

func container(name string, labels map[string]string, nets map[string]string, ports ...int) Container {
	var c Container
	c.ID = name + "0123456789abcdef"
	c.Names = []string{"/" + name}
	c.State = "running"
	c.Labels = labels
	c.NetworkSettings.Networks = map[string]struct {
		IPAddress string `json:"IPAddress"`
	}{}
	for n, ip := range nets {
		c.NetworkSettings.Networks[n] = struct {
			IPAddress string `json:"IPAddress"`
		}{ip}
	}
	for _, p := range ports {
		c.Ports = append(c.Ports, struct {
			PrivatePort int    `json:"PrivatePort"`
			Type        string `json:"Type"`
		}{p, "tcp"})
	}
	return c
}

func TestRules(t *testing.T) {
	list := []Container{
		container("grafana", map[string]string{LabelHostname: "Grafana.acme.eu, g2.acme.eu"}, map[string]string{"web": "172.18.0.3"}, 9000, 3000),
		container("nas", map[string]string{LabelHostname: "nas.acme.eu", LabelScheme: "https", LabelPort: "5001", LabelNoTLSVerify: "true", LabelNetwork: "lan"},
			map[string]string{"lan": "172.20.0.2", "web": "172.18.0.9"}),
		container("ssh", map[string]string{LabelHostname: "ssh.acme.eu", LabelScheme: "ssh", LabelPort: "22"}, map[string]string{"web": "172.18.0.4"}),
		container("noport", map[string]string{LabelHostname: "x.acme.eu"}, map[string]string{"web": "172.18.0.5"}),
		container("twonets", map[string]string{LabelHostname: "y.acme.eu"}, map[string]string{"a": "10.0.0.1", "b": "10.0.1.1"}, 80),
		container("dup1", map[string]string{LabelHostname: "dup.acme.eu"}, map[string]string{"web": "172.18.0.6"}, 80),
		container("dup2", map[string]string{LabelHostname: "dup.acme.eu"}, map[string]string{"web": "172.18.0.7"}, 80),
		container("wild", map[string]string{LabelHostname: "*.acme.eu"}, map[string]string{"web": "172.18.0.8"}, 80),
	}
	rules, errs := Rules(list, "")
	got := map[string]string{}
	for _, r := range rules {
		got[r.Hostname] = r.Service
	}
	want := map[string]string{
		"grafana.acme.eu": "http://172.18.0.3:3000",
		"g2.acme.eu":      "http://172.18.0.3:3000",
		"nas.acme.eu":     "https://172.20.0.2:5001",
		"ssh.acme.eu":     "ssh://172.18.0.4:22",
	}
	if len(got) != len(want) {
		t.Fatalf("rules %v", got)
	}
	for h, s := range want {
		if got[h] != s {
			t.Errorf("%s: got %q want %q", h, got[h], s)
		}
	}
	for _, r := range rules {
		if r.Hostname == "nas.acme.eu" && !r.OriginRequest.NoTLSVerify {
			t.Error("no_tls_verify lost")
		}
		if r.Hostname == "ssh.acme.eu" && r.TCPAddr() != "172.18.0.4:22" {
			t.Errorf("tcp addr %q", r.TCPAddr())
		}
	}
	all := ""
	for _, e := range errs {
		all += e.Error() + "\n"
	}
	for _, s := range []string{"noport: set codigate.port", "twonets: on several networks", "dup.acme.eu is on containers dup1 and dup2", "wild: codigate.hostname \"*.acme.eu\""} {
		if !strings.Contains(all, s) {
			t.Errorf("missing problem %q in\n%s", s, all)
		}
	}
}

func TestWatchFollowsEvents(t *testing.T) {
	var mu sync.Mutex
	list := []Container{container("a", map[string]string{LabelHostname: "a.acme.eu"}, map[string]string{"web": "172.18.0.2"}, 80)}
	events := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/containers/json":
			if !strings.Contains(r.URL.Query().Get("filters"), LabelHostname) {
				t.Errorf("filters %q", r.URL.Query().Get("filters"))
			}
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(list)
		case "/events":
			w.(http.Flusher).Flush()
			for {
				select {
				case e := <-events:
					_, _ = io.WriteString(w, e+"\n")
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := NewClient("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan []config.Rule, 4)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go Watch(ctx, c, "", slog.New(slog.DiscardHandler), func(r []config.Rule) { got <- r })

	if r := <-got; len(r) != 1 || r[0].Hostname != "a.acme.eu" {
		t.Fatalf("first %v", r)
	}
	mu.Lock()
	list = append(list, container("b", map[string]string{LabelHostname: "b.acme.eu"}, map[string]string{"web": "172.18.0.3"}, 80))
	mu.Unlock()
	events <- `{"Type":"container","Action":"start"}`
	select {
	case r := <-got:
		if len(r) != 2 {
			t.Fatalf("after event %v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no update after the event")
	}
}
