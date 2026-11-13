package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const sample = `tunnel: home
ingress:
  - hostname: grafana.example.com
    service: http://localhost:3000
  - hostname: taken.example.com
    service: http://localhost:3001
  - service: http_status:404
`

func fakeAPI(t *testing.T) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls = append(calls, r.URL.Path+" "+body["name"]+" "+body["agent_id"])
		mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/agents":
			w.WriteHeader(201)
			w.Write([]byte(`{"data":{"id":"agt_new"},"token":"cga_secret"}`))
		case body["hostname"] == "taken.example.com":
			w.WriteHeader(422)
			w.Write([]byte(`{"message":"x","errors":{"hostname":["This hostname is already in use."]}}`))
		default:
			w.WriteHeader(201)
			w.Write([]byte(`{"data":{"id":"app_1"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runImport(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestImportWritesConfig(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "config.yml")
	os.WriteFile(src, []byte(sample), 0o644)
	dst := filepath.Join(dir, "codigate.yml")

	code, _, stderr := run(t, "cloudflared", "-out", dst, "-edge", "https://connect.test", src)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	data, _ := os.ReadFile(dst)
	if !strings.Contains(string(data), "edge: https://connect.test") || !strings.Contains(string(data), "grafana.example.com") {
		t.Errorf("config:\n%s", data)
	}
	if !strings.Contains(stderr, "rerun with -api") {
		t.Errorf("stderr: %s", stderr)
	}

	if code, _, stderr := run(t, "cloudflared", "-out", dst, src); code != 1 || !strings.Contains(stderr, "exists") {
		t.Errorf("overwrite: exit %d: %s", code, stderr)
	}
	if code, _, _ := run(t, "cloudflared", "-out", dst, "-force", src); code != 0 {
		t.Errorf("-force: exit %d", code)
	}
	if code, stdout, _ := run(t, "cloudflared", "-out", "-", src); code != 0 || !strings.Contains(stdout, "ingress:") {
		t.Errorf("stdout: exit %d: %s", code, stdout)
	}
}

func TestImportUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"ngrok"}, {"cloudflared", "-nope"}, {"cloudflared", "a", "b"}, {"cloudflared", "/does/not/exist"}} {
		if code, _, _ := run(t, args...); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
}

func TestImportCreatesAgentAndApps(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "config.yml")
	os.WriteFile(src, []byte(sample), 0o644)
	tokenPath := filepath.Join(dir, "token")
	srv, calls := fakeAPI(t)

	t.Setenv("CODIGATE_API_TOKEN", "")
	if code, _, stderr := run(t, "cloudflared", "-out", "-", "-api", srv.URL, src); code != 2 || !strings.Contains(stderr, "CODIGATE_API_TOKEN") {
		t.Fatalf("no token: exit %d: %s", code, stderr)
	}
	t.Setenv("CODIGATE_API_TOKEN", "cgk_test")

	code, _, stderr := run(t, "cloudflared", "-out", "-", "-api", srv.URL, "-write-token", tokenPath, "-dry-run", src)
	if code != 0 || !strings.Contains(stderr, `would create agent "home"`) || len(*calls) != 0 {
		t.Fatalf("dry run: exit %d calls %v: %s", code, *calls, stderr)
	}

	code, _, stderr = run(t, "cloudflared", "-out", "-", "-api", srv.URL, "-write-token", tokenPath, src)
	if code != 1 { // taken.example.com fails
		t.Errorf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "created app grafana.example.com") || !strings.Contains(stderr, "app taken.example.com: This hostname is already in use.") {
		t.Errorf("stderr: %s", stderr)
	}
	tok, err := os.ReadFile(tokenPath)
	if err != nil || string(tok) != "cga_secret\n" {
		t.Errorf("token file = %q %v", tok, err)
	}
	if fi, _ := os.Stat(tokenPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("token mode = %v", fi.Mode().Perm())
	}
	want := "/api/v1/agents home ,/api/v1/apps grafana agt_new,/api/v1/apps taken agt_new"
	if got := strings.Join(*calls, ","); got != want {
		t.Errorf("calls = %s", got)
	}

	// Existing token file: refuse before creating a second agent.
	*calls = nil
	if code, _, stderr := run(t, "cloudflared", "-out", "-", "-api", srv.URL, "-write-token", tokenPath, src); code != 1 || len(*calls) != 0 {
		t.Errorf("existing token: exit %d calls %v: %s", code, *calls, stderr)
	}
	// Existing agent: no agent call, no token written.
	if code, _, _ := run(t, "cloudflared", "-out", "-", "-api", srv.URL, "-agent", "agt_old", src); code != 1 || strings.Contains(strings.Join(*calls, ","), "agents") || !strings.Contains((*calls)[0], "agt_old") {
		t.Errorf("existing agent: exit %d calls %v", code, *calls)
	}
}
