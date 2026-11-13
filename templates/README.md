# Install templates

Ready-made ways to run the agent. They all end up with the same binary and the same config format (see the [main README](../README.md)).

| Where | Use |
| --- | --- |
| Docker / Compose | [compose/compose.yaml](./compose/compose.yaml): agent, a read-only Docker socket proxy and a labelled example app |
| Debian, Ubuntu, Raspberry Pi OS, Proxmox LXC | [linux/install.sh](./linux/install.sh) and the hardened [systemd unit](./linux/codigate-agent.service) |
| Unraid | [unraid/codigate-agent.xml](./unraid/codigate-agent.xml) |
| Home Assistant OS / Supervised | the add-on in [home-assistant/codigate](./home-assistant/codigate/) |

## Docker Compose

```sh
mkdir codigate && cd codigate
curl -fsSLO https://raw.githubusercontent.com/codigate/agent/main/templates/compose/compose.yaml
echo "CODIGATE_TOKEN=cga_…" > .env   # agent token from the dashboard
docker compose up -d
```

Change the `whoami` example to your own containers: a `codigate.hostname` label per app, and `codigate.port` when the container exposes more than one port.

## Linux with systemd

```sh
curl -fsSL https://github.com/codigate/agent/releases/latest/download/install.sh | sudo CODIGATE_TOKEN=cga_… sh
sudoedit /etc/codigate/codigate.yml   # add your ingress rules
sudo systemctl enable --now codigate-agent
```

The script picks the binary for the machine (`amd64`, `arm64`, `armv7`), checks it against the release's `SHA256SUMS`, creates a `codigate` system user and installs the unit. Running it again upgrades the binary and restarts the service; it never overwrites `codigate.yml` or the token. Pin a version with `VERSION=v0.4.0`.

To read the script first: download `install.sh` and `SHA256SUMS` from the [latest release](https://github.com/codigate/agent/releases/latest), check `sha256sum -c --ignore-missing SHA256SUMS`, then run it.

### Proxmox

Run the agent in a small unprivileged LXC container rather than on the Proxmox host itself:

1. Create a Debian 12 container (1 core, 128 MB RAM and 1 GB disk is plenty), unprivileged, with network access to the services you want to publish.
2. In its console: `apt install -y curl`, then the install command above.
3. Point the ingress rules at the LAN addresses of your VMs and containers, e.g. `service: https://192.168.1.20:8006` with `noTLSVerify: true` for the Proxmox web UI itself.

## Unraid

Docker → Add Container → Template repositories: add `https://github.com/codigate/agent`, or save [codigate-agent.xml](./unraid/codigate-agent.xml) to `/boot/config/plugins/dockerMan/templates-user/`. Fill in the agent token and put `codigate.yml` in `/mnt/user/appdata/codigate/`. Point rules at `http://<unraid-ip>:<port>`; the agent runs in the bridge network, so `localhost` is the agent itself.

For label discovery instead of a config file: run `tecnativa/docker-socket-proxy` (with `CONTAINERS=1` and `EVENTS=1`) on a custom network together with the agent, set *Docker discovery* to `true` and *Docker API* to `tcp://<proxy-name>:2375`, and add `codigate.hostname` labels to your containers (Edit → Add another Path, Port, Variable, Label or Device → Label).

## Home Assistant

Settings → Add-ons → Add-on store → ⋮ → Repositories → add `https://github.com/codigate/agent`, then install **Codigate**. See [DOCS.md](./home-assistant/codigate/DOCS.md) for the options and the one change Home Assistant itself needs (`trusted_proxies`).
