# Development and releases

```bash
make build               # bin/aicatraz
make test                # unit tests (add -race for the heavy ones: a couple of minutes)
make test-integration    # real containers: needs Docker and network access
make release             # static binaries and SHA256SUMS in dist/
make release-check       # builds twice, in different directories, and compares the hashes
```

The layout is `cmd/aicatraz` (the one binary: launcher, broker, shim and CLI, chosen by the name it is started under) and `internal/*`, one package per concern: `config` (schema and linter), `jail` (container, volumes, build, sessions), `broker`, `shim` and `proto` (the host door), `policy`, `pins`, `approve`, `canary`, `agent`, `egress` (network rules), `devices`, `mux` (the session multiplexer), `examples`, `osrelease`, `ui`. Every Go file carries `SPDX-License-Identifier: GPL-3.0-only`, and a test keeps it that way.

## Releases

```bash
make release          # dist/aicatraz-<version>-linux-{amd64,arm64} and dist/SHA256SUMS
make release-check    # builds twice, in different directories, and compares the hashes
```

Builds use `CGO_ENABLED=0`, `-trimpath`, `-buildvcs=false` and no build id, so the same source and the same Go toolchain give byte-identical binaries. To verify a release: check out the tag, run `make release` with the Go version shown by `aicatraz version`, and compare `dist/SHA256SUMS`. `make release` refuses to run on a tree with uncommitted changes.

Pushing a tag such as `v0.1.0` runs `.github/workflows/release.yml`: it vets and tests, checks that the build is reproducible, builds both binaries and publishes them, with `SHA256SUMS`, as a GitHub release.
