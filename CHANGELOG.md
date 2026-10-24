# Changelog

All notable changes to tunr are documented here. This project adheres to
[Semantic Versioning](https://semver.org/).

## v0.9.0 — 2026-10-24

### Added
- **bridge:** SQL Server resources (`--kind mssql`) — Nebim, Logo, Netsis and
  most Turkish ERPs. The connector runs each statement itself with a read-only
  login, inside a transaction it always rolls back, behind a T-SQL allow-list;
  the agent never gets a SQL Server login. `tunr access` serves it as a local
  `POST /query` endpoint.
- **bridge:** `tunr connect mssql-role` prints the T-SQL for a read-only login
  (`SELECT` only, `DENY` on writes, `EXECUTE`, `ALTER`, `UNMASK`), optionally
  with SQL Server Dynamic Data Masking (`--mask`).
- **bridge:** schema profiles for Nebim V3, Logo Tiger/Go and Netsis
  (`tunr profile list|show`, `resource add --profile`). `tunr connect resource
  verify` checks the login is read-only and which profile tables exist; only
  those table names are shared with linked vendors.
- **bridge:** MCP tools `tunr_describe_resource` (the profile, verified or not)
  and `tunr_query` (one read-only statement, rows as JSON, 200 rows by default)
  for postgres and mssql resources.
- **bridge:** column masking for postgres resources (`resource add --mask
  table.column=redact|partial:N|hash|null`), following aliases and refusing
  expressions over masked columns.
- **bridge:** grants act for a person — the requester, or whoever issued the
  agent's token. The customer sees who; the grant stops working when that
  person leaves the vendor org.
- **bridge:** one-time connect links for vendors (`tunr invite create`): the
  customer signs in, gets an org, a link to the vendor and a site in one step.
- **docs:** a brief to paste into a coding agent
  (`docs/bridge/agent-prompt.md`) and a guide for the customer's IT team in
  English and Turkish.

### Security
- **bridge:** a portal sign-in link asks before signing in unless this browser
  requested that address's link (login CSRF).
- **bridge:** a hosted agent's app is managed by its org's current
  owners/admins; whoever deployed it loses control with the role.
- **bridge:** hosted-agent hostnames stay guarded when the relay starts while
  Postgres is down.
- **bridge:** a busy agent stream gets backpressure instead of disconnecting
  the whole site.

### Removed
- **relay:** the unused `/auth/magic` and `/auth/verify` routes (sign-in lives
  in the dashboard).
- **cli:** the `e2e` alias of `tunr tls`: raw TLS tunnels are not end-to-end
  encrypted (`tls-proxy` still works).

## v0.8.1 — 2026-10-23

### Security
- **relay:** a deployed app always owns its hostname; a tunnel can no longer
  take over another user's app or hosted-agent subdomain. Host matching is
  case-insensitive.
- **relay:** `POST /v1/apps` no longer accepts a caller-chosen upstream.
- **bridge:** revoking a link, disabling a resource or an agent, and revoking
  an agent token now end live streams and the grants behind them. "Cut all
  access" also denies pending grants and turns auto-approve off.
- **bridge:** grant webhooks only reach public addresses and don't follow
  redirects.
- **cli:** the connector's Postgres proxy no longer lets agents change
  `statement_timeout`, `client_encoding` or `standard_conforming_strings`,
  refuses non-UTF-8 statements, checks quoted function names, and bounds
  prepared statements per session.
- **cli:** the portal's connect command reads the site token from a prompt
  (`--token-stdin`); `--token` only runs setup. Keychain writes on macOS and
  the Windows login token no longer pass secrets on a command line.
- **cli:** `tunr access` raw-TCP ports refuse other users' processes on Linux
  and warn elsewhere.
- **bridge:** an org admin can't change an owner's role, and an org keeps at
  least one owner. Portal sign-in is rate-limited per IP, and a new sign-in
  link no longer cancels an earlier one.
- **relay:** removed the unused `/api/user/token/rotate` route. Disabled hosted
  agents keep their hostname guarded. Audit CSV cells are formula-safe.

## v0.8.0 — 2026-10-23

tunr's headline is now **Agent Bridge**: AI vendors' agents get read-only,
time-boxed, customer-approved and audited access to data inside a customer's
network through one outbound-only connector. Tunnels and deploy are unchanged.

### Added
- **cli:** `tunr connect` runs the site connector inside the customer's network
  (outbound only), with `resource add`, `pg-role` (the SQL for a `SELECT`-only
  role), `install`/`uninstall` as a hardened system service, `status`, and a
  local kill switch (`kill`/`resume`) that holds across restarts.
- **cli:** the connector proxies Postgres read-only: a `SELECT`-only role,
  read-only transactions, a statement timeout and a statement classifier;
  writes fail with SQLSTATE 42501 and the session stays usable. Row, byte and
  rate limits per grant.
- **cli:** `tunr access request|list|status` and `tunr access <org>/<resource>`
  open an authorised local port to a resource, end-to-end TLS to the connector
  pinned to the site's certificate (the fingerprint is logged so it can be
  compared). The agent's DSN carries a per-process secret, never a database
  password.
