# Changelog

Every release in [github.com/codigate/agent](https://github.com/codigate/agent/releases) is one commit with the section below as its message. Binaries for Linux, macOS and Windows and a `SHA256SUMS` file are attached to the release; the Docker image is `ghcr.io/codigate/agent:vX.Y.Z` (`:latest` for stable releases).

## Unreleased

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
