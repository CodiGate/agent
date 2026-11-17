package importer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Remotely managed tunnels (P2-16): cloudflared started with --token (TUNNEL_TOKEN, the common
// Docker setup) has no config.yml; its ingress rules live at Cloudflare. Cloudflare reads them
// through the API and hands back the same document a config.yml holds, so Cloudflared() converts it.

// CloudflareAPI reads tunnel configurations with a Cloudflare API token that has the
// "Cloudflare Tunnel: Read" permission for the account.
type CloudflareAPI struct {
	BaseURL string // default https://api.cloudflare.com/client/v4
	Token   string
	Client  *http.Client
}

var (
	cfAccountPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	cfTunnelPattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// ParseTunnelRef accepts a tunnel id (UUID) or a tunnel token and returns the account and tunnel
// ids. A token is base64 JSON {"a": account, "t": tunnel, "s": secret}; the secret is never kept.
// account may be empty when ref is a UUID; the caller then needs it from elsewhere.
func ParseTunnelRef(ref string) (account, tunnel string, err error) {
	ref = strings.TrimSpace(ref)
	if cfTunnelPattern.MatchString(strings.ToLower(ref)) {
		return "", strings.ToLower(ref), nil
	}
	raw, err := base64.StdEncoding.DecodeString(ref)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(ref, "="))
	}
	var tok struct {
		A string `json:"a"`
		T string `json:"t"`
	}
	if err != nil || json.Unmarshal(raw, &tok) != nil || !cfAccountPattern.MatchString(tok.A) || !cfTunnelPattern.MatchString(tok.T) {
		return "", "", errors.New("not a tunnel id or a cloudflared tunnel token")
	}
	return tok.A, tok.T, nil
}

// TunnelConfig returns the tunnel's remote configuration in the config.yml shape (JSON, which
// Cloudflared() reads as YAML), with the tunnel's name as "tunnel".
func (c *CloudflareAPI) TunnelConfig(ctx context.Context, account, tunnel string) ([]byte, error) {
	if !cfAccountPattern.MatchString(account) || !cfTunnelPattern.MatchString(tunnel) {
		return nil, errors.New("cloudflare: invalid account or tunnel id")
	}
	base := "/accounts/" + account + "/cfd_tunnel/" + tunnel
	var info struct {
		Name      string `json:"name"`
		DeletedAt string `json:"deleted_at"`
	}
	if err := c.get(ctx, base, &info); err != nil {
		return nil, err
	}
	if info.DeletedAt != "" {
		return nil, fmt.Errorf("cloudflare: tunnel %s was deleted", tunnel)
	}
	var cfg struct {
		Source string                     `json:"source"`
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := c.get(ctx, base+"/configurations", &cfg); err != nil {
		return nil, err
	}
	var rules []json.RawMessage
	if cfg.Config == nil || json.Unmarshal(cfg.Config["ingress"], &rules) != nil || len(rules) == 0 {
		return nil, fmt.Errorf("cloudflare: tunnel %q has no remotely managed ingress rules; if it runs with a config.yml, import that file instead", info.Name)
	}
	name, _ := json.Marshal(info.Name)
	cfg.Config["tunnel"] = name
	return json.Marshal(cfg.Config)
}

func (c *CloudflareAPI) get(ctx context.Context, path string, into any) error {
	if c.Token == "" {
		return errors.New("cloudflare: no API token")
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://api.cloudflare.com/client/v4"
	}
	if u, err := url.Parse(base); err != nil || (u.Scheme != "https" && u.Hostname() != "127.0.0.1") {
		return fmt.Errorf("cloudflare: API URL must be https, got %q", c.BaseURL)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare: %w", err)
	}
	defer resp.Body.Close()
	var env struct {
		Success bool            `json:"success"`
		Errors  []cfError       `json:"errors"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&env); err != nil {
		return fmt.Errorf("cloudflare: HTTP %d, unreadable answer", resp.StatusCode)
	}
	if !env.Success || resp.StatusCode/100 != 2 {
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if len(env.Errors) > 0 {
			msg = env.Errors[0].String()
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			msg += " (the API token needs Account → Cloudflare Tunnel → Read for this account)"
		case http.StatusNotFound:
			msg += " (check the account and tunnel id)"
		}
		return errors.New("cloudflare: " + msg)
	}
	return json.Unmarshal(env.Result, into)
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e cfError) String() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }
