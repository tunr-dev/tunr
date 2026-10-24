package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tunr-dev/tunr/internal/bridge/access"
	"github.com/tunr-dev/tunr/internal/bridge/api"
	"github.com/tunr-dev/tunr/internal/bridge/connector"
	"github.com/tunr-dev/tunr/internal/bridge/profile"
	"github.com/tunr-dev/tunr/internal/bridge/query"
	"github.com/tunr-dev/tunr/internal/logger"
	"github.com/tunr-dev/tunr/internal/mcp"
)

// bridge_mcp.go — mcp.BridgeTools backed by the relay API and local
// `tunr access` listeners living inside the MCP server process.

type bridgeMCP struct {
	relay  string
	tokens *api.TokenSource
	api    *api.Client
	root   context.Context

	mu   sync.Mutex
	open map[string]*openListener
}

type openListener struct {
	cancel context.CancelFunc
	info   mcp.OpenedAccess
	qs     *access.QuerySession // mssql

	pgMu sync.Mutex
	pg   *pgconn.PgConn // postgres, for tunr_query; opened on first use
}

func newBridgeMCP(ctx context.Context, relay string, tokens *api.TokenSource) (*bridgeMCP, error) {
	if err := connector.ValidateRelay(relay); err != nil {
		return nil, err
	}
	return &bridgeMCP{relay: relay, tokens: tokens, api: &api.Client{Relay: relay, Tokens: tokens}, root: ctx,
		open: map[string]*openListener{}}, nil
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

	cl := &access.Client{Relay: b.relay, TokenFunc: b.tokens.Token, Resource: resource, ClientSecret: access.NewClientSecret(),
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
	cl.SameUserOnly = chk.Kind == "tcp"
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return mcp.OpenedAccess{}, err
	}
	sctx, cancel := context.WithCancel(b.root)
	ol := &openListener{cancel: cancel}
	if chk.Kind == "mssql" {
		ol.qs = cl.NewQuerySession()
		go func() { _ = access.ServeQuery(sctx, l, ol.qs, cl.ClientSecret) }()
	} else {
		go func() { _ = cl.Serve(sctx, l) }()
	}

	info := mcp.OpenedAccess{Resource: resource, Kind: chk.Kind, Mode: chk.Mode, LocalAddress: l.Addr().String(),
		GrantID: chk.GrantID, GrantExpiresAt: chk.GrantExpiresAt}
	if chk.Kind == "postgres" {
		info.DSN = access.DSN(info.LocalAddress, cl.ClientSecret)
		info.Hint = "Use this DSN with psql, psycopg or pgx. Read-only: write statements fail with SQLSTATE 42501. " +
			"Every query is summarised for the customer."
	}
	if chk.Kind == "mssql" {
		info.QueryURL = "http://" + info.LocalAddress + "/query"
		info.QueryToken = cl.ClientSecret
		info.Hint = "Simplest: call tunr_query. From code: POST " + info.QueryURL + ` with {"sql": "SELECT …"} and ` +
			"Authorization: Bearer <query_token>. T-SQL, read-only (SELECT/WITH); every statement is summarised for the customer."
	}
	if chk.Kind == "tcp" {
		info.Hint = "Connect your client to " + info.LocalAddress + "; bytes go to the customer's resource unchanged."
	}
	ol.info = info
	b.mu.Lock()
	b.open[resource] = ol
	b.mu.Unlock()
	return info, nil
}

// DescribeResource returns the resource's catalog entry with its schema
// profile rendered for an agent.
func (b *bridgeMCP) DescribeResource(ctx context.Context, resource string) (any, error) {
	items, err := b.api.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if it.Address != resource {
			continue
		}
		out := map[string]any{"resource": it.Address, "kind": it.Kind, "grant_status": it.GrantStatus}
		if it.Description != "" {
			out["customer_note"] = it.Description
		}
		explore := map[string]string{
			"postgres": "SELECT table_schema, table_name FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog','information_schema')",
			"mssql":    "SELECT TABLE_SCHEMA, TABLE_NAME FROM INFORMATION_SCHEMA.TABLES",
		}[it.Kind]
		if it.Profile == "" {
			out["profile"] = nil
			if explore != "" {
				out["hint"] = "No schema profile attached by the customer. Once access is approved, list tables with tunr_query: " + explore
			}
			return out, nil
		}
		p, ok := profile.Get(it.Profile)
		if !ok {
			out["profile"] = it.Profile
			out["hint"] = "This tunr version doesn't know profile " + it.Profile + "; update tunr. Meanwhile explore with: " + explore
			return out, nil
		}
		var verified []string
		if it.ProfileVerifiedAt != nil {
			verified = append([]string{}, it.ProfileTables...)
		}
		out["profile"] = p.Describe(verified, it.ProfileVerifiedAt)
		return out, nil
	}
	return nil, fmt.Errorf("no resource %s in your catalog (tunr_list_resources shows what the customer shares)", resource)
}

