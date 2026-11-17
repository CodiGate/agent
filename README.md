# codigate-agent

The Codigate agent runs next to your services. It dials **out** to a Codigate edge on port 443, and requests for your public hostnames come back through that tunnel. You don't need open ports, a public IP or port forwarding.

Licence: Apache-2.0 ([LICENSE](./LICENSE)). The agent is open source so you can read exactly what runs on your network; the Codigate edge and control plane are a hosted service.

## Run

```sh
codigate-agent --config /etc/codigate/codigate.yml   # or CODIGATE_CONFIG=…
codigate-agent --version
```

The configuration format follows `cloudflared`'s `config.yml` ingress rules. See [codigate.example.yml](./codigate.example.yml), which is checked in CI as a valid config.

| Key | Meaning |
| --- | --- |
| `edge` | Edge URL, `https://` (env `CODIGATE_EDGE` overrides) |
| `token` / `token_file` | Agent token (env `CODIGATE_TOKEN` overrides both) |
| `edge_ca_file` | Extra CAs to trust for the edge (private CA, local dev) |
| `allow_insecure_edge` | Permit `http://` edges; local testing only |
| `log_level` | `debug`, `info`, `warn`, `error` |
| `ingress[]` | `hostname` (exact or `*.` one-label wildcard), `service` (`http://`, `https://`, `tcp://host:port`, `ssh://host[:port]`, `http_status:<code>`), `originRequest` (`noTLSVerify`, `httpHostHeader`, `originServerName`). The last rule must be a catch-all |
| `docker` | Label discovery, see [Docker labels](#docker-labels): `enabled` (env `CODIGATE_DOCKER`), `endpoint` (default `DOCKER_HOST` or the local socket), `network`, `create_apps` (env `CODIGATE_CREATE_APPS`) |
| `api` / `api_token` | Control plane URL and admin API token, only for `docker.create_apps` (env `CODIGATE_API`, `CODIGATE_API_TOKEN`) |

Unknown keys are an error, so typos don't silently do nothing. With `docker.enabled`, `ingress` may be empty: a catch-all `http_status:404` is implied.

`HTTPS_PROXY` / `NO_PROXY` are honoured for the connection to the edge.

## Run with Docker

The image `ghcr.io/codigate/agent` is built for `linux/amd64`, `linux/arm64` and `linux/arm/v7` (Raspberry Pi, Home Assistant boxes). It is built by this repository's release workflow from the tagged source: `:latest` is the newest stable release, `:vX.Y.Z` a fixed version (pre-releases such as `v0.1.0-rc.1` never move `:latest`).

```yaml
# compose.yaml, next to codigate.yml
services:
  codigate-agent:
    image: ghcr.io/codigate/agent
    restart: unless-stopped
    environment:
      CODIGATE_TOKEN: ${CODIGATE_TOKEN}   # from the dashboard; overrides token_file
    volumes:
      - ./codigate.yml:/etc/codigate/codigate.yml:ro
```

Inside the container `localhost` is the agent itself. Point `service:` at other containers by name (`http://grafana:3000`, when they share a network with the agent), at a LAN address, or use `network_mode: host` for services on the host.

## Install templates

Docker Compose, a systemd install script (Debian, Raspberry Pi OS, Proxmox LXC), Unraid, a Home Assistant add-on and a Helm chart for Kubernetes: see [templates](./templates/README.md). Release binaries for Linux, macOS and Windows, with `SHA256SUMS`, are on the [releases page](https://github.com/codigate/agent/releases).

## Docker labels

With `docker.enabled: true` the agent reads the labels of running containers and serves them, next to the rules in `ingress`. It follows Docker events, so starting, stopping or relabelling a container takes effect within seconds.

```yaml
# compose.yaml: no codigate.yml needed
services:
  codigate-agent:
    image: ghcr.io/codigate/agent
    restart: unless-stopped
    environment:
      CODIGATE_EDGE: https://connect.codigate.nl
      CODIGATE_TOKEN: ${CODIGATE_TOKEN}
      CODIGATE_DOCKER: "true"
      DOCKER_HOST: tcp://socket-proxy:2375
  # Read-only view of the Docker API: containers and events, nothing else. It holds the Docker
  # socket, so pin it by digest.
  socket-proxy:
    image: tecnativa/docker-socket-proxy:v0.5.0@sha256:1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459
    restart: unless-stopped
    environment:
      CONTAINERS: 1
      EVENTS: 1
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
  grafana:
    image: grafana/grafana
    labels:
      codigate.hostname: grafana.acme.codigate.app
      codigate.port: "3000"
```

| Label | Meaning |
| --- | --- |
| `codigate.hostname` | Public hostname(s), comma separated. Exact names only, no wildcards |
| `codigate.port` | Container port; default: the lowest exposed TCP port |
| `codigate.scheme` | `http` (default), `https`, `tcp` or `ssh` |
| `codigate.network` | Which network's address to use when the container is on several (default: `docker.network`, else the only/first one) |
| `codigate.no_tls_verify` | `true` for `https` origins with a self-signed certificate |
| `codigate.host_header` | Rewrite the `Host` header sent to the container |

Rules in `codigate.yml` win over labels for the same exact hostname; labels win over wildcard rules and the catch-all. When two containers claim the same hostname, neither is served and the agent logs the conflict. The agent must share a network with the containers it serves.

Every hostname still needs an app in the dashboard. With `CODIGATE_CREATE_APPS=true`, `CODIGATE_API=https://app.codigate.nl` and an admin `CODIGATE_API_TOKEN` the agent creates missing apps itself, protected and with nobody allowed until you add an access rule.

`CODIGATE_DOCKER=true` turns discovery on; with it, the agent also starts without a config file. The socket proxy keeps the agent away from the full Docker API (mounting `/var/run/docker.sock` directly gives root on the host, and the image's `nonroot` user can't read it anyway). Outside Docker, the agent uses `DOCKER_HOST` or `/var/run/docker.sock`.

## TCP and SSH

A `tcp://` or `ssh://` rule publishes a raw TCP service (SSH, a database, RDP) on an exact hostname. TCP needs the Codigate client on the connecting side; there is no browser login, so give the app a **service token** in the dashboard (Service tokens) and allow it in the app's access rules.

```sh
# SSH: ProxyCommand speaks over stdin/stdout
export CODIGATE_CLIENT_ID=… CODIGATE_CLIENT_SECRET=…
ssh -o ProxyCommand="codigate-agent access tcp %h" user@ssh.acme.codigate.app

# Anything else: a local listener, one tunnel per connection
codigate-agent access tcp --listen 127.0.0.1:5432 db.acme.codigate.app
psql -h 127.0.0.1 -p 5432 …
```

The connection runs over HTTPS on port 443 (an HTTP/1.1 upgrade, like WebSockets), so it works wherever the web does. Half-closed connections are not supported: when either side closes, both directions end.

## Move from cloudflared

```sh
codigate-agent import cloudflared ~/.cloudflared/config.yml            # writes codigate.yml
export CODIGATE_API_TOKEN=cgk_…                                         # admin API token (dashboard → API tokens)
codigate-agent import cloudflared -api https://app.codigate.nl -dry-run  # show what would be created
codigate-agent import cloudflared -api https://app.codigate.nl           # create an agent + the apps
```

The importer converts a locally managed tunnel's `config.yml`, or with `-cf` a remotely managed tunnel's rules from the Cloudflare API (see below). Every difference is reported as a `warn` or `info` note, on stderr and at the top of the generated file:

| cloudflared | Codigate |
| --- | --- |
| `http://`, `https://`, `http_status:` services | kept |
| `tcp://`, `ssh://` | kept on exact hostnames; clients use `codigate-agent access tcp` |
| `rdp://` | becomes `tcp://` (port 3389 when missing) |
| `smb://`, `unix:`, `hello_world`, bastion/SOCKS, TCP on a wildcard | rule skipped; a catch-all becomes `http_status:404` |
| `path:` rules | skipped; Codigate routes by hostname only |
| `*.example.com` | kept in the agent config, but each name needs its own app and only one label matches |
| `noTLSVerify`, `httpHostHeader`, `originServerName`, `matchSNIToHost` | kept (`matchSNIToHost` becomes `originServerName`) |
| `access` (Cloudflare Access at the origin) | recreate the policy in Codigate; the origin can verify `X-Codigate-User` |
| `caPool`, `ipRules`, `warp-routing` | not supported, warned |
| timeouts and keep-alive tuning | ignored, Codigate's defaults apply |

With `-api`, the importer creates a new agent (named after the tunnel; `-agent agt_…` reuses one) and saves its token to `-write-token` (default `./codigate-token`, mode 0600, never overwritten). It then creates one app per hostname. A hostname that is in use, or that is not under a verified domain, is reported and the rest continue (exit status 1). New apps are protected and not served until you set who may access them.

**cloudflared in Docker** with a mounted `config.yml`: run the importer from the image. `--user` lets it write `codigate.yml` into the current folder (the image runs as `nonroot`):

```sh
docker run --rm --user "$(id -u):$(id -g)" -w /out -v "$PWD":/out \
    -v ~/.cloudflared:/in:ro \
    ghcr.io/codigate/agent import cloudflared /in/config.yml
```

Service names such as `http://homeassistant:8123` keep working when the agent joins the network cloudflared was on.

**cloudflared started with `--token`** (remotely managed, the usual Docker setup) has no `config.yml`: its rules live in the Cloudflare dashboard. Create a Cloudflare API token with **Account → Cloudflare Tunnel → Read** (My Profile → API Tokens → Create Custom Token) and pass the tunnel token cloudflared runs with:

```sh
export CLOUDFLARE_API_TOKEN=…        # read-only, can be deleted afterwards
export TUNNEL_TOKEN=eyJh…            # the value after --token, or TUNNEL_TOKEN from your compose file
codigate-agent import cloudflared -cf                      # writes codigate.yml
codigate-agent import cloudflared -cf -api https://app.codigate.nl   # and creates the agent + apps
```

The importer reads only the account and tunnel id from the tunnel token; its secret is never used or sent. Instead of the token you can pass `-cf-tunnel <tunnel id> -cf-account <account id>` (both on the tunnel's page in Cloudflare Zero Trust).

**Cloudflare Access policies** carry over with `-cf-access` (the API token then also needs **Account → Access: Apps and Policies → Read**). The importer shows, per hostname, which rules it found; with `-api` and `-idp idp_…` it sets them on the new apps:

```sh
codigate-agent import cloudflared -cf -cf-access                                       # show the mapping
codigate-agent import cloudflared -cf -cf-access -api https://app.codigate.nl -idp idp_…  # create + set access
```

Codigate rules only allow (everyone who can log in, email addresses, email domains, groups), so:

| Cloudflare Access | Codigate |
| --- | --- |
| Allow: emails, email domains, everyone | the same |
| Allow: Entra ID groups, Okta groups, SAML `groups` attribute, OIDC `groups` claim | groups (the provider you choose with `-idp` must send them) |
| Allow: Access groups | their include rules, as above |
| Allow: IP ranges, countries, login methods, GitHub orgs, Google groups, lists | dropped and reported (fewer people get in, never more) |
| Service token rules, non-identity policies | dropped; create service tokens in Codigate |
| Require or exclude rules, deny or bypass policies | the app's access is **not imported**; set it yourself |
| Access applications for a path (`example.com/api`) | reported; Codigate uses one policy per hostname |

An app without imported access stays protected and is not served until you set its access.

## How it works

1. `POST <edge>/v1/tunnel` with `Upgrade: codigate-tunnel/1` and `Authorization: Bearer <token>`, forced to HTTP/1.1 via ALPN. The edge answers `101 Switching Protocols`.
2. Both sides run [yamux](https://github.com/hashicorp/yamux) over that connection; the edge opens one stream per request.
3. Each stream starts with a small header carrying the public hostname ([protocol/header.go](./protocol/header.go)). The agent picks the upstream from **its own** ingress rules by that hostname, never from the `Host` header, so the edge can't point the agent at anything you didn't configure.
4. The original `Host` and the edge's `X-Forwarded-For/Host/Proto` reach your origin. Errors the agent generates carry a `Codigate-Error` header (`no-ingress-rule`, `origin-unreachable`, `tcp-service`). TCP rules accept only the `codigate-tcp/1` upgrade and then copy bytes both ways.
5. On disconnect the agent reconnects with full-jitter exponential backoff (0.5 s to 30 s). A rejected token waits the maximum delay between attempts. On SIGINT/SIGTERM it drains in-flight requests (up to 10 s).

## Packages

| Package | What |
| --- | --- |
| [protocol](./protocol/) | Wire protocol shared with the edge: connect path, upgrade token, headers, stream header, yamux settings |
| [config](./config/) | YAML config, validation, ingress matching |
| [tunnel](./tunnel/) | Tunnel client (connect, serve, reconnect) and the ingress handler (HTTP and TCP) |
| [docker](./docker/) | Docker API client and label discovery |
| [access](./access/) | Client side of TCP apps (`codigate-agent access tcp`) |
| [importer](./importer/) | `cloudflared` config conversion and the small `/api/v1` client used by `import` |
| [cmd/codigate-agent](./cmd/codigate-agent/) | The binary |

## Develop

```sh
go test -race ./...
go test -fuzz FuzzReadStreamHeader -fuzztime 30s ./protocol/
go build -ldflags "-X main.version=dev" -o codigate-agent ./cmd/codigate-agent
```

`tunnel/client_test.go` contains a fake edge that can be reused for agent-only tests. End-to-end tests against the real edge run in Codigate's own CI before every release.
