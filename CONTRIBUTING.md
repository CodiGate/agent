# Contributing

Thanks for helping. Issues with a clear reproduction (agent version, config with secrets removed, logs at `log_level: debug`) are the most useful contribution.

**Pull requests**: please open an issue first for anything bigger than a small fix. Before we can merge outside contributions we ask you to sign a contributor licence agreement (CLA); we'll send a link in the pull request. The agent stays Apache-2.0.

This repository is published from Codigate's internal monorepo: each release lands here as one commit. Accepted pull requests are applied there and show up in the next release, with you credited as co-author.

```sh
go test -race ./...
gofmt -l .        # must print nothing
go vet ./...
```