// Query runs one statement, opening access first if needed.
func (b *bridgeMCP) Query(ctx context.Context, resource, sql string, maxRows int) (*query.Response, error) {
	if _, err := b.OpenAccess(ctx, resource, 0); err != nil {
		return nil, err
	}
	b.mu.Lock()
	ol := b.open[resource]
	b.mu.Unlock()
	if ol == nil {
		return nil, fmt.Errorf("access to %s was closed", resource)
	}
	switch ol.info.Kind {
	case "mssql":
		return ol.qs.Query(ctx, sql, maxRows)
	case "postgres":
		return ol.pgQuery(ctx, sql, maxRows)
	}
	return nil, fmt.Errorf("tunr_query works on postgres and mssql resources; %s is %s — use tunr_open_access", resource, ol.info.Kind)
}

var pgTypes = pgtype.NewMap()

func (ol *openListener) pgQuery(ctx context.Context, sql string, maxRows int) (*query.Response, error) {
	ol.pgMu.Lock()
	defer ol.pgMu.Unlock()
	if ol.pg == nil || ol.pg.IsClosed() {
		c, err := pgconn.Connect(ctx, ol.info.DSN)
		if err != nil {
			return nil, fmt.Errorf("connect through the bridge: %w", err)
		}
		ol.pg = c
	}
	start := time.Now()
	resp := &query.Response{ID: "1"}
	mrr := ol.pg.Exec(ctx, sql)
	for mrr.NextResult() {
		rr := mrr.ResultReader()
		rs := query.ResultSet{Rows: [][]any{}}
		for _, fd := range rr.FieldDescriptions() {
			typ := strconv.FormatUint(uint64(fd.DataTypeOID), 10)
			if t, ok := pgTypes.TypeForOID(fd.DataTypeOID); ok {
				typ = t.Name
			}
			rs.Columns = append(rs.Columns, query.Column{Name: fd.Name, Type: typ})
		}
		for rr.NextRow() {
			if maxRows > 0 && resp.RowCount >= maxRows {
				resp.Truncated = true
				continue // drain; the rows are discarded
			}
			vals := rr.Values()
			row := make([]any, len(vals))
			for i, v := range vals {
				if v != nil {
					row[i] = string(v)
				}
			}
			rs.Rows = append(rs.Rows, row)
			resp.RowCount++
		}
		if _, err := rr.Close(); err != nil {
			resp.Error = pgError(err)
		}
		if len(rs.Columns) > 0 {
			resp.ResultSets = append(resp.ResultSets, rs)
		}
	}
	if err := mrr.Close(); err != nil && resp.Error == nil {
		resp.Error = pgError(err)
	}
	if resp.Error != nil && ol.pg.IsClosed() {
		ol.pg = nil
	}
	if resp.Truncated {
		resp.Notice = fmt.Sprintf("showing the first %d rows; narrow the query or aggregate", maxRows)
	}
	resp.DurationMS = int(time.Since(start).Milliseconds())
	return resp, nil
}

func pgError(err error) *query.Error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		code := "db_error"
		if pe.Code == "42501" {
			code = "blocked"
		}
		msg := pe.Message
		if pe.Hint != "" {
			msg += " (" + pe.Hint + ")"
		}
		return &query.Error{Code: code, Message: "SQLSTATE " + pe.Code + ": " + msg}
	}
	return &query.Error{Code: "bridge", Message: err.Error()}
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
	cur.pgMu.Lock()
	if cur.pg != nil {
		_ = cur.pg.Close(context.Background())
	}
	cur.pgMu.Unlock()
	return nil
}
