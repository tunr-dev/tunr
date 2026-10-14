package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// bridge.go — Agent Bridge tools (P4-T5). They let an agent discover a
// customer's resources, ask for time-boxed access with a reason the customer
// reads, wait for the decision, and open a local port to the resource.
//
// Identity is the agent token (TUNR_AGENT_TOKEN in the MCP server's env),
// not the user's login: the customer approves *this agent*.

// BridgeResource is a catalog entry.
type BridgeResource struct {
	Address        string     `json:"address"`
	Kind           string     `json:"kind"`
	Description    string     `json:"description,omitempty"`
	GrantID        string     `json:"grant_id,omitempty"`
	GrantStatus    string     `json:"grant_status,omitempty"`
	GrantMode      string     `json:"grant_mode,omitempty"`
	GrantExpiresAt *time.Time `json:"grant_expires_at,omitempty"`
}

// BridgeGrant is a grant as the agent sees it.
type BridgeGrant struct {
	ID                 string     `json:"grant_id"`
	Address            string     `json:"resource"`
	Mode               string     `json:"mode"`
	Status             string     `json:"status"`
	WaitingForApprover bool       `json:"waiting_for_second_approver,omitempty"`
	ExpiresAt          *time.Time `json:"expires_at,omitempty"`
}

// OpenedAccess describes a local port opened to a resource.
type OpenedAccess struct {
	Resource       string    `json:"resource"`
	Kind           string    `json:"kind"`
	Mode           string    `json:"mode"`
	LocalAddress   string    `json:"local_address"`
	GrantID        string    `json:"grant_id"`
	GrantExpiresAt time.Time `json:"grant_expires_at"`
	Hint           string    `json:"hint,omitempty"`
}

// BridgeTools is implemented by the CLI (cmd/tunr/bridge_mcp.go).
type BridgeTools interface {
	ListResources(ctx context.Context) ([]BridgeResource, error)
	RequestAccess(ctx context.Context, resource, mode, ttl, reason string) (BridgeGrant, error)
	AccessStatus(ctx context.Context, grantID string) (BridgeGrant, error)
	OpenAccess(ctx context.Context, resource string, localPort int) (OpenedAccess, error)
	CloseAccess(resource string) error
}

// WithBridge wires the Agent Bridge tools.
func (s *Server) WithBridge(b BridgeTools) *Server {
	s.bridge = b
	return s
}

const bridgeRule = " Access is granted per agent, per resource, for a limited time, and every " +
	"connection is logged for the customer. Postgres resources are read-only: write " +
	"statements are rejected."

func bridgeToolList() []map[string]interface{} {
	str := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	return []map[string]interface{}{
		{
			"name": "tunr_list_resources",
			"description": "List the customer systems (databases, servers) this agent may request access to " +
				"through the tunr Agent Bridge, with the current grant status for each. Call this first to " +
				"find the resource address (\"customer/name\")." + bridgeRule,
			"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		},
		{
			"name": "tunr_request_access",
			"description": "Ask a customer for time-limited access to one of their resources. The customer " +
				"reads the reason before approving: say plainly what you will read and why, in one or two " +
				"sentences. Returns status \"pending\" until a person approves; poll tunr_access_status or tell " +
				"the user you are waiting. Do not request more time than the task needs." + bridgeRule,
			"inputSchema": map[string]interface{}{
				"type":     "object",
				"required": []string{"resource", "reason"},
				"properties": map[string]interface{}{
					"resource": str("Resource address from tunr_list_resources, e.g. \"acme/erp-db\""),
					"reason":   str("Why you need it — shown to the customer (max 500 chars)"),
					"ttl":      str("How long, e.g. \"30m\" or \"2h\" (default 1h)"),
					"mode":     str("Leave empty: read-only for databases; raw tcp resources need rw and two customer approvers"),
				},
			},
		},
		{
			"name":        "tunr_access_status",
			"description": "Check whether a grant has been approved, denied, revoked or has expired.",
			"inputSchema": map[string]interface{}{
				"type":       "object",
				"required":   []string{"grant_id"},
				"properties": map[string]interface{}{"grant_id": str("Grant ID returned by tunr_request_access")},
			},
		},
		{
			"name": "tunr_open_access",
			"description": "Open a local port (127.0.0.1) that reaches an approved resource through the bridge, " +
				"and return the address to connect to. The resource's real address is never revealed. Fails " +
				"with a clear reason if there is no approved grant or the customer's connector is offline." + bridgeRule,
			"inputSchema": map[string]interface{}{
				"type":     "object",
				"required": []string{"resource"},
				"properties": map[string]interface{}{
					"resource":   str("Resource address, e.g. \"acme/erp-db\""),
					"local_port": map[string]interface{}{"type": "integer", "description": "Local port (default: any free port)"},
				},
			},
		},
		{
			"name":        "tunr_close_access",
			"description": "Close the local port opened by tunr_open_access when you are done with a resource.",
			"inputSchema": map[string]interface{}{
				"type":       "object",
				"required":   []string{"resource"},
				"properties": map[string]interface{}{"resource": str("Resource address")},
			},
		},
	}
}

func (s *Server) bridgeTool(id interface{}, name string, args json.RawMessage) bool {
	switch name {
	case "tunr_list_resources", "tunr_request_access", "tunr_access_status", "tunr_open_access", "tunr_close_access":
	default:
		return false
	}
	if s.bridge == nil {
		s.sendToolError(id, "Agent Bridge is not configured: set TUNR_AGENT_TOKEN in this MCP server's environment "+
			"(create one with `tunr agent token <name> --org <vendor-org>`).")
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var in struct {
		Resource  string `json:"resource"`
		Reason    string `json:"reason"`
		TTL       string `json:"ttl"`
		Mode      string `json:"mode"`
		GrantID   string `json:"grant_id"`
		LocalPort int    `json:"local_port"`
	}
	if len(args) > 0 && json.Unmarshal(args, &in) != nil {
		s.sendToolError(id, "invalid arguments")
		return true
	}
	reply := func(v any, err error) {
		if err != nil {
			s.sendToolError(id, err.Error())
			return
		}
		b, _ := json.MarshalIndent(v, "", "  ")
		s.sendToolResult(id, string(b))
	}
	switch name {
	case "tunr_list_resources":
		reply(s.bridge.ListResources(ctx))
	case "tunr_request_access":
		if in.Resource == "" || strings.TrimSpace(in.Reason) == "" {
			s.sendToolError(id, "resource and reason are required")
			return true
		}
		if in.TTL == "" {
			in.TTL = "1h"
		}
		reply(s.bridge.RequestAccess(ctx, in.Resource, in.Mode, in.TTL, in.Reason))
	case "tunr_access_status":
		if in.GrantID == "" {
			s.sendToolError(id, "grant_id is required")
			return true
		}
		reply(s.bridge.AccessStatus(ctx, in.GrantID))
	case "tunr_open_access":
		if in.Resource == "" {
			s.sendToolError(id, "resource is required")
			return true
		}
		if in.LocalPort != 0 && (in.LocalPort < 1024 || in.LocalPort > 65535) {
			s.sendToolError(id, fmt.Sprintf("invalid local_port %d (1024-65535)", in.LocalPort))
			return true
		}
		reply(s.bridge.OpenAccess(ctx, in.Resource, in.LocalPort))
	case "tunr_close_access":
		if err := s.bridge.CloseAccess(in.Resource); err != nil {
			s.sendToolError(id, err.Error())
			return true
		}
		s.sendToolResult(id, "closed "+in.Resource)
	}
	return true
}
