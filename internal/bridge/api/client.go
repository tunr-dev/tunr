// Package api is a small client for the relay's /api/v1/bridge endpoints,
// used by the CLI and the MCP tools. It speaks with either a human token
// (management) or an agent token (/agent/…), never logs either, and turns
// relay refusals into *Error with the machine code.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Error is a relay refusal.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Client calls the bridge API.
type Client struct {
	Relay  string
	Token  string
	Tokens *TokenSource // when set, used instead of Token (hosted agents)
	HTTP   *http.Client
}

// Do sends a request; body and out may be nil.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Relay, "/")+"/api/v1/bridge"+path, rd)
	if err != nil {
		return err
	}
	tok := c.Token
	if c.Tokens != nil {
		t, err := c.Tokens.Token(ctx)
		if err != nil {
			return err
		}
		tok = t
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		var e struct{ Error, Message string }
		if json.Unmarshal(raw, &e) != nil || e.Error == "" {
			return &Error{Status: resp.StatusCode, Code: fmt.Sprintf("http_%d", resp.StatusCode), Message: strings.TrimSpace(string(raw))}
		}
		return &Error{Status: resp.StatusCode, Code: e.Error, Message: e.Message}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Grant mirrors the relay's GrantView (the fields the CLI shows).
type Grant struct {
	ID            string     `json:"id"`
	AgentID       string     `json:"agent_id"`
	AgentName     string     `json:"agent_name"`
	VendorSlug    string     `json:"vendor_slug"`
	Address       string     `json:"address"`
	ResourceKind  string     `json:"resource_kind"`
	Mode          string     `json:"mode"`
	Status        string     `json:"status"`
	Reason        string     `json:"reason"`
	RequestedTTL  int        `json:"requested_ttl_secs"`
	FirstApproved bool       `json:"first_approval_recorded"`
	ApprovedAt    *time.Time `json:"approved_at"`
	ExpiresAt     *time.Time `json:"expires_at"`
	CreatedAt     time.Time  `json:"created_at"`
}

// CatalogItem mirrors an /agent/catalog entry.
type CatalogItem struct {
	Address        string     `json:"address"`
	Kind           string     `json:"kind"`
	Description    string     `json:"description"`
	GrantID        string     `json:"grant_id"`
	GrantStatus    string     `json:"grant_status"`
	GrantMode      string     `json:"grant_mode"`
	GrantExpiresAt *time.Time `json:"grant_expires_at"`
}

// GrantRequest is POST /agent/grants (and /orgs/{org}/grants with AgentID).
type GrantRequest struct {
	Resource string `json:"resource"`
	Mode     string `json:"mode,omitempty"`
	TTL      string `json:"ttl"`
	Reason   string `json:"reason"`
	AgentID  string `json:"agent_id,omitempty"`
}

// Catalog lists the resources this agent can ask for.
func (c *Client) Catalog(ctx context.Context) ([]CatalogItem, error) {
	var out struct{ Resources []CatalogItem }
	return out.Resources, c.Do(ctx, http.MethodGet, "/agent/catalog", nil, &out)
}

// RequestGrant asks for access as the calling agent.
func (c *Client) RequestGrant(ctx context.Context, r GrantRequest) (*Grant, error) {
	var g Grant
	return &g, c.Do(ctx, http.MethodPost, "/agent/grants", r, &g)
}

// AgentGrants lists the calling agent's grants.
func (c *Client) AgentGrants(ctx context.Context, status string) ([]Grant, error) {
	var out struct{ Grants []Grant }
	path := "/agent/grants"
	if status != "" {
		path += "?status=" + status
	}
	return out.Grants, c.Do(ctx, http.MethodGet, path, nil, &out)
}

// AgentGrant returns one of the calling agent's grants.
func (c *Client) AgentGrant(ctx context.Context, id string) (*Grant, error) {
	var g Grant
	return &g, c.Do(ctx, http.MethodGet, "/agent/grants/"+id, nil, &g)
}
