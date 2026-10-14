package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// bridge.go — Agent Bridge sites, resources, agents, agent tokens and grants.
// Schema: migrations/006_bridge.sql.
//
// Every function that reads or mutates an org-owned row takes orgID and puts
// it in the WHERE clause. A row belonging to another org is ErrNotFound —
// indistinguishable from a missing one (TestBridgeTenantIsolation).

// ─── Sites ───────────────────────────────────────────────────────────────────

// Site is a customer network location served by one connector.
type Site struct {
	ID           string     `json:"id"`
	OrgID        string     `json:"org_id"`
	Name         string     `json:"name"`
	Status       string     `json:"status"`
	LastSeenAt   *time.Time `json:"last_seen_at,omitempty"`
	ConnectorVer string     `json:"connector_version,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

const siteCols = `id, org_id, name, status, last_seen_at, COALESCE(connector_ver, ''), created_at`

func scanSite(row pgx.Row) (*Site, error) {
	var s Site
	if err := row.Scan(&s.ID, &s.OrgID, &s.Name, &s.Status, &s.LastSeenAt, &s.ConnectorVer, &s.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &s, nil
}

// CreateSite stores a site with the hash of its (caller-generated) token.
func (db *DB) CreateSite(ctx context.Context, orgID, name, tokenHash string) (*Site, error) {
	return scanSite(db.pool.QueryRow(ctx, `
		INSERT INTO sites (id, org_id, name, token_hash) VALUES ($1, $2, $3, $4)
		RETURNING `+siteCols, newID("site_"), orgID, name, tokenHash))
}

// ListSites lists an org's sites.
func (db *DB) ListSites(ctx context.Context, orgID string) ([]Site, error) {
	rows, err := db.pool.Query(ctx, `SELECT `+siteCols+` FROM sites WHERE org_id = $1 ORDER BY created_at`, orgID)
	return collect(rows, err, scanSite)
}

// GetSite returns one of the org's sites.
func (db *DB) GetSite(ctx context.Context, orgID, siteID string) (*Site, error) {
	return scanSite(db.pool.QueryRow(ctx, `SELECT `+siteCols+` FROM sites WHERE id = $1 AND org_id = $2`, siteID, orgID))
}

// GetSiteByTokenHash is the connector login lookup (P2). Not org-scoped by
// design: the token *is* the credential. Disabled sites are not returned.
func (db *DB) GetSiteByTokenHash(ctx context.Context, tokenHash string) (*Site, error) {
	return scanSite(db.pool.QueryRow(ctx,
		`SELECT `+siteCols+` FROM sites WHERE token_hash = $1 AND status <> 'disabled'`, tokenHash))
}

// RotateSiteToken replaces a site's token hash; the old token stops working.
func (db *DB) RotateSiteToken(ctx context.Context, orgID, siteID, tokenHash string) error {
	return affected(db.pool.Exec(ctx,
		`UPDATE sites SET token_hash = $3 WHERE id = $1 AND org_id = $2`, siteID, orgID, tokenHash))
}

// DeleteSite deletes a site and (by cascade) its resources and their grants.
func (db *DB) DeleteSite(ctx context.Context, orgID, siteID string) error {
	return affected(db.pool.Exec(ctx, `DELETE FROM sites WHERE id = $1 AND org_id = $2`, siteID, orgID))
}

// ─── Resources ───────────────────────────────────────────────────────────────

// Resource is a named target inside a site. Its network address is never
// stored here — only the connector knows it.
type Resource struct {
	ID          string          `json:"id"`
	SiteID      string          `json:"site_id"`
	OrgID       string          `json:"org_id"`
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	Description string          `json:"description"`
	AutoApprove json.RawMessage `json:"auto_approve,omitempty"`
	Limits      json.RawMessage `json:"limits"`
	Status      string          `json:"status"`
	CreatedAt   time.Time       `json:"created_at"`
}

const resourceCols = `id, site_id, org_id, name, kind, COALESCE(description, ''), auto_approve, limits, status, created_at`

func scanResource(row pgx.Row) (*Resource, error) {
	var r Resource
	var auto, lim []byte
	if err := row.Scan(&r.ID, &r.SiteID, &r.OrgID, &r.Name, &r.Kind, &r.Description,
		&auto, &lim, &r.Status, &r.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	if auto != nil {
		r.AutoApprove = auto
	}
	r.Limits = lim
	return &r, nil
}

// UpsertResource registers a resource announced by a site's connector,
// reactivating it if it existed. orgID must own siteID.
func (db *DB) UpsertResource(ctx context.Context, orgID, siteID, name, kind string) (*Resource, error) {
	return scanResource(db.pool.QueryRow(ctx, `
		INSERT INTO resources (id, site_id, org_id, name, kind)
		SELECT $1, s.id, s.org_id, $4, $5 FROM sites s WHERE s.id = $2 AND s.org_id = $3
		ON CONFLICT (org_id, name) DO UPDATE
		  SET kind = EXCLUDED.kind, site_id = EXCLUDED.site_id,
		      status = CASE WHEN resources.status = 'disabled' THEN 'disabled' ELSE 'active' END
		RETURNING `+resourceCols, newID("res_"), siteID, orgID, name, kind))
}

// ListResources lists an org's resources.
func (db *DB) ListResources(ctx context.Context, orgID string) ([]Resource, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT `+resourceCols+` FROM resources WHERE org_id = $1 ORDER BY name`, orgID)
	return collect(rows, err, scanResource)
}

// GetResource returns one of the org's resources.
func (db *DB) GetResource(ctx context.Context, orgID, resourceID string) (*Resource, error) {
	return scanResource(db.pool.QueryRow(ctx,
		`SELECT `+resourceCols+` FROM resources WHERE id = $1 AND org_id = $2`, resourceID, orgID))
}

// ResourcePatch carries optional resource updates; nil fields are unchanged.
type ResourcePatch struct {
	Description *string
	AutoApprove json.RawMessage // nil = unchanged; JSON null clears
	Limits      json.RawMessage // nil = unchanged
	Status      *string         // active | disabled
}

// UpdateResource applies a patch to one of the org's resources.
func (db *DB) UpdateResource(ctx context.Context, orgID, resourceID string, p ResourcePatch) (*Resource, error) {
	return scanResource(db.pool.QueryRow(ctx, `
		UPDATE resources SET
		  description  = COALESCE($3, description),
		  auto_approve = CASE WHEN $4::jsonb IS NULL THEN auto_approve
		                      WHEN $4::jsonb = 'null'::jsonb THEN NULL ELSE $4::jsonb END,
		  limits       = COALESCE($5::jsonb, limits),
		  status       = COALESCE($6, status)
		WHERE id = $1 AND org_id = $2
		RETURNING `+resourceCols,
		resourceID, orgID, p.Description, rawOrNil(p.AutoApprove), rawOrNil(p.Limits), p.Status))
}

// CatalogEntry is a resource as a linked vendor sees it: name, kind and
// description — never site details or limits configuration.
type CatalogEntry struct {
	Address     string `json:"address"` // "<customer-slug>/<resource-name>"
	ResourceID  string `json:"resource_id"`
	CustomerOrg string `json:"customer_org"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

// Catalog lists active resources of every customer that has an active link
// to vendorOrgID.
func (db *DB) Catalog(ctx context.Context, vendorOrgID string) ([]CatalogEntry, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT c.slug || '/' || r.name, r.id, c.slug, r.kind, COALESCE(r.description, '')
		FROM links l
		JOIN orgs c      ON c.id = l.customer_org_id
		JOIN resources r ON r.org_id = c.id AND r.status = 'active'
		WHERE l.vendor_org_id = $1 AND l.status = 'active'
		ORDER BY 1`, vendorOrgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CatalogEntry
	for rows.Next() {
		var e CatalogEntry
		if err := rows.Scan(&e.Address, &e.ResourceID, &e.CustomerOrg, &e.Kind, &e.Description); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ─── Agents ──────────────────────────────────────────────────────────────────

// Agent is a vendor-owned identity.
type Agent struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	AppID     string    `json:"app_id,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

const agentCols = `id, org_id, name, kind, COALESCE(app_id, ''), status, created_at`

func scanAgent(row pgx.Row) (*Agent, error) {
	var a Agent
	if err := row.Scan(&a.ID, &a.OrgID, &a.Name, &a.Kind, &a.AppID, &a.Status, &a.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &a, nil
}

// CreateAgent creates an external agent in the vendor org.
func (db *DB) CreateAgent(ctx context.Context, orgID, name string) (*Agent, error) {
	return scanAgent(db.pool.QueryRow(ctx, `
		INSERT INTO agents (id, org_id, name) VALUES ($1, $2, $3) RETURNING `+agentCols,
		newID("agt_"), orgID, name))
}

// ListAgents lists the org's agents.
func (db *DB) ListAgents(ctx context.Context, orgID string) ([]Agent, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT `+agentCols+` FROM agents WHERE org_id = $1 ORDER BY created_at`, orgID)
	return collect(rows, err, scanAgent)
}

// GetAgent returns one of the org's agents.
func (db *DB) GetAgent(ctx context.Context, orgID, agentID string) (*Agent, error) {
	return scanAgent(db.pool.QueryRow(ctx,
		`SELECT `+agentCols+` FROM agents WHERE id = $1 AND org_id = $2`, agentID, orgID))
}

// SetAgentStatus enables or disables one of the org's agents.
func (db *DB) SetAgentStatus(ctx context.Context, orgID, agentID, status string) (*Agent, error) {
	return scanAgent(db.pool.QueryRow(ctx, `
		UPDATE agents SET status = $3 WHERE id = $1 AND org_id = $2 RETURNING `+agentCols,
		agentID, orgID, status))
}

// AgentToken is the record of an issued agent JWT (its jti).
type AgentToken struct {
	ID        string     `json:"id"`
	AgentID   string     `json:"agent_id"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

const agentTokenCols = `t.id, t.agent_id, t.expires_at, t.revoked_at, t.created_at`

func scanAgentToken(row pgx.Row) (*AgentToken, error) {
	var t AgentToken
	if err := row.Scan(&t.ID, &t.AgentID, &t.ExpiresAt, &t.RevokedAt, &t.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &t, nil
}

// CreateAgentToken records a new token for one of the org's active agents.
// The caller signs the JWT with the returned ID as jti.
func (db *DB) CreateAgentToken(ctx context.Context, orgID, agentID, createdBy string, expiresAt time.Time) (*AgentToken, error) {
	var by any
	if createdBy != "" {
		by = createdBy
	}
	return scanAgentToken(db.pool.QueryRow(ctx, `
		INSERT INTO agent_tokens AS t (id, agent_id, expires_at, created_by)
		SELECT $1, a.id, $4, $5::uuid FROM agents a
		WHERE a.id = $2 AND a.org_id = $3 AND a.status = 'active'
		RETURNING `+agentTokenCols, newID("atk_"), agentID, orgID, expiresAt, by))
}

// ListAgentTokens lists tokens of one of the org's agents.
func (db *DB) ListAgentTokens(ctx context.Context, orgID, agentID string) ([]AgentToken, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT `+agentTokenCols+` FROM agent_tokens t JOIN agents a ON a.id = t.agent_id
		WHERE t.agent_id = $1 AND a.org_id = $2 ORDER BY t.created_at DESC`, agentID, orgID)
	return collect(rows, err, scanAgentToken)
}

// RevokeAgentToken revokes one token of one of the org's agents.
func (db *DB) RevokeAgentToken(ctx context.Context, orgID, agentID, tokenID string) error {
	return affected(db.pool.Exec(ctx, `
		UPDATE agent_tokens t SET revoked_at = COALESCE(t.revoked_at, now())
		FROM agents a
		WHERE t.id = $1 AND t.agent_id = $2 AND a.id = t.agent_id AND a.org_id = $3`,
		tokenID, agentID, orgID))
}

