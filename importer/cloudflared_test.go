package importer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/codigate/agent/config"
)

func parseOut(t *testing.T, data []byte) *config.Config {
	t.Helper()
	c, err := config.Parse(data, func(k string) string {
		if k == "CODIGATE_TOKEN" {
			return "tok"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("output does not parse: %v\n%s", err, data)
	}
	return c
}

func hasNote(r *Result, level, substr string) bool {
	for _, n := range r.Notes {
		if n.Level == level && strings.Contains(n.Message, substr) {
			return true
		}
	}
	return false
}

func TestCloudflaredSample(t *testing.T) {
	data, err := os.ReadFile("testdata/cloudflared.yml")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Cloudflared(data, Options{Edge: "https://connect.test", TokenFile: "/tmp/tok"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tunnel != "6ff42ae2-765d-4adf-8112-31c55c1551ef" {
		t.Errorf("tunnel = %q", res.Tunnel)
	}
	if want := []string{"grafana.example.com", "nas.example.com", "rdp.example.com", "ssh.example.com", "static.example.com"}; !reflect.DeepEqual(res.Apps, want) {
		t.Errorf("apps = %v, want %v", res.Apps, want)
	}

	c := parseOut(t, res.Config)
	if c.Edge != "https://connect.test" || c.TokenFile != "/tmp/tok" {
		t.Errorf("edge/token_file = %q %q", c.Edge, c.TokenFile)
	}
	got := map[string]config.Rule{}
	var order []string
	for _, r := range c.Ingress {
		got[r.Hostname] = r
		order = append(order, r.Hostname)
	}
	if want := []string{"grafana.example.com", "static.example.com", "nas.example.com", "*.dev.example.com", "ssh.example.com", "rdp.example.com", ""}; !reflect.DeepEqual(order, want) {
		t.Fatalf("rules = %q, want %q", order, want)
	}
	if got["rdp.example.com"].Service != "tcp://10.0.0.5:3389" || got["ssh.example.com"].Service != "ssh://localhost:22" {
		t.Errorf("tcp rules = %+v %+v", got["rdp.example.com"], got["ssh.example.com"])
	}
	if got["grafana.example.com"].Service != "http://localhost:3000" {
		t.Errorf("first grafana rule must win: %+v", got["grafana.example.com"])
	}
	if got["static.example.com"].Service != "http://localhost:8000" {
		t.Errorf("path rule must be skipped: %+v", got["static.example.com"])
	}
	nas := got["nas.example.com"]
	if !nas.OriginRequest.NoTLSVerify || nas.OriginRequest.OriginServerName != "nas.example.com" {
		t.Errorf("nas origin = %+v", nas.OriginRequest)
	}
	if got["*.dev.example.com"].OriginRequest.HTTPHostHeader != "dev.internal" {
		t.Errorf("wildcard origin = %+v", got["*.dev.example.com"].OriginRequest)
	}
	if got[""].Service != "http_status:404" {
		t.Errorf("catch-all = %+v", got[""])
	}

	for _, want := range []struct{ level, text string }{
		{Info, "credentials file"},
		{Warn, "warp-routing"},
		{Info, "unreachable"},
		{Warn, "path-based rule"},
		{Warn, "Cloudflare Access"},
		{Warn, "wildcard *.dev.example.com"},
		{Info, "ssh.example.com is a TCP service (ssh://localhost:22)"},
		{Info, "rdp.example.com is a TCP service (tcp://10.0.0.5:3389)"},
		{Warn, `service "smb://nas:445"`},
		{Warn, "bastionMode"},
		{Info, "connectTimeout"},
	} {
		if !hasNote(res, want.level, want.text) {
			t.Errorf("missing %s note containing %q; notes: %v", want.level, want.text, res.Notes)
		}
	}
	if !strings.HasPrefix(string(res.Config), "# codigate-agent configuration, imported from cloudflared (tunnel 6ff42ae2") ||
		!strings.Contains(string(res.Config), "#   warn: ingress[8]: service") {
		t.Errorf("header missing notes:\n%s", res.Config)
	}
	if res.Warnings() < 5 {
		t.Errorf("warnings = %d", res.Warnings())
	}
}

func TestCloudflaredCases(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		apps     []string
		catchAll string
		note     string
	}{
		{"adds missing catch-all", "ingress:\n  - hostname: a.example.com\n    service: http://x:1\n", []string{"a.example.com"}, "http_status:404", "no catch-all"},
		{"unsupported catch-all", "ingress:\n  - hostname: a.example.com\n    service: http://x:1\n  - service: hello_world\n", []string{"a.example.com"}, "http_status:404", "catch-all service"},
		{"http catch-all kept", "ingress:\n  - service: http://localhost:80\n", nil, "http://localhost:80", "no rule could be imported"},
		{"early hostless rule", "ingress:\n  - service: http://a:1\n  - hostname: b.example.com\n    service: http://b:1\n  - service: http_status:404\n", []string{"b.example.com"}, "http_status:404", "without hostname"},
		{"invalid hostname", "ingress:\n  - hostname: bad_host!\n    service: http://a:1\n  - service: http_status:404\n", nil, "http_status:404", "not valid"},
		{"deep wildcard", "ingress:\n  - hostname: a.*.example.com\n    service: http://a:1\n  - service: http_status:404\n", nil, "http_status:404", "not valid"},
		{"trailing dot", "ingress:\n  - hostname: a.example.com.\n    service: http://a:1\n  - service: http_status:404\n", []string{"a.example.com"}, "http_status:404", ""},
		{"unknown option", "originRequest:\n  fancy: 1\ningress:\n  - hostname: a.example.com\n    service: http://a:1\n  - service: http_status:404\n", []string{"a.example.com"}, "http_status:404", `"fancy"`},
		{"caPool", "ingress:\n  - hostname: a.example.com\n    service: https://a:1\n    originRequest:\n      caPool: /ca.pem\n  - service: http_status:404\n", []string{"a.example.com"}, "http_status:404", "caPool"},
		{"rule overrides default", "originRequest:\n  noTLSVerify: true\ningress:\n  - hostname: a.example.com\n    service: https://a:1\n    originRequest:\n      noTLSVerify: false\n  - service: http_status:404\n", []string{"a.example.com"}, "http_status:404", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Cloudflared([]byte(tc.in), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(res.Apps, tc.apps) {
				t.Errorf("apps = %v, want %v", res.Apps, tc.apps)
			}
			c := parseOut(t, res.Config)
			last := c.Ingress[len(c.Ingress)-1]
			if last.Hostname != "" || last.Service != tc.catchAll {
				t.Errorf("catch-all = %+v, want %s", last, tc.catchAll)
			}
			if c.Edge != "https://connect.codigate.nl" || c.TokenFile != "/etc/codigate/token" {
				t.Errorf("defaults = %q %q", c.Edge, c.TokenFile)
			}
			if tc.note != "" && !hasNote(res, Warn, tc.note) && !hasNote(res, Info, tc.note) {
				t.Errorf("no note containing %q: %v", tc.note, res.Notes)
			}
			if tc.name == "rule overrides default" && c.Ingress[0].OriginRequest.NoTLSVerify {
				t.Error("rule-level noTLSVerify: false must override the default")
			}
			if tc.name == "unknown option" && len(res.Notes) != 1 {
				t.Errorf("inherited option must be noted once: %v", res.Notes)
			}
			if c.Ingress[len(c.Ingress)-1].OriginRequest.NoTLSVerify {
				t.Error("http_status catch-all must not get origin options")
			}
		})
	}
}

func TestCloudflaredErrors(t *testing.T) {
	for _, in := range []string{"tunnel: x\n", "ingress: [", "url: http://localhost:8080\n"} {
		if _, err := Cloudflared([]byte(in), Options{}); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

func TestAPI(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cgk_test" || r.Header.Get("Accept") != "application/json" {
			w.WriteHeader(401)
			w.Write([]byte(`{"message":"Unauthenticated."}`))
			return
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, r.URL.Path+" "+body["name"]+" "+body["hostname"]+" "+body["agent_id"])
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/agents":
			w.WriteHeader(201)
			w.Write([]byte(`{"data":{"id":"agt_1","name":"x"},"token":"cga_secret"}`))
		case r.URL.Path == "/api/v1/apps" && body["hostname"] == "taken.example.com":
			w.WriteHeader(422)
			w.Write([]byte(`{"message":"The hostname has already been taken.","errors":{"hostname":["This hostname is already in use."]}}`))
		case r.URL.Path == "/api/v1/apps":
			w.WriteHeader(201)
			w.Write([]byte(`{"data":{"id":"app_1"}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	api := &API{BaseURL: srv.URL + "/", Token: "cgk_test"}
	ctx := context.Background()
	id, tok, err := api.CreateAgent(ctx, "home")
	if err != nil || id != "agt_1" || tok != "cga_secret" {
		t.Fatalf("CreateAgent = %q %q %v", id, tok, err)
	}
	if appID, err := api.CreateApp(ctx, "grafana", "grafana.example.com", "agt_1"); err != nil || appID != "app_1" {
		t.Fatalf("CreateApp = %q %v", appID, err)
	}
	_, err = api.CreateApp(ctx, "taken", "taken.example.com", "agt_1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 422 || apiErr.Message != "This hostname is already in use." {
		t.Fatalf("422 error = %v", err)
	}
	want := []string{"/api/v1/agents home  ", "/api/v1/apps grafana grafana.example.com agt_1", "/api/v1/apps taken taken.example.com agt_1"}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %q", calls)
	}

	bad := &API{BaseURL: srv.URL, Token: "wrong"}
	if _, _, err := bad.CreateAgent(ctx, "x"); err == nil || err.Error() != "Unauthenticated." {
		t.Errorf("401 error = %v", err)
	}
}

func TestAppName(t *testing.T) {
	if AppName("grafana.acme.com") != "grafana" || AppName("localhost") != "localhost" {
		t.Error("AppName")
	}
}
