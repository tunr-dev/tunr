# Changelog

All notable changes to tunr are documented here. This project adheres to
[Semantic Versioning](https://semver.org/).

## Unreleased

### Fixed
- **relay:** a search crawler (Googlebot, Bingbot, YandexBot) visiting a real
  page of a sleeping app got the edge's synthetic `200 ok` meant for health
  checks, so the page could be indexed as "ok". Crawlers on real pages now wake
  the app and get the real page, without resetting the idle clock. SEO tools and
  scanners on a sleeping app's real pages get `503` + `Retry-After` instead of
  the synthetic 200; health checks and uptime monitors are unchanged.
- **relay:** after a relay restart (idle clocks live in memory), an app reached
  only by monitors or crawlers never slept again: the sweeper skipped any app
  with no recorded request. It now starts the clock the first time it sees such
  an app, so a fresh deploy or a restarted relay gets one full idle window and
  then the normal HOT → WARM → STOPPED ladder.
- **cli:** `--json` output was interleaved with `INFO` lines on stdout, so
  `tunr share/tcp/udp/tls --json | jq` failed. Progress lines now go to stderr
  whenever `--json` is set.
- **docs:** removed claims for features that aren't wired up — OAuth2/SAML SSO,
  SOC2 audit logging and SSRF/private-IP validation exist only as unused code —
  and corrected the feedback widget description (it sends a text note; there
  are no visual pins, and the script is inline, not loaded from a CDN).

## v0.6.2 — 2026-09-29

### Security
- **proxy:** text sent to the `--inject-widget` feedback and JS-error endpoints
  was printed to the developer's terminal as-is, so a visitor could send ANSI
  escape sequences to spoof output or reach terminal-emulator bugs. Every field
  is now scrubbed of control characters, escape sequences and bidi overrides,
  capped at 500 characters, and request bodies are limited to 8 KB. Nothing
  sent to these endpoints was ever executed.

### Fixed
- **cli, docs:** `tunr tls` was documented as end-to-end, SNI-passthrough
  encryption the relay couldn't read, and as "compliance ready". The relay has
  never implemented that: `tls` is a raw tunnel like `tcp`, and the relay
  terminates TLS for `*.tunr.sh`. The help text, command output, README and
  docs now say so, and `tcp`/`udp`/`tls` are marked experimental — they are
  reachable only through the relay's WebSocket endpoint, which the output now
  prints (the old `ssh user@https://… -p 443` hint never worked).
- **cli:** `--allow-ip` on `tcp`/`udp`/`tls` was accepted and silently ignored,
  leaving the port open to everyone. It now exits with an error.
- **relay:** the raw-tunnel info page no longer tells visitors to use a plain
  TCP client, and its docs link no longer 404s.
- **cli:** `go install …/cmd/tunr@vX` builds reported `tunr version dev`; they
  now report the module version.

## v0.6.1 — 2026-09-26

**v0.6.0 was tagged but never published** — its GitHub release stayed a draft,
so `tunr update`, the install script and PyPI/npm never saw it, and the
Homebrew cask that did point at it 404'd. v0.6.1 is the first public release
carrying everything listed under v0.6.0 below.

### Changed
- **repo:** the canonical repository is now
  [github.com/tunr-dev/tunr](https://github.com/tunr-dev/tunr). Go module paths
  are `github.com/tunr-dev/tunr` and `github.com/tunr-dev/tunr/relay`;
  `tunr update` and the release pipeline point there.
- **docs:** new documentation at [tunr.sh/docs](https://tunr.sh/docs/),
  written against the actual CLI. The README no longer claims `ams`/`sea`/`sin`
  relays — one EU relay serves all traffic today; multi-region is planned.
- **contacts:** `dev@tunr.sh` for security and development, `contact@tunr.sh`
  for everything else.

### Fixed
- **install:** `https://tunr.sh/install.sh` returned 404 — the landing's
  one-liner never worked. The script is now published with the site, falls back
  to the legacy repository if the latest release can't be resolved, and honours
  `TUNR_INSTALL_DIR`.
- **deploy:** the >50 MB upload error suggested `.gitignore`/`.tunrignore`,
  neither of which the packer reads.
- **sdk:** Python `tunr.__version__` reported `0.4.0`.
- **release:** the npm and PyPI publish steps never ran — their `if:` read a
  variable from the step's own `env:`, which a step condition can't see. The
  npm job also lacked the `id-token` permission `--provenance` needs.

## v0.6.0 — 2026-08-12 (tagged, never published)

**Cloud deploy leaves private preview.** `tunr deploy` builds a directory with
Nixpacks and runs it in a gVisor sandbox on tunr's infrastructure — it sleeps
when idle, wakes on request, and keeps serving after you close your laptop. The
same pipeline is now reachable from an agent over MCP.

### Added
- **cli:** `tunr deploy [dir]` — pack, build (Nixpacks, no Dockerfile needed)
  and host a project; `--name`, `--port`, `--env KEY=VALUE`. `.env` files are
  never uploaded.
- **cli:** `tunr apps`, `tunr apps delete <name>` and **`tunr apps logs <name>`**
  (`--follow`, `--tail N`) — the last of which the README had been documenting
  without it existing.
- **cli:** global **`--relay <url>`** flag. The self-hosting docs have taught
  this command since v0.4.0, but the only real knob was `TUNR_RELAY_URL`, so
  the documented command failed with `unknown flag`. The env var still works.
- **mcp:** **`tunr_deploy`**, **`tunr_app_logs`** and **`tunr_delete_app`**.
  Previously the MCP surface was tunnel-only, so an agent asked to "deploy this"
  had no tool to reach for and fell back to opening a temporary tunnel — the
  right-looking answer to the wrong question. The `tunr_deploy` and `tunr_share`
  descriptions now state the distinction explicitly.
- **relay:** `GET /v1/apps/logs` (owner-scoped) and a matching runner endpoint,
  routed through the scheduler rather than the runner client.
- **cloud:** density levers — `HOT → WARM → STOPPED` lifecycle, cgroup memory
  QoS, build slice isolation, PSI safety valve, activity classification
  (probe/pin) so a monitored app can still fall asleep.
- **cloud:** multi-node readiness — `Scheduler`/`NodeClient` indirection, disk
  route-cache mirror (the data plane survives a Postgres outage), startup state
  reconciliation, `--role=all|agent|builder`.

### Fixed
- **mcp:** the server no longer writes log lines to **stdout**, which is the
  JSON-RPC transport. `INFO`/`WARN` now go to stderr in `tunr mcp`, so strict
  MCP clients stop seeing a parse error on connect.
- **runner:** `docker logs` processes are killed and reaped on close instead of
  leaking a zombie per request.
- **relay:** survive a host reboot — DB startup race, route cache, ephemeral
  firewall rules.
- **build:** `ref/` (pivot planning material) was committed by mistake in
  `4a57bb9`; it contains stray `.go` files whose package names collide, which
  broke `go build ./...` and `go test ./...` — the release gate. Untracked and
  ignored.

### Changed
- **licence:** the repository is now **dual-licensed by directory**. The CLI and
  SDKs (`cmd/`, `internal/`, `sdk/`) are **Apache-2.0** — OSI-approved open
  source. The relay, control plane and runner (`relay/`) stay **PolyForm Shield
  1.0.0**, source-available. Previously the README claimed "open source" in
  three places while the whole repo was PolyForm, and the published SDK packages
  declared Apache-2.0 — the split makes all of those statements true. See
  [NOTICE](NOTICE).
- **docs:** README restructured around the cloud; every tunnel feature is kept,
  in collapsible sections. The four tunnel-competitor tables moved to
  [docs/compare-tunnels.md](docs/compare-tunnels.md), with the incorrect
  "Open Source ✅" rows corrected to name the actual licence.
- **docs:** `SELF_HOSTING.md` now covers the cloud runner, not just tunnels.
- **sdk:** Python and Node packages bumped to 0.6.0 and published from CI on tag.

## v0.5.0 — 2026-07-24

This release marks the start of tunr's shift from a pure tunneling CLI toward a
place to **deploy, host and share the apps your agent builds**. The tunnel
(`tunr share`) stays free and first-class — it's now the on-ramp.

> **Cloud deploy (`tunr deploy`) is in private preview** and not part of this
> open-source build yet. This release ships the brand refresh plus build and
> robustness hardening; the tunnel/CLI feature set is unchanged and stable.

### Changed
- **brand:** new "t+c" monogram logo (renders navy in light UI, knocks out to
  white in dark), matching wordmark, and reworked positioning across the README,
  landing and dashboard.

### Fixed
- **cli:** `tcp` — check the forwarded `conn.Write`, close the WebSocket
  handshake response body, and drop an unused struct field.
- **cli:** `service uninstall` — the best-effort `systemctl`/`launchctl` calls no
  longer trip `errcheck`.
- **cli:** `up` — dropped a redundant `fmt.Sprintf("%s", …)`.
- **relay:** guard the `SetWriteDeadline` return in the TCP write path.

### CI / Build
- Pin the CI Go toolchain to 1.23: newer macOS runners' dyld rejected the
  race-instrumented test binaries older Go produced (`missing LC_UUID load
  command / abort trap`). Green across ubuntu/macos/windows again.
- `gofmt` + `golangci-lint` clean on both modules.

## v0.4.1 — 2026-06-11

### Security
- **relay:** `/auth/magic` no longer returns the login token in its HTTP response
  (was an account-takeover vector); the dev link is gated behind `TUNR_DEV_MODE`.
- **relay:** `/auth/verify` no longer mints a JWT for arbitrary tokens when no
  database is configured unless `TUNR_DEV_MODE=1`.
- **relay:** per-IP rate limiting added to `/auth/magic` (token spam / user
  enumeration / email flood).
- **relay:** fixed WebSocket origin bypass on `/tunnel/connect` & `/tunnel/tcp`
  (`strings.HasSuffix(origin, "tunr.sh")` accepted `eviltunr.sh`); now host-parsed.
- **relay:** bounded the in-memory rate-limiter bucket map with opportunistic
  eviction; `X-RateLimit-Limit` now reports the real per-plan value.

### Fixed
- **cli:** implemented the daemon `runCommand` helper (was a stub) so Windows
  process detection / `tunr stop` works.
- **cli:** fixed the invalid `http://localhost:*` CORS value in the internal API;
  loopback origins are now reflected correctly.

### Added
- **relay:** real per-user request/bandwidth usage metering, surfaced via
  `/api/user/profile` and `/api/user/usage` (previously hardcoded to `0`).

### Ops
- **caddy:** removed manual `header_up Upgrade/Connection` from the reverse proxy —
  Caddy v2 proxies WebSocket upgrades natively and those lines broke the handshake.

## v0.4.0 — 2026-04-16
- UDP & TLS tunnels, Docker image, multi-tunnel config, Prometheus metrics,
  Python & Node.js SDKs.
