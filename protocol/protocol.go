// Package protocol defines the wire format between the Codigate agent and the edge.
//
// Connection setup: the agent sends an HTTP/1.1 request
//
//	POST /v1/tunnel
//	Connection: Upgrade
//	Upgrade: codigate-tunnel/1
//	Authorization: Bearer <agent token>
//
// and the edge answers "101 Switching Protocols". From then on both sides run a yamux session over
// the same (TLS) connection. The edge is the yamux client and opens one stream per proxied HTTP
// connection; the agent is the yamux server and accepts them.
//
// Every stream starts with a stream header (see WriteStreamHeader), followed by raw HTTP/1.1.
package protocol

import (
	"io"
	"time"

	"github.com/hashicorp/yamux"
)

const (
	// ConnectPath is the path on the connect host that agents upgrade.
	ConnectPath = "/v1/tunnel"
	// UpgradeToken is the value of the Upgrade header. The suffix is the protocol version; the edge
	// supports the current and the previous version.
	UpgradeToken = "codigate-tunnel/1"

	// TCPUpgradeToken is the Upgrade value for a raw TCP connection to an app whose rule is
	// tcp:// or ssh:// ("codigate-agent access tcp"). The edge passes it through like a WebSocket;
	// after the agent's 101 the connection carries the raw bytes in both directions.
	TCPUpgradeToken = "codigate-tcp/1"

	// HeaderAgentVersion carries the agent's build version, for the dashboard and support.
	HeaderAgentVersion = "Codigate-Agent-Version"
	// HeaderAgentID is returned by the edge in the 101 response, so the agent can log who it is.
	HeaderAgentID = "Codigate-Agent-Id"
	// HeaderEdgeID is returned by the edge in the 101 response.
	HeaderEdgeID = "Codigate-Edge-Id"
)

// YamuxConfig is the session configuration both sides must use.
func YamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.ConnectionWriteTimeout = 10 * time.Second
	cfg.StreamOpenTimeout = 10 * time.Second
	cfg.StreamCloseTimeout = 2 * time.Minute
	cfg.MaxStreamWindowSize = 1 << 20
	cfg.LogOutput = io.Discard
	return cfg
}
