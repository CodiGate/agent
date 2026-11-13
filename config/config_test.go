package config

import (
	"os"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

const valid = `
edge: https://connect.codigate.nl
token: secret
ingress:
  - hostname: App.Example.com
    service: http://localhost:8080
  - hostname: "*.lab.example.com"
    service: https://10.0.0.5:8443
    originRequest:
      noTLSVerify: true
  - service: http_status:404
`

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(valid), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Match("app.example.com"); got == nil || got.Target().Host != "localhost:8080" {
		t.Fatalf("exact match (case-folded): %+v", got)
	}
	if got := c.Match("nas.lab.example.com"); got == nil || !got.OriginRequest.NoTLSVerify {
		t.Fatalf("wildcard match: %+v", got)
	}
	if got := c.Match("a.b.lab.example.com"); got == nil || got.Status() != 404 {
		t.Fatalf("wildcard must cover one label only, want catch-all: %+v", got)
	}
	if got := c.Match("other.com"); got == nil || got.Status() != 404 {
		t.Fatalf("catch-all: %+v", got)
	}
}

func TestEnvOverrides(t *testing.T) {
	c, err := Parse([]byte(valid), env(map[string]string{"CODIGATE_TOKEN": "fromenv", "CODIGATE_EDGE": "https://x.eu"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "fromenv" || c.Edge != "https://x.eu" {
		t.Fatalf("overrides not applied: %+v", c)
	}
}

func TestNoMatchWithoutCatchAll(t *testing.T) {
	c, err := Parse([]byte("edge: https://e.eu\ntoken: t\ningress:\n  - hostname: a.com\n    service: http://x:1\n"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Match("b.com") != nil {
		t.Fatal("unexpected match")
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"insecure edge":                  "edge: http://e.eu\ntoken: t\ningress:\n  - service: http_status:404\n",
		"missing token":                  "edge: https://e.eu\ningress:\n  - service: http_status:404\n",
		"no ingress":                     "edge: https://e.eu\ntoken: t\n",
		"tcp without port":               "edge: https://e.eu\ntoken: t\ningress:\n  - hostname: a.com\n    service: tcp://localhost\n",
		"tcp catch-all":                  "edge: https://e.eu\ntoken: t\ningress:\n  - service: tcp://localhost:22\n",
		"tcp wildcard":                   "edge: https://e.eu\ntoken: t\ningress:\n  - hostname: '*.a.com'\n    service: tcp://localhost:22\n",
		"unix service":                   "edge: https://e.eu\ntoken: t\ningress:\n  - hostname: a.com\n    service: unix:/run/x.sock\n",
		"docker bad endpoint":            "edge: https://e.eu\ntoken: t\ndocker:\n  enabled: true\n  endpoint: http://x\n",
		"docker create apps without api": "edge: https://e.eu\ntoken: t\ndocker:\n  enabled: true\n  create_apps: true\n",
		"catch-all not last":             "edge: https://e.eu\ntoken: t\ningress:\n  - service: http_status:404\n  - hostname: a.com\n    service: http://x:1\n",
		"duplicate":                      "edge: https://e.eu\ntoken: t\ningress:\n  - hostname: a.com\n    service: http://x:1\n  - hostname: A.com\n    service: http://x:2\n",
		"unknown field":                  "edge: https://e.eu\ntoken: t\ntunnel: abc\ningress:\n  - service: http_status:404\n",
		"bad hostname":                   "edge: https://e.eu\ntoken: t\ningress:\n  - hostname: a_b.com\n    service: http://x:1\n",
		"bad status":                     "edge: https://e.eu\ntoken: t\ningress:\n  - service: http_status:99\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(in), env(nil)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestInsecureEdgeAllowedWhenExplicit(t *testing.T) {
	in := "edge: http://127.0.0.1:8080\nallow_insecure_edge: true\ntoken: t\ningress:\n  - service: http_status:404\n"
	if _, err := Parse([]byte(in), env(nil)); err != nil {
		t.Fatal(err)
	}
}

func TestTCPServices(t *testing.T) {
	c, err := Parse([]byte("edge: https://e.eu\ntoken: t\ningress:\n  - hostname: ssh.a.com\n    service: ssh://nas\n  - hostname: db.a.com\n    service: tcp://10.0.0.5:5432\n"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Match("ssh.a.com").TCPAddr(); got != "nas:22" {
		t.Fatalf("ssh: %q", got)
	}
	if got := c.Match("db.a.com").TCPAddr(); got != "10.0.0.5:5432" {
		t.Fatalf("tcp: %q", got)
	}
}

func TestDockerOnlyConfig(t *testing.T) {
	c, err := Parse([]byte("edge: https://e.eu\ntoken: t\napi: https://app.e.eu\ndocker:\n  enabled: true\n  create_apps: true\n"),
		env(map[string]string{"CODIGATE_API_TOKEN": "cgk_x", "DOCKER_HOST": "tcp://proxy:2375"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Docker.Endpoint != "tcp://proxy:2375" || c.APIToken != "cgk_x" {
		t.Fatalf("%+v", c)
	}
}

func TestErrorListsServices(t *testing.T) {
	_, err := Parse([]byte("edge: https://e.eu\ntoken: t\ningress:\n  - hostname: a.com\n    service: rdp://pc:3389\n"), env(nil))
	if err == nil || !strings.Contains(err.Error(), "tcp://") {
		t.Fatalf("want a clear unsupported-service error, got %v", err)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	data, err := os.ReadFile("../codigate.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"CODIGATE_TOKEN": "example"}
	cfg, err := Parse(data, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if r := cfg.Match("x.dev.example.nl"); r == nil || r.OriginRequest.HTTPHostHeader != "dev.internal" {
		t.Fatalf("wildcard rule not matched: %+v", r)
	}
}

func TestDockerFromEnv(t *testing.T) {
	env := map[string]string{"CODIGATE_DOCKER": "true", "CODIGATE_EDGE": "https://edge.test", "CODIGATE_TOKEN": "t", "CODIGATE_API": "https://cp.test"}
	c, err := Parse([]byte("{}"), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if !c.Docker.Enabled || c.API != "https://cp.test" || c.Docker.Endpoint == "" {
		t.Errorf("config = %+v", c)
	}
}