// AgentTokenUsable reports whether a token (by jti) is unrevoked, unexpired,
// and belongs to an active agent of orgID. Used on every agent request; the
// JWT signature alone is not enough because tokens can be revoked.
func (db *DB) AgentTokenUsable(ctx context.Context, orgID, agentID, tokenID string) (bool, error) {
	var ok bool
	err := db.pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM agent_tokens t JOIN agents a ON a.id = t.agent_id
		  WHERE t.id = $1 AND t.agent_id = $2 AND a.org_id = $3
		    AND a.status = 'active' AND t.revoked_at IS NULL AND t.expires_at > now())`,
		tokenID, agentID, orgID).Scan(&ok)
	return ok, err
}

// ─── Grants (read side; the state machine lives in P4) ──────────────────────

// Grant is an agent's time-boxed right to a resource.
type Grant struct {
	ID            string          `json:"id"`
	AgentID       string          `json:"agent_id"`
	ResourceID    string          `json:"resource_id"`
	VendorOrgID   string          `json:"vendor_org_id"`
	CustomerOrgID string          `json:"customer_org_id"`
	Mode          string          `json:"mode"`
	Status        string          `json:"status"`
	Reason        string          `json:"reason"`
	RequestedTTL  time.Duration   `json:"-"`
	ApprovedAt    *time.Time      `json:"approved_at,omitempty"`
	ExpiresAt     *time.Time      `json:"expires_at,omitempty"`
	Limits        json.RawMessage `json:"limits"`
	CreatedAt     time.Time       `json:"created_at"`
}

const grantCols = `g.id, g.agent_id, g.resource_id, g.vendor_org_id, g.customer_org_id, g.mode, g.status,
	COALESCE(g.reason, ''), g.requested_ttl_secs, g.approved_at, g.expires_at, g.limits, g.created_at`

func scanGrant(row pgx.Row) (*Grant, error) {
	var g Grant
	var ttl int
	var lim []byte
	if err := row.Scan(&g.ID, &g.AgentID, &g.ResourceID, &g.VendorOrgID, &g.CustomerOrgID,
		&g.Mode, &g.Status, &g.Reason, &ttl, &g.ApprovedAt, &g.ExpiresAt, &lim, &g.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	g.RequestedTTL = time.Duration(ttl) * time.Second
	g.Limits = lim
	return &g, nil
}

// CreateGrantRequest inserts a pending grant for vendorOrgID's agent on a
// resource of a customer that actively links to the vendor. Returns
// ErrNotFound if the agent isn't the vendor's, the resource doesn't exist, or
// there is no active link — the caller cannot tell which, by design.
// Policy checks (TTL caps, rw rules, auto-approve) belong to P4.
func (db *DB) CreateGrantRequest(ctx context.Context, vendorOrgID, agentID, resourceID, mode, reason string, ttl time.Duration) (*Grant, error) {
	return scanGrant(db.pool.QueryRow(ctx, `
		INSERT INTO grants AS g (id, agent_id, resource_id, vendor_org_id, customer_org_id, mode, reason, requested_ttl_secs)
		SELECT $1, a.id, r.id, a.org_id, r.org_id, $5, NULLIF($6, ''), $7
		FROM agents a
		JOIN resources r ON r.id = $4 AND r.status = 'active'
		JOIN links l ON l.vendor_org_id = a.org_id AND l.customer_org_id = r.org_id AND l.status = 'active'
		WHERE a.id = $3 AND a.org_id = $2 AND a.status = 'active'
		RETURNING `+grantCols,
		newID("grt_"), vendorOrgID, agentID, resourceID, mode, reason, int(ttl/time.Second)))
}

// GetGrant returns a grant if orgID is its vendor or its customer.
func (db *DB) GetGrant(ctx context.Context, orgID, grantID string) (*Grant, error) {
	return scanGrant(db.pool.QueryRow(ctx, `
		SELECT `+grantCols+` FROM grants g
		WHERE g.id = $1 AND (g.vendor_org_id = $2 OR g.customer_org_id = $2)`, grantID, orgID))
}

// ListGrants lists grants where orgID is on the given side ("vendor" or
// "customer"), optionally filtered by status.
func (db *DB) ListGrants(ctx context.Context, orgID, side, status string) ([]Grant, error) {
	col := "g.customer_org_id"
	if side == "vendor" {
		col = "g.vendor_org_id"
	}
	rows, err := db.pool.Query(ctx, `
		SELECT `+grantCols+` FROM grants g
		WHERE `+col+` = $1 AND ($2 = '' OR g.status = $2)
		ORDER BY g.created_at DESC LIMIT 500`, orgID, status)
	return collect(rows, err, scanGrant)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// collect turns a Query result into a slice using scan.
func collect[T any](rows pgx.Rows, err error, scan func(pgx.Row) (*T, error)) ([]T, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// affected maps "0 rows changed" to ErrNotFound.
func affected(tag interface{ RowsAffected() int64 }, err error) error {
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func rawOrNil(r json.RawMessage) any {
	if r == nil {
		return nil
	}
	return string(r)
}

// ─── Connector session state (P2) ───────────────────────────────────────────

// ResourceDecl is a resource a connector announces: name and kind only.
type ResourceDecl struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// ErrResourceOwnedByOtherSite means a connector announced a resource name
// that another site of the same org already serves. Moving it silently would
// let one site hijack another's address.
var ErrResourceOwnedByOtherSite = errors.New("resource name is served by another site")

// SyncSiteResources makes the site's active resources exactly decls: each one
// is upserted (reactivated unless disabled by the customer), and any resource
// of this site not announced is marked offline. Atomic.
func (db *DB) SyncSiteResources(ctx context.Context, orgID, siteID string, decls []ResourceDecl) ([]Resource, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	names := make([]string, 0, len(decls))
	out := make([]Resource, 0, len(decls))
	for _, d := range decls {
		var other string
		err := tx.QueryRow(ctx,
			`SELECT site_id FROM resources WHERE org_id = $1 AND name = $2`, orgID, d.Name).Scan(&other)
		if err == nil && other != siteID {
			return nil, fmt.Errorf("%w: %s", ErrResourceOwnedByOtherSite, d.Name)
		}
		r, err := scanResource(tx.QueryRow(ctx, `
			INSERT INTO resources (id, site_id, org_id, name, kind)
			SELECT $1, s.id, s.org_id, $4, $5 FROM sites s WHERE s.id = $2 AND s.org_id = $3
			ON CONFLICT (org_id, name) DO UPDATE
			  SET kind = EXCLUDED.kind,
			      status = CASE WHEN resources.status = 'disabled' THEN 'disabled' ELSE 'active' END
			  WHERE resources.site_id = EXCLUDED.site_id
			RETURNING `+resourceCols, newID("res_"), siteID, orgID, d.Name, d.Kind))
		if err != nil {
			return nil, err
		}
		names = append(names, d.Name)
		out = append(out, *r)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE resources SET status = 'offline'
		WHERE site_id = $1 AND org_id = $2 AND status = 'active' AND NOT (name = ANY($3))`,
		siteID, orgID, names); err != nil {
		return nil, err
	}
	return out, mapErr(tx.Commit(ctx))
}

// SetSiteOnline records a connector session start.
func (db *DB) SetSiteOnline(ctx context.Context, siteID, connectorVer string) error {
	return affected(db.pool.Exec(ctx, `
		UPDATE sites SET status = 'online', last_seen_at = now(), connector_ver = NULLIF($2, '')
		WHERE id = $1 AND status <> 'disabled'`, siteID, connectorVer))
}

// SetSiteOffline records a connector session end. A disabled site stays disabled.
func (db *DB) SetSiteOffline(ctx context.Context, siteID string) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE sites SET status = 'offline', last_seen_at = now()
		WHERE id = $1 AND status = 'online'`, siteID)
	return err
}
