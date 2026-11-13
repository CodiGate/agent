# Security

Please report vulnerabilities privately, not in a public issue:

- GitHub: **Security → Report a vulnerability** on this repository, or
- email security@codigate.nl.

We confirm within two working days and keep you informed until it is fixed. Once a fix is released we credit you in the release notes, unless you'd rather not.

In scope: this agent, and how it talks to the Codigate edge. For the hosted service (edge, dashboard, login) the same addresses apply. Please don't test against other customers' apps or degrade the service for others.

## Design notes

- The agent only makes outbound connections. It picks the upstream for a request from **its own** ingress rules by the public hostname; the edge can't make it connect anywhere you didn't configure.
- The agent token can only open a tunnel for that agent's apps. Rotate it in the dashboard if it leaks.
- Release binaries come with a `SHA256SUMS` file; `install.sh` refuses binaries that don't match.
