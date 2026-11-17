package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
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

// A remotely managed tunnel (cloudflared --token) imports from the Cloudflare API (P2-16).
func TestImportFromCloudflareAPI(t *testing.T) {
	const account, tunnel = "0123456789abcdef0123456789abcdef", "c1744f8b-faa1-48a4-9e5c-02ac921467fa"
	var paths []string
	cf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Header.Get("Authorization")+" "+r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/configurations") {
			w.Write([]byte(`{"success":true,"result":{"config":{"ingress":[{"hostname":"ha.example.com","service":"http://homeassistant:8123"},{"service":"http_status:404"}]}}}`))
			return
		}
		w.Write([]byte(`{"success":true,"result":{"name":"docker-home"}}`))
	}))
	t.Cleanup(cf.Close)
	token := base64.StdEncoding.EncodeToString([]byte(`{"a":"` + account + `","t":"` + tunnel + `","s":"dHVubmVsLXNlY3JldA=="}`))
	t.Setenv("CLOUDFLARE_API_TOKEN", "cf-api")
	t.Setenv("TUNNEL_TOKEN", token)

	code, stdout, stderr := run(t, "cloudflared", "-cf", "-cf-api", cf.URL, "-out", "-")
	if code != 0 || !strings.Contains(stdout, "hostname: ha.example.com") || !strings.Contains(stdout, `docker-home`) {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	want := "Bearer cf-api /accounts/" + account + "/cfd_tunnel/" + tunnel
	if len(paths) != 2 || paths[0] != want || paths[1] != want+"/configurations" {
		t.Errorf("requests %q", paths)
	}

	// A tunnel id needs the account; a file argument and -cf don't mix; no API token, no request.
	if code, _, stderr := run(t, "cloudflared", "-cf", "-cf-api", cf.URL, "-cf-tunnel", tunnel, "-out", "-"); code != 1 || !strings.Contains(stderr, "-cf-account") {
		t.Errorf("id without account: %d %s", code, stderr)
	}
	if code, _, _ := run(t, "cloudflared", "-cf", "-cf-api", cf.URL, "-cf-tunnel", tunnel, "-cf-account", account, "-out", "-"); code != 0 {
		t.Errorf("id with account: %d", code)
	}
	if code, _, _ := run(t, "cloudflared", "-cf", "config.yml"); code != 2 {
		t.Errorf("-cf with a file: %d", code)
	}
	t.Setenv("CLOUDFLARE_API_TOKEN", "")
	if code, _, stderr := run(t, "cloudflared", "-cf", "-cf-api", cf.URL, "-out", "-"); code != 1 || !strings.Contains(stderr, "CLOUDFLARE_API_TOKEN") {
		t.Errorf("no API token: %d %s", code, stderr)
	}
}

// -cf-access maps Cloudflare Access onto the new apps and, with -idp, sets it (P2-03).
func TestImportAccessFromCloudflare(t *testing.T) {
	const account = "0123456789abcdef0123456789abcdef"
	cf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts/"+account+"/access/apps" || r.Header.Get("Authorization") != "Bearer cf-api" {
			w.WriteHeader(404)
			w.Write([]byte(`{"success":false,"errors":[{"code":1,"message":"nope"}]}`))
			return
		}
		w.Write([]byte(`{"success":true,"result":[
			{"name":"Grafana","type":"self_hosted","domain":"grafana.example.com","policies":[{"name":"staff","decision":"allow","include":[{"email_domain":{"domain":"example.com"}}]}]},
			{"name":"Taken","type":"self_hosted","domain":"taken.example.com","policies":[{"name":"staff","decision":"allow","include":[{"everyone":{}}]}]}]}`))
	}))
	t.Cleanup(cf.Close)
	var mu sync.Mutex
	var puts []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPut:
			mu.Lock()
			puts = append(puts, r.URL.Path+" "+string(body))
			mu.Unlock()
			w.Write([]byte(`{"data":{}}`))
		case r.URL.Path == "/api/v1/agents":
			w.WriteHeader(201)
			w.Write([]byte(`{"data":{"id":"agt_new"},"token":"cga_secret"}`))
		case strings.Contains(string(body), "taken.example.com"):
			w.WriteHeader(422)
			w.Write([]byte(`{"message":"x","errors":{"hostname":["This hostname is already in use."]}}`))
		default:
			w.WriteHeader(201)
			w.Write([]byte(`{"data":{"id":"app_grafana"}}`))
		}
	}))
	t.Cleanup(api.Close)
	dir := t.TempDir()
	src := filepath.Join(dir, "config.yml")
	os.WriteFile(src, []byte(sample), 0o644)
	t.Setenv("CLOUDFLARE_API_TOKEN", "cf-api")
	t.Setenv("CODIGATE_API_TOKEN", "cgk_x")
	t.Setenv("TUNNEL_TOKEN", "")

	// Without -api: only shown.
	code, _, stderr := run(t, "cloudflared", "-cf-access", "-cf-api", cf.URL, "-cf-account", account, "-out", "-", src)
	if code != 0 || !strings.Contains(stderr, `access grafana.example.com (from "Grafana"): domains example.com`) {
		t.Fatalf("exit %d: %s", code, stderr)
	}

	code, _, stderr = run(t, "cloudflared", "-cf-access", "-cf-api", cf.URL, "-cf-account", account, "-api", api.URL, "-idp", "idp_kc",
		"-write-token", filepath.Join(dir, "tok"), "-out", filepath.Join(dir, "codigate.yml"), src)
	if code != 1 { // taken.example.com fails to create
		t.Errorf("exit %d: %s", code, stderr)
	}
	want := `/api/v1/apps/app_grafana/access {"access":"protected","idp_id":"idp_kc","policy":{"everyone":false,"emails":[],"email_domains":["example.com"],"groups":[]}}`
	if len(puts) != 1 || puts[0] != want {
		t.Errorf("PUTs %q\n%s", puts, stderr)
	}

	if code, _, stderr := run(t, "cloudflared", "-cf-access", "-cf-api", cf.URL, "-out", "-", src); code != 1 || !strings.Contains(stderr, "-cf-account") {
		t.Errorf("no account: %d %s", code, stderr)
	}
}
