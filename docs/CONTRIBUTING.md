# Contributing to tunr

Thank you for your interest in contributing! tunr welcomes contributions from the community.

## What's in This Repository

This repository holds two Go modules:

- **The tunr CLI** (root module, Apache-2.0, [LICENSE](../LICENSE)) — the client binary you install and run locally.
- **The relay** (`relay/`, its own `go.mod`, PolyForm Shield 1.0.0, [relay/LICENSE](../relay/LICENSE)) — the server that tunnels, the Agent Bridge and deploys connect to.

Open to contributions:
- `cmd/tunr/` — CLI commands
- `internal/` — Core packages (tunnel, proxy, inspector, mcp, auth, config, bridge, etc.)
- `relay/` — Relay server
- `sdk/` — Python, Node, Go and JS SDKs
- `docs/` — Documentation

### Agent Bridge code

The bridge (preview) spans both modules. Keep the module boundary: the CLI never imports `relay/internal/...` and vice versa.

| Where | What |
|-------|------|
| `internal/bridge/connector/` | `tunr connect`: connector client, config, stream handling, grant verification, end-to-end TLS, service install |
| `internal/bridge/connector/pgproxy/` | Read-only Postgres proxy and statement classifier |
| `internal/bridge/access/` | `tunr access`: local listener and end-to-end TLS client |
| `internal/bridge/api/` | HTTP client for `/api/v1/bridge/*`, agent token source |
| `internal/mcp/bridge.go` | Bridge MCP tools |
| `cmd/tunr/connect.go`, `access.go`, `bridge_cmds.go` | Cobra commands |
| `relay/internal/bridge/` | Relay side: connector hub, access, grants, sweeper, notifications, hosted agents, cron, embedded portal (`portal/`) |
| `relay/internal/auth/` | Agent/site tokens and Ed25519 grant signing |
| `relay/migrations/005`–`009` | Bridge schema |

Design and wire contract: [adr/0001-agent-bridge.md](adr/0001-agent-bridge.md), [protocol/bridge.md](protocol/bridge.md). Any new message type must be handled by both sides, and both must ignore unknown types.

## Getting Started

```bash
# Clone the repo
git clone https://github.com/tunr-dev/tunr.git
cd tunr

# Install dependencies
go mod download

# Build
go build -o tunr ./cmd/tunr

# Run tests
go test ./...

# Lint (requires golangci-lint)
golangci-lint run

# The relay is a separate module
cd relay && go build ./cmd/server && go test ./...
```

### Bridge tests

Relay tests that need PostgreSQL are skipped unless `TUNR_TEST_DATABASE_URL`
points at an admin DSN (one that may `CREATE DATABASE`); each test gets a
throwaway database with every `relay/migrations/*.sql` applied:

```bash
docker run -d --rm -p 5432:5432 -e POSTGRES_PASSWORD=postgres postgres:16-alpine
cd relay && TUNR_TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable' go test ./...
```

End-to-end suites (Docker Compose v2, `curl`, `jq`, `python3`; see
[scripts/e2e/bridge/README.md](../scripts/e2e/bridge/README.md)):

```bash
./scripts/e2e/bridge/run.sh      # connector, grants, read-only Postgres, revoke, audit — on isolated networks
./scripts/e2e/bridge/hosted.sh   # hosted agents through a real runner (also needs openssl and go)
KEEP=1 ./scripts/e2e/bridge/run.sh   # leave the stack up afterwards
```

Run both before opening a PR that touches bridge code; CI runs them in
`.github/workflows/bridge.yml`.

## Before You Open a PR

1. **Check existing issues** — your feature or bug may already be tracked
2. **Open an issue first** for significant changes — let's discuss before you invest time
3. **Security issues** — see [SECURITY.md](../SECURITY.md), do NOT open public issues for vulnerabilities

## Code Standards

- **Go idioms** — follow standard Go conventions
- **Tests** — new features need tests, bug fixes should add a regression test
- **Lint** — CI runs `golangci-lint`; fixes must pass
- **Security** — CI runs `govulncheck`; no known CVEs
- **Comments** — exported functions must have doc comments

## Pull Request Process

1. Fork → branch (`feat/`, `fix/`, `docs/` prefix)
2. Make changes + tests
3. `go test ./...` must pass
4. `go vet ./...` must be clean
5. Open PR against `main`
6. Fill out the PR template
7. CI must pass before merge

## Commit Style

We loosely follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat: add --ttl flag to share command
fix: handle graceful shutdown on SIGHUP
docs: update MCP configuration example
chore: bump gorilla/websocket to v1.5.3
```

## Questions?

- Open a [GitHub Discussion](https://github.com/tunr-dev/tunr/discussions)
- Join [Discord](https://discord.gg/tunr)

## License

By contributing, you agree that your contributions are licensed under the license of the module you change: [Apache-2.0](../LICENSE) for the CLI, and the [PolyForm Shield License 1.0.0](../relay/LICENSE) for `relay/`, which allows use, modification and sharing but not building a competing service.
