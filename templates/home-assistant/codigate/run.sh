#!/usr/bin/with-contenv bashio
# Turns the add-on options into an agent config. JSON is valid YAML, so jq writes it.
set -e

if ! jq -e '(.token // "") != ""' /data/options.json >/dev/null; then
	bashio::exit.nok "Set the agent token from the Codigate dashboard in the add-on configuration."
fi

jq '{
	edge: .edge,
	token: .token,
	log_level: .log_level,
	ingress: (
		(if (.hostname // "") != "" then [{hostname: .hostname, service: "http://homeassistant:8123"}] else [] end)
		+ [(.rules // [])[] | {hostname, service}]
		+ [{service: "http_status:404"}]
	)
}' /data/options.json >/tmp/codigate.yml

bashio::log.info "Starting codigate-agent $(codigate-agent --version)"
exec codigate-agent --config /tmp/codigate.yml
