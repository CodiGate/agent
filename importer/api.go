package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// API is a minimal client for the Codigate workspace API (/api/v1), enough to set up imported apps.
type API struct {
	BaseURL string // e.g. https://app.codigate.nl
	Token   string // workspace API token (cgk_…), admin role
	Client  *http.Client
}

// APIError is a non-2xx answer; Message is the server's explanation when it gave one.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// CreateAgent creates an agent and returns its id and its token (shown only this once).
func (a *API) CreateAgent(ctx context.Context, name string) (id, token string, err error) {
	var out struct {
		Data  struct{ ID string } `json:"data"`
		Token string              `json:"token"`
	}
	if err := a.do(ctx, http.MethodPost, "/agents", map[string]string{"name": name}, &out); err != nil {
		return "", "", err
	}
	if out.Data.ID == "" || out.Token == "" {
		return "", "", errors.New("unexpected response from POST /agents")
	}
	return out.Data.ID, out.Token, nil
}

// CreateApp creates a (protected, not yet served) app for hostname behind agentID.
func (a *API) CreateApp(ctx context.Context, name, hostname, agentID string) (string, error) {
	var out struct {
		Data struct{ ID string } `json:"data"`
	}
	err := a.do(ctx, http.MethodPost, "/apps", map[string]string{"name": name, "hostname": hostname, "agent_id": agentID}, &out)
	return out.Data.ID, err
}

// ProtectApp sets an app's access: log in with idpID, then policy decides who gets in.
func (a *API) ProtectApp(ctx context.Context, appID, idpID string, policy *Policy) error {
	var out struct{}
	return a.do(ctx, http.MethodPut, "/apps/"+url.PathEscape(appID)+"/access", map[string]any{"access": "protected", "idp_id": idpID, "policy": policy}, &out)
}

func (a *API) do(ctx context.Context, method, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.BaseURL, "/")+"/api/v1"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message string              `json:"message"`
			Errors  map[string][]string `json:"errors"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Message
		for _, list := range e.Errors { // Laravel puts the specific reason here.
			if len(list) > 0 {
				msg = list[0]
				break
			}
		}
		return &APIError{Status: resp.StatusCode, Message: msg}
	}
	return json.Unmarshal(data, out)
}

// AppName derives a readable app name from a hostname: "grafana.acme.com" → "grafana".
func AppName(hostname string) string {
	name, _, _ := strings.Cut(hostname, ".")
	return name
}
