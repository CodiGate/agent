#!/bin/sh
# Installs or upgrades codigate-agent as a systemd service (Debian, Ubuntu, Raspberry Pi OS,
# Proxmox LXC containers, ...).
#
#   curl -fsSL https://github.com/codigate/agent/releases/latest/download/install.sh | sudo CODIGATE_TOKEN=cga_... sh
#
# CODIGATE_TOKEN  agent token from the dashboard; written to /etc/codigate/token (first install only)
# CODIGATE_EDGE   edge URL (default https://connect.codigate.nl)
# VERSION         release tag to install, e.g. v0.4.0 (default: the latest release)
# CODIGATE_RELEASES  where releases live (default https://github.com/codigate/agent; for testing)
set -eu

REPO=${CODIGATE_RELEASES:-https://github.com/codigate/agent}
EDGE=${CODIGATE_EDGE:-https://connect.codigate.nl}
CONF_DIR=/etc/codigate

die() { echo "install.sh: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (sudo)"
command -v systemctl >/dev/null || die "systemd is required; use the Docker image otherwise"
command -v curl >/dev/null || die "curl is required"
command -v sha256sum >/dev/null || die "sha256sum is required"

case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	armv7l | armv7*) arch=armv7 ;;
	*) die "unsupported architecture $(uname -m)" ;;
esac

if [ -n "${VERSION:-}" ]; then
	base="$REPO/releases/download/$VERSION"
else
	base="$REPO/releases/latest/download"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
bin="codigate-agent_linux_$arch"
echo "Downloading $bin from $base"
for f in "$bin" codigate-agent.service SHA256SUMS; do
	curl -fsSL -o "$tmp/$f" "$base/$f" || die "download of $f failed"
done
(cd "$tmp" && grep -E " \*?($bin|codigate-agent\.service)\$" SHA256SUMS | sha256sum -c -) || die "checksum mismatch"

id codigate >/dev/null 2>&1 || useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin codigate
install -m 0755 "$tmp/$bin" /usr/local/bin/codigate-agent
install -m 0644 "$tmp/codigate-agent.service" /etc/systemd/system/codigate-agent.service
install -d -m 0750 -g codigate "$CONF_DIR"

if [ ! -f "$CONF_DIR/token" ] && [ -n "${CODIGATE_TOKEN:-}" ]; then
	(umask 027 && printf '%s\n' "$CODIGATE_TOKEN" >"$CONF_DIR/token")
	chgrp codigate "$CONF_DIR/token"
fi

if [ ! -f "$CONF_DIR/codigate.yml" ]; then
	cat >"$CONF_DIR/codigate.yml" <<CONF
# codigate-agent configuration: https://github.com/codigate/agent#run
edge: $EDGE
token_file: $CONF_DIR/token
log_level: info

# One rule per public hostname; the last rule is the catch-all.
ingress:
  # - hostname: grafana.acme.codigate.nl
  #   service: http://localhost:3000
  - service: http_status:404
CONF
	chmod 0640 "$CONF_DIR/codigate.yml"
	chgrp codigate "$CONF_DIR/codigate.yml"
	echo "Wrote $CONF_DIR/codigate.yml: add your ingress rules, then: systemctl enable --now codigate-agent"
	systemctl daemon-reload
	exit 0
fi

systemctl daemon-reload
systemctl enable codigate-agent >/dev/null
systemctl restart codigate-agent
echo "codigate-agent $(/usr/local/bin/codigate-agent --version) is running: journalctl -u codigate-agent -f"
