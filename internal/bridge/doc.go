// Package bridge is the CLI side of tunr Agent Bridge.
//
// Subpackages (added by roadmap tasks P2–P5):
//
//	connector/          `tunr connect` — runs inside the customer's network,
//	                    dials the relay outbound only, verifies each grant JWT
//	                    against the pinned relay key, and connects only to
//	                    resources listed in its local config.
//	connector/pgproxy/  read-only Postgres proxy with credential injection.
//	access/             `tunr access` — agent-side local listener.
//
// Wire types mirror relay/internal/bridge; the contract is
// docs/protocol/bridge.md. No cross-module imports.
package bridge
