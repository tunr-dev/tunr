package bridge

import "encoding/json"

// wire.go — CLI copy of the bridge wire types. The relay's copy is
// relay/internal/bridge/wire.go; the contract is docs/protocol/bridge.md.
// Change both together. Unknown message types must be ignored.

// Message is the envelope ({type, data}), same shape as the tunnel protocol.
type Message struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Message types.
const (
	MsgHello   = "bridge_hello"
	MsgWelcome = "bridge_welcome"
	MsgOpen    = "bridge_open"
	MsgData    = "bridge_data"
	MsgClose   = "bridge_close"
	MsgAudit   = "bridge_audit"
	MsgKill    = "bridge_kill"
	MsgPing    = "ping"
	MsgPong    = "pong"
	MsgError   = "error"
)

// ResourceDecl is a resource as announced to the relay: name, kind and
// optionally its schema profile. There is deliberately no address field —
// targets never leave the site.
type ResourceDecl struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Profile names a built-in schema profile ("nebim-v3"); ProfileTables
	// are the profile's tables found in the real database by
	// `tunr connect resource verify`. Older relays ignore both.
	Profile           string   `json:"profile,omitempty"`
	ProfileTables     []string `json:"profile_tables,omitempty"`
	ProfileVerifiedAt string   `json:"profile_verified_at,omitempty"` // RFC 3339
}

// HelloData is the connector's first message.
type HelloData struct {
	ConnectorVersion string         `json:"connector_version"`
	Resources        []ResourceDecl `json:"resources"`
	// TLSCertSHA256 announces end-to-end TLS support (hex SHA-256 of the
	// connector's certificate). Empty: an older connector, no E2E.
	TLSCertSHA256 string `json:"tls_cert_sha256,omitempty"`
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
	RelayKey  string            `json:"relay_key"`
	Resources []WelcomeResource `json:"resources"`
}

// OpenData asks the connector to open a stream.
type OpenData struct {
	StreamID   string `json:"stream_id"`
	ResourceID string `json:"resource_id"`
	GrantJWT   string `json:"grant_jwt"`
	// ClientSecretHash is the hex SHA-256 of the per-process secret in the
	// agent's DSN (`tunr access`); pgproxy accepts only that password. Only
	// the hash crosses the relay.
	ClientSecretHash string `json:"client_secret_sha256,omitempty"`
	// E2E: the agent will start a TLS handshake with the connector inside
	// this stream; the connector terminates it with its site certificate.
	E2E bool `json:"e2e,omitempty"`
}

// DataPayload carries stream bytes.
type DataPayload struct {
	StreamID   string `json:"stream_id"`
	PayloadB64 string `json:"payload_b64"`
}

// CloseData ends a stream.
type CloseData struct {
	StreamID string `json:"stream_id"`
	Reason   string `json:"reason,omitempty"`
}

// AuditData is a per-query summary.
type AuditData struct {
	StreamID   string   `json:"stream_id"`
	Command    string   `json:"command"`
	Tables     []string `json:"tables,omitempty"`
	RowCount   int      `json:"row_count"`
	DurationMS int      `json:"duration_ms"`
	Blocked    bool     `json:"blocked"`
	QueryText  string   `json:"query_text,omitempty"`
}

// KillData closes a grant's streams ("*" = all).
type KillData struct {
	GrantID string `json:"grant_id"`
}

// ErrorData precedes a relay-initiated close.
type ErrorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// NewMessage builds an envelope.
func NewMessage(typ string, data any) Message {
	if data == nil {
		return Message{Type: typ}
	}
	b, _ := json.Marshal(data)
	return Message{Type: typ, Data: b}
}
