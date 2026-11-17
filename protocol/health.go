package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Origin health checks (P3-12, ADR 0036).
//
// An agent that can check its origins sends "Codigate-Agent-Features: health" when it connects.
// Only then does the edge open health streams: a stream header with Kind "health", then one JSON
// HealthRequest. The agent answers with one JSON HealthResponse and closes the stream.
const (
	// HeaderAgentFeatures lists optional features of the agent, comma separated. Older edges ignore it.
	HeaderAgentFeatures = "Codigate-Agent-Features"
	// FeatureHealth means the agent answers health streams.
	FeatureHealth = "health"
	// KindHealth is the stream kind of a health request.
	KindHealth = "health"

	HealthUp      = "up"
	HealthDown    = "down"
	HealthUnknown = "unknown"

	// MaxHealthHosts bounds one request.
	MaxHealthHosts = 1000
	// MaxHealthError bounds the error text per host.
	MaxHealthError = 200
	// maxHealthBody bounds what either side reads.
	maxHealthBody = 1 << 20
)

// HealthRequest names the public hostnames the edge routes to this agent.
type HealthRequest struct {
	Hosts []string `json:"hosts"`
}

// HealthResult is the state of the origin behind one hostname.
type HealthResult struct {
	Host   string `json:"host"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// HealthResponse answers a HealthRequest.
type HealthResponse struct {
	Results []HealthResult `json:"results"`
}

// Validate checks the request's hostnames.
func (r HealthRequest) Validate() error {
	if len(r.Hosts) > MaxHealthHosts {
		return fmt.Errorf("protocol: more than %d hosts", MaxHealthHosts)
	}
	for _, h := range r.Hosts {
		if err := ValidateHostname(h); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks every result has a valid hostname, a known status and a short error.
func (r HealthResponse) Validate() error {
	if len(r.Results) > MaxHealthHosts {
		return fmt.Errorf("protocol: more than %d results", MaxHealthHosts)
	}
	for _, x := range r.Results {
		if err := ValidateHostname(x.Host); err != nil {
			return err
		}
		if x.Status != HealthUp && x.Status != HealthDown && x.Status != HealthUnknown {
			return fmt.Errorf("protocol: unknown health status %q", x.Status)
		}
		if len(x.Error) > MaxHealthError {
			return errors.New("protocol: health error too long")
		}
	}
	return nil
}

// ReadHealthRequest reads and validates one request.
func ReadHealthRequest(r io.Reader) (HealthRequest, error) {
	var req HealthRequest
	if err := json.NewDecoder(io.LimitReader(r, maxHealthBody)).Decode(&req); err != nil {
		return HealthRequest{}, fmt.Errorf("protocol: decode health request: %w", err)
	}
	return req, req.Validate()
}

// ReadHealthResponse reads and validates one response.
func ReadHealthResponse(r io.Reader) (HealthResponse, error) {
	var resp HealthResponse
	if err := json.NewDecoder(io.LimitReader(r, maxHealthBody)).Decode(&resp); err != nil {
		return HealthResponse{}, fmt.Errorf("protocol: decode health response: %w", err)
	}
	return resp, resp.Validate()
}

// TruncateHealthError shortens s to MaxHealthError bytes on a rune boundary.
func TruncateHealthError(s string) string {
	if len(s) <= MaxHealthError {
		return s
	}
	cut := MaxHealthError
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}
