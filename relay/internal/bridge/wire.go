package bridge

import "encoding/json"

// wire.go — connector/agent ↔ relay messages. Contract: docs/protocol/bridge.md.
// The CLI module keeps its own copy (internal/bridge/wire.go); change both.
//
// Envelope is the same as the tunnel protocol ({type, data}) so tooling and
// framing code look alike, but bridge messages only travel on /bridge/*
// sockets. Both peers ignore unknown types.

// Message is the envelope.
type Message struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Message types.
const (
	MsgHello   = "bridge_hello"   // connector → relay, first message
	MsgWelcome = "bridge_welcome" // relay → connector
	MsgOpen    = "bridge_open"    // relay → connector: open an authorised stream
	MsgData    = "bridge_data"    // both ways
	MsgClose   = "bridge_close"   // both ways
	MsgAudit   = "bridge_audit"   // connector → relay: per-query summary
	MsgKill    = "bridge_kill"    // relay → connector: close streams now
	MsgPing    = "ping"           // relay → connector
	MsgPong    = "pong"           // connector → relay
	MsgError   = "error"          // relay → connector, then close
)

// ResourceDecl is a resource a connector announces. Never an address.
type ResourceDecl struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// HelloData is the connector's first message. The site token travels in
// the Authorization header and is checked before the WebSocket upgrade.
type HelloData struct {
	ConnectorVersion string         `json:"connector_version"`
	Resources        []ResourceDecl `json:"resources"`
}

// WelcomeResource maps an announced name to its relay ID.
type WelcomeResource struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// WelcomeData answers the hello.
type WelcomeData struct {
	SiteID    string            `json:"site_id"`
	SiteName  string            `json:"site_name"`
	RelayKey  string            `json:"relay_key"` // connector compares with its pinned key
	Resources []WelcomeResource `json:"resources"`
}

// OpenData asks the connector to open a stream to a resource.
type OpenData struct {
	StreamID   string `json:"stream_id"`
	ResourceID string `json:"resource_id"`
	GrantJWT   string `json:"grant_jwt"`
	// ClientSecretHash is the hex SHA-256 of the per-process secret in the
	// agent's DSN (`tunr access`); pgproxy accepts only that password. Only
	// the hash crosses the relay.
	ClientSecretHash string `json:"client_secret_sha256,omitempty"`
}

// DataPayload carries stream bytes (same encoding as tcp_data).
type DataPayload struct {
	StreamID   string `json:"stream_id"`
	PayloadB64 string `json:"payload_b64"`
}

// CloseData ends a stream.
type CloseData struct {
	StreamID string `json:"stream_id"`
	Reason   string `json:"reason,omitempty"`
}

// AuditData is a per-query summary from the connector.
type AuditData struct {
	StreamID   string   `json:"stream_id"`
	Command    string   `json:"command"`
	Tables     []string `json:"tables,omitempty"`
	RowCount   int      `json:"row_count"`
	DurationMS int      `json:"duration_ms"`
	Blocked    bool     `json:"blocked"`
	QueryText  string   `json:"query_text,omitempty"`
}

// KillData closes every stream of a grant, or all streams when GrantID is "*".
type KillData struct {
	GrantID string `json:"grant_id"`
}

// ErrorData precedes a relay-initiated close.
type ErrorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func msg(typ string, data any) Message {
	if data == nil {
		return Message{Type: typ}
	}
	b, _ := json.Marshal(data)
	return Message{Type: typ, Data: b}
}
