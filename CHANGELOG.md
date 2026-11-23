# Changelog

Every release in [github.com/codigate/agent](https://github.com/codigate/agent/releases) is one commit with the section below as its message. Binaries for Linux, macOS and Windows and a `SHA256SUMS` file are attached to the release; the Docker image is `ghcr.io/codigate/agent:vX.Y.Z` (`:latest` for stable releases) and the Helm chart `oci://ghcr.io/codigate/charts/codigate-agent` version `X.Y.Z`.

## 0.2.2

- Clear start-up errors for a broken configuration: the config path is a directory (Docker creates an empty directory when the mounted file does not exist on the host; the message says how to fix it), the file is missing or empty, a YAML error (with the line), no ingress rules, or no token. The agent prints one message and exits instead of a bare read error.

## 0.2.1

- `import cloudflared`: merging `originRequest` defaults into a rule no longer sizes the map from a sum of lengths (CodeQL: size computation may overflow). No change in behaviour.

## 0.2.0

- Helm chart for Kubernetes: `helm install codigate-agent oci://ghcr.io/codigate/charts/codigate-agent --version X.Y.Z`. Token from an existing Secret or from values, ingress rules from values or an existing ConfigMap, replicas with an optional PodDisruptionBudget, a restricted security context and no Kubernetes API access. The chart is released with every agent version.
- `import cloudflared -cf`: imports a remotely managed tunnel (cloudflared run with `--token`) by reading its rules from the Cloudflare API, with a read-only API token and the tunnel token (whose secret is never used).
- `import cloudflared -cf-access`: carries Cloudflare Access policies over to the new apps where that is safe (allow rules on emails, domains, groups, everyone); anything stricter-than-allow is reported and left for you.
- Origin health checks: the agent checks each origin every 30 seconds (an HTTP GET where any answer below 500 counts as up, honouring `noTLSVerify`, with a 5 second timeout; a TCP connect for `tcp://` and `ssh://` rules) and reports the result to the edge when asked. The dashboard shows the status and can alert you when an origin stays down. Older edges never ask, so nothing changes there.

## 0.1.0

First release.

- HTTP(S) and WebSocket tunnel to the Codigate edge over one outbound TLS connection on port 443, with `cloudflared`-style ingress rules.
- TCP services: `tcp://host:port` and `ssh://host[:port]` rules, and `codigate-agent access tcp` for the connecting side (SSH `ProxyCommand` or a local listener), authenticated with a service token.
- Docker-label discovery: `codigate.hostname`, `codigate.port`, `codigate.scheme` and friends on your containers, followed live through Docker events; optional `create_apps`.
- Environment-only setup: `CODIGATE_DOCKER`, `CODIGATE_CREATE_APPS`, `CODIGATE_API`; no config file needed with label discovery.
- `import cloudflared`: converts a `config.yml` (HTTP, TCP and SSH rules; `rdp://` becomes `tcp://`) and can create the agent and apps through the API.
- Install templates: Docker Compose, `install.sh` with a hardened systemd unit (Debian, Raspberry Pi OS, Proxmox LXC), Unraid and a Home Assistant add-on.
- The Compose template pins `docker-socket-proxy` and `whoami` by digest (the socket proxy holds the Docker socket).
- Multi-arch Docker image (`linux/amd64`, `linux/arm64`, `linux/arm/v7`).

## 0.1.0-rc.1

Release candidate for 0.1.0, to test the release chain; the same features.
