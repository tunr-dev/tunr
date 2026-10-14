package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"

	"github.com/tunr-dev/tunr/internal/bridge/access"
	"github.com/tunr-dev/tunr/internal/bridge/api"
	"github.com/tunr-dev/tunr/internal/bridge/connector"
	"github.com/tunr-dev/tunr/internal/logger"
	"github.com/tunr-dev/tunr/internal/mcp"
)

// bridge_mcp.go — mcp.BridgeTools backed by the relay API and local
// `tunr access` listeners living inside the MCP server process.

type bridgeMCP struct {
	relay, token string
	api          *api.Client
	root         context.Context

	mu   sync.Mutex
	open map[string]openListener
}

type openListener struct {
	cancel context.CancelFunc
	info   mcp.OpenedAccess
}

func newBridgeMCP(ctx context.Context, relay, token string) (*bridgeMCP, error) {
	if err := connector.ValidateRelay(relay); err != nil {
		return nil, err
	}
	return &bridgeMCP{relay: relay, token: token, api: &api.Client{Relay: relay, Token: token}, root: ctx,
		open: map[string]openListener{}}, nil
}

func toMCPGrant(g *api.Grant) mcp.BridgeGrant {
	return mcp.BridgeGrant{ID: g.ID, Address: g.Address, Mode: g.Mode, Status: g.Status,
		WaitingForApprover: g.Status == "pending" && g.FirstApproved, ExpiresAt: g.ExpiresAt}
}

func (b *bridgeMCP) ListResources(ctx context.Context) ([]mcp.BridgeResource, error) {
	items, err := b.api.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.BridgeResource, 0, len(items))
	for _, it := range items {
		out = append(out, mcp.BridgeResource{Address: it.Address, Kind: it.Kind, Description: it.Description,
			GrantID: it.GrantID, GrantStatus: it.GrantStatus, GrantMode: it.GrantMode, GrantExpiresAt: it.GrantExpiresAt})
	}
	return out, nil
}

func (b *bridgeMCP) RequestAccess(ctx context.Context, resource, mode, ttl, reason string) (mcp.BridgeGrant, error) {
	g, err := b.api.RequestGrant(ctx, api.GrantRequest{Resource: resource, Mode: mode, TTL: ttl, Reason: reason})
	if err != nil {
		return mcp.BridgeGrant{}, err
	}
	return toMCPGrant(g), nil
}

func (b *bridgeMCP) AccessStatus(ctx context.Context, grantID string) (mcp.BridgeGrant, error) {
	g, err := b.api.AgentGrant(ctx, grantID)
	if err != nil {
		return mcp.BridgeGrant{}, err
	}
	return toMCPGrant(g), nil
}

func (b *bridgeMCP) OpenAccess(ctx context.Context, resource string, port int) (mcp.OpenedAccess, error) {
	b.mu.Lock()
	if cur, ok := b.open[resource]; ok {
		b.mu.Unlock()
		return cur.info, nil
	}
	b.mu.Unlock()

	cl := &access.Client{Relay: b.relay, Token: b.token, Resource: resource,
		Logf: func(f string, a ...any) { logger.Info(f, a...) }}
	chk, err := cl.Check(ctx)
	if err != nil {
		if ae, ok := err.(*access.Error); ok {
			if adv := ae.Advice(resource); adv != "" {
				return mcp.OpenedAccess{}, fmt.Errorf("%s: %s (%s)", ae.Code, ae.Message, adv)
			}
		}
		return mcp.OpenedAccess{}, err
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return mcp.OpenedAccess{}, err
	}
	sctx, cancel := context.WithCancel(b.root)
	go func() { _ = cl.Serve(sctx, l) }()

	info := mcp.OpenedAccess{Resource: resource, Kind: chk.Kind, Mode: chk.Mode, LocalAddress: l.Addr().String(),
		GrantID: chk.GrantID, GrantExpiresAt: chk.GrantExpiresAt}
	if chk.Kind == "tcp" {
		info.Hint = "Connect your client to " + info.LocalAddress + "; bytes go to the customer's resource unchanged."
	}
	b.mu.Lock()
	b.open[resource] = openListener{cancel: cancel, info: info}
	b.mu.Unlock()
	return info, nil
}

func (b *bridgeMCP) CloseAccess(resource string) error {
	b.mu.Lock()
	cur, ok := b.open[resource]
	delete(b.open, resource)
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("no open access to %s", resource)
	}
	cur.cancel()
	return nil
}