- **cli:** `tunr agent create|token|deploy|trigger`; MCP tools
  `tunr_list_resources`, `tunr_request_access`, `tunr_access_status`,
  `tunr_open_access`, `tunr_close_access`.
- **relay:** bridge API under `/api/v1/bridge/*` (orgs, members, vendor links,
  sites, resources, agents and tokens, grants with approve/deny/revoke and
  automatic expiry, sessions and query summaries, audit log with CSV export,
  signed webhooks, org settings, site kill switch), per-principal rate limits,
  a metrics endpoint, and an embedded portal at `/portal/`. Off unless
  `TUNR_BRIDGE_ENABLED=1`; requires `DATABASE_URL` and
  `TUNR_GRANT_SIGNING_KEY`.
- **relay:** hosted agents (private apps woken by cron or signed webhooks, with
  hourly identity tokens and a `/data` volume), opt-in with
  `TUNR_BRIDGE_HOSTED_AGENTS=1`. Early access: egress and CPU quotas are not
  enforced yet, and the volume size is not capped by the filesystem.
- **relay:** migrations `005`–`009` (orgs, bridge, webhooks, hosted agents,
  site TLS). Re-runnable, like every migration.

### Fixed
- **relay:** the relay's own routes (`/api/v1/*`, `/auth/*`, `/v1/apps`, and
  with the bridge on `/portal/` and `/bridge/*`) shadowed the same paths on
  every tunnel and app. They now answer only on the relay's own hosts.
  `relay`, `bridge`, `app`, `www` and `api` are reserved names.
- **self-host:** `docker compose up` works as documented: a Caddyfile with
  on-demand certificates (gated by `/api/v1/tls/ask`), `.env.example`, and the
  schema applied on first start.
- **cli:** `tunr access --require-e2e` was documented but not registered.
- **relay:** a request still being served no longer lets the sweeper pause its
  app, and finishing it restarts the idle clock (#8).
- **relay:** browser TCP connections are refused before the WebSocket upgrade
  (#10); fresh databases get `users.paddle_customer_id` (#7).

### Security
- **build:** golang.org/x/text v0.41.0 (GO-2026-6629) in both modules (#9).
- **build:** the production runner image builds with Go 1.27 (was 1.22) and
  for amd64 and arm64.

## v0.7.0 — 2026-10-02

### Security
- **cli:** WebSocket connections skipped every tunnel access check. A tunnel
  shared with `--password`, `--auth-token` or `--allow-ip` accepted WebSockets
  from anyone, because the relay bridges them outside the HTTP middleware.
  They now pass the same checks before the dev server is dialled.
- **relay:** the client IP was read from `Fly-Client-IP` / `CF-Connecting-IP`
  first, headers any visitor can send, so `--allow-ip` and the per-IP rate
  limits (including magic-link e-mails) could be bypassed. Forwarding headers
  are now trusted only from the relay's own reverse proxy, and the WebSocket
  path stamps `X-Forwarded-For` the way the HTTP path does.
- **relay:** dependencies bumped past vulnerabilities the relay calls: pgx
  v5.9.2 (GO-2026-5004), golang-jwt v5.2.2 (GO-2025-3553), gorilla/websocket
  v1.5.3 (GO-2026-6278), x/text v0.39.0 (GO-2026-5970).
- **build:** binaries are built with Go 1.27 (were 1.22, which carried 30 known
  standard-library vulnerabilities). `go install` needs Go 1.26+.

### Added
- **cli:** `--demo-allow` / `--demo-block` rules (`"[METHOD ]/path[*]"`, `WS` for
  sockets) to let a read-only POST through or block a GET that writes; also
  `demoAllow` / `demoBlock` in `.tunr.json`.

### Changed
- **cli:** `--demo` now also drops messages the visitor sends over WebSockets
  (server pushes still arrive; dev-server HMR sockets are exempt), lets GraphQL
  queries sent as POST through (mutations stay blocked), and echoes a JSON
  request body back instead of a generic payload. Blocked GraphQL mutations get
  `200 {"data": null}`.
- **cli:** `"demo": true` on a tcp/udp/tls tunnel is now an error instead of
  being silently ignored.

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
