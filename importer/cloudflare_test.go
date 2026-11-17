package importer

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testAccount = "0123456789abcdef0123456789abcdef"
	testTunnel  = "c1744f8b-faa1-48a4-9e5c-02ac921467fa"
)

// fakeCloudflare answers like api.cloudflare.com/client/v4 for one tunnel.
func fakeCloudflare(t *testing.T, config string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer cf-token" {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"Authentication error"}],"result":null}`))
			return
		}
		base := "/accounts/" + testAccount + "/cfd_tunnel/" + testTunnel
		switch r.URL.Path {
		case base:
			w.Write([]byte(`{"success":true,"errors":[],"result":{"id":"` + testTunnel + `","name":"home-lab","deleted_at":null,"remote_config":true}}`))
		case base + "/configurations":
			w.Write([]byte(`{"success":true,"errors":[],"result":{"tunnel_id":"` + testTunnel + `","version":3,"source":"cloudflare","config":` + config + `}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"success":false,"errors":[{"code":1003,"message":"Tunnel not found"}],"result":null}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const remoteConfig = `{"ingress":[
	{"hostname":"grafana.example.com","service":"http://grafana:3000","originRequest":{}},
	{"hostname":"nas.example.com","path":"/admin","service":"http://nas:5000"},
	{"hostname":"nas.example.com","service":"https://nas:5001","originRequest":{"noTLSVerify":true}},
	{"service":"http_status:404"}],
	"originRequest":{"connectTimeout":30},
	"warp-routing":{"enabled":false}}`

func TestRemoteTunnelImports(t *testing.T) {
	srv := fakeCloudflare(t, remoteConfig)
	data, err := (&CloudflareAPI{BaseURL: srv.URL, Token: "cf-token"}).TunnelConfig(context.Background(), testAccount, testTunnel)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Cloudflared(data, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tunnel != "home-lab" || strings.Join(res.Apps, ",") != "grafana.example.com,nas.example.com" {
		t.Fatalf("tunnel %q apps %v", res.Tunnel, res.Apps)
	}
	if res.Warnings() != 1 { // the path rule
		t.Errorf("notes: %v", res.Notes)
	}
	if !strings.Contains(string(res.Config), "service: http://grafana:3000") || !strings.Contains(string(res.Config), "noTLSVerify: true") {
		t.Fatalf("config:\n%s", res.Config)
	}
}

func TestRemoteTunnelErrors(t *testing.T) {
	srv := fakeCloudflare(t, `{"ingress":[]}`)
	ctx := context.Background()
	if _, err := (&CloudflareAPI{BaseURL: srv.URL, Token: "cf-token"}).TunnelConfig(ctx, testAccount, testTunnel); err == nil || !strings.Contains(err.Error(), "no remotely managed ingress") {
		t.Errorf("empty config: %v", err)
	}
	if _, err := (&CloudflareAPI{BaseURL: srv.URL, Token: "wrong"}).TunnelConfig(ctx, testAccount, testTunnel); err == nil || !strings.Contains(err.Error(), "Cloudflare Tunnel → Read") {
		t.Errorf("bad token: %v", err)
	}
	other := "d1744f8b-faa1-48a4-9e5c-02ac921467fa"
	if _, err := (&CloudflareAPI{BaseURL: srv.URL, Token: "cf-token"}).TunnelConfig(ctx, testAccount, other); err == nil || !strings.Contains(err.Error(), "Tunnel not found") {
		t.Errorf("unknown tunnel: %v", err)
	}
	if _, err := (&CloudflareAPI{BaseURL: srv.URL, Token: "cf-token"}).TunnelConfig(ctx, testAccount, "../x"); err == nil {
		t.Error("path in tunnel id accepted")
	}
	if _, err := (&CloudflareAPI{BaseURL: "http://api.example.com", Token: "cf-token"}).TunnelConfig(ctx, testAccount, testTunnel); err == nil {
		t.Error("plain http API URL accepted")
	}
}

func TestParseTunnelRef(t *testing.T) {
	tok := base64.StdEncoding.EncodeToString([]byte(`{"a":"` + testAccount + `","t":"` + testTunnel + `","s":"c2VjcmV0"}`))
	for ref, want := range map[string][2]string{
		tok:                                     {testAccount, testTunnel},
		strings.TrimRight(tok, "="):             {testAccount, testTunnel},
		" " + strings.ToUpper(testTunnel) + " ": {"", testTunnel},
	} {
		a, tn, err := ParseTunnelRef(ref)
		if err != nil || a != want[0] || tn != want[1] {
			t.Errorf("%q: %q %q %v", ref, a, tn, err)
		}
	}
	for _, bad := range []string{"", "home-lab", base64.StdEncoding.EncodeToString([]byte(`{"a":"x","t":"y"}`))} {
		if _, _, err := ParseTunnelRef(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
