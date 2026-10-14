// Package bridge is the relay side of tunr Agent Bridge: it connects a
// vendor's agent to a named resource inside a customer's network, through an
// outbound-only connector, under a time-boxed grant the customer approved.
//
// Everything here is behind TUNR_BRIDGE_ENABLED. With the flag off nothing in
// this package is mounted, no goroutine starts and no table is touched — the
// relay behaves exactly as it did before the package existed.
//
// Layout (filled in by roadmap tasks P1–P8, docs/protocol/bridge.md is the
// wire contract):
//
//	bridge.go         flag, Server, route registration
//	api.go            /api/v1/bridge/* management API (orgs, links, sites, agents, grants)
//	connector_hub.go  /bridge/connect — one WebSocket per customer site
//	access.go         /bridge/access  — agent-side streams, authorised per grant
//	sweeper.go        grant expiry; its own goroutine, never the app sweeper's
//
// Module boundary: nothing here imports the CLI module. Wire types are
// duplicated on the CLI side (internal/bridge) and kept in sync through
// docs/protocol/bridge.md.
package bridge
