# Codigate add-on

Reach Home Assistant (and other add-ons) from anywhere through [Codigate](https://codigate.nl): an EU-hosted tunnel with a login in front of your apps. Nothing is opened on your router; the add-on dials out to Codigate on port 443.

## Setup

1. In the Codigate dashboard: create an agent and copy its token, and create an app for the hostname you want, for example `ha.acme.codigate.app`.
2. In the add-on configuration: paste the token in **token** and the hostname in **hostname**. Start the add-on.
3. Home Assistant only accepts proxied requests from proxies it trusts. Add this to `configuration.yaml` and restart Home Assistant:

   ```yaml
   http:
     use_x_forwarded_for: true
     trusted_proxies:
       - 172.30.33.0/24 # the add-on network
   ```

Without step 3, Home Assistant answers `400: Bad Request`.

## Options

| Option | Meaning |
| --- | --- |
| `token` | Agent token from the Codigate dashboard |
| `hostname` | Public hostname for Home Assistant itself (proxied to `http://homeassistant:8123`). Leave empty to publish only `rules` |
| `rules` | More apps: a `hostname` and a `service`, e.g. `http://a0d7b954-nodered:1880` for an add-on, or `http://192.168.1.20:80` for a device on your network |
| `edge` | Codigate edge URL; leave the default |
| `log_level` | `debug`, `info`, `warn` or `error` |

## Who does the login

- **Home Assistant's own login**: make the app public in the Codigate dashboard. The companion app works as usual. Turn on two-factor authentication for every Home Assistant user, because the login page can be reached by anyone.
- **An extra login at Codigate first**: keep the app protected. The companion app can't complete that login, so open `/api/` and `/auth/token` without it (bypass paths); Home Assistant's own tokens still protect those.

Both need a paid plan and your own domain. The [Home Assistant guide](https://codigate.nl/home-assistant/) explains the trade-off.
