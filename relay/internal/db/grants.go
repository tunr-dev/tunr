package db

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// grants.go — grant state machine and read models (P4).
//
//	pending → approved | denied
//	approved → revoked | expired
//
// No other transition exists. rw grants need two different approvers: the
// first approval is recorded in approved_by and the grant stays pending.

// Errors for the state machine.
var (
	ErrInvalidTransition = errors.New("grant is not in a state that allows this")
	ErrSameApprover      = errors.New("an rw grant needs a second, different approver")
)

// GrantView is a grant with the names a human needs to decide on it.
type GrantView struct {
	Grant
	AgentName       string `json:"agent_name"`
	VendorSlug      string `json:"vendor_slug"`
	CustomerSlug    string `json:"customer_slug"`
	ResourceName    string `json:"resource_name"`
	ResourceKind    string `json:"resource_kind"`
	SiteID          string `json:"site_id"`
	Address         string `json:"address"` // "<customer>/<resource>"
	RequestedTTLSec int    `json:"requested_ttl_secs"`
	FirstApproved   bool   `json:"first_approval_recorded"` // rw: waiting for the second approver
}

const grantViewSelect = `
	SELECT ` + grantCols + `,
	       a.name, v.slug, c.slug, r.name, r.kind, r.site_id, g.approved_by IS NOT NULL
	FROM grants g
	JOIN agents a    ON a.id = g.agent_id
	JOIN orgs v      ON v.id = g.vendor_org_id
	JOIN orgs c      ON c.id = g.customer_org_id
	JOIN resources r ON r.id = g.resource_id`

func scanGrantView(row pgx.Row) (*GrantView, error) {
	var v GrantView
	var ttl int
	var lim []byte
	g := &v.Grant
	if err := row.Scan(&g.ID, &g.AgentID, &g.ResourceID, &g.VendorOrgID, &g.CustomerOrgID,
		&g.Mode, &g.Status, &g.Reason, &ttl, &g.ApprovedAt, &g.ExpiresAt, &lim, &g.CreatedAt,
		&v.AgentName, &v.VendorSlug, &v.CustomerSlug, &v.ResourceName, &v.ResourceKind, &v.SiteID,
		&v.FirstApproved); err != nil {
		return nil, mapErr(err)
	}
	g.RequestedTTL = time.Duration(ttl) * time.Second
	g.Limits = lim
	v.RequestedTTLSec = ttl
	v.Address = v.CustomerSlug + "/" + v.ResourceName
	if g.Status != "pending" {
		v.FirstApproved = false
	}
	return &v, nil
}

// GetGrantView returns a grant visible to orgID (either side).
func (db *DB) GetGrantView(ctx context.Context, orgID, grantID string) (*GrantView, error) {
	return scanGrantView(db.pool.QueryRow(ctx, grantViewSelect+`
		WHERE g.id = $1 AND (g.vendor_org_id = $2 OR g.customer_org_id = $2)`, grantID, orgID))
}

// GrantFilter narrows ListGrantViews.
type GrantFilter struct {
	Side    string // "vendor" | "customer" (required)
	Status  string // optional
	AgentID string // optional (vendor side)
}

// ListGrantViews lists grants on one side of orgID, newest first.
func (db *DB) ListGrantViews(ctx context.Context, orgID string, f GrantFilter) ([]GrantView, error) {
	col := "g.customer_org_id"
	if f.Side == "vendor" {
		col = "g.vendor_org_id"
	}
	rows, err := db.pool.Query(ctx, grantViewSelect+`
		WHERE `+col+` = $1 AND ($2 = '' OR g.status = $2) AND ($3 = '' OR g.agent_id = $3)
		ORDER BY (g.status = 'pending') DESC, g.created_at DESC LIMIT 500`, orgID, f.Status, f.AgentID)
	return collect(rows, err, scanGrantView)
}

// NewGrant is a validated grant request.
type NewGrant struct {
	VendorOrgID, AgentID, ResourceID string
	Mode, Reason                     string
	TTL                              time.Duration
	Limits                           json.RawMessage // already validated; nil = none
	AutoApprove                      bool            // policy decided by the caller
}

// CreateGrant inserts a grant request (pending, or approved at once when
// AutoApprove). Same visibility rules as CreateGrantRequest.
func (db *DB) CreateGrant(ctx context.Context, n NewGrant) (*GrantView, error) {
	lim := "{}"
	if n.Limits != nil {
		lim = string(n.Limits)
	}
	status := "pending"
	if n.AutoApprove {
		status = "approved"
	}
	var id string
	err := db.pool.QueryRow(ctx, `
		INSERT INTO grants AS g (id, agent_id, resource_id, vendor_org_id, customer_org_id, mode, reason,
		                         requested_ttl_secs, limits, status, approved_at, expires_at)
		SELECT $1, a.id, r.id, a.org_id, r.org_id, $5, NULLIF($6, ''), $7::int, $8::jsonb, $9::text,
		       CASE WHEN $9::text = 'approved' THEN now() END,
		       CASE WHEN $9::text = 'approved' THEN now() + make_interval(secs => $7::int) END
		FROM agents a
		JOIN resources r ON r.id = $4 AND r.status = 'active'
		JOIN links l ON l.vendor_org_id = a.org_id AND l.customer_org_id = r.org_id AND l.status = 'active'
		WHERE a.id = $3 AND a.org_id = $2 AND a.status = 'active'
		RETURNING g.id`,
		newID("grt_"), n.VendorOrgID, n.AgentID, n.ResourceID, n.Mode, n.Reason,
		int(n.TTL/time.Second), lim, status).Scan(&id)
	if err != nil {
		return nil, mapErr(err)
	}
	return db.GetGrantView(ctx, n.VendorOrgID, id)
}

// ApproveGrant records an approval by userID on behalf of the customer org.
// For rw the first call records the approver and returns approved=false; a
// second call by a different user approves. expires_at counts from approval.
func (db *DB) ApproveGrant(ctx context.Context, customerOrgID, grantID, userID string) (*GrantView, bool, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status, mode string
	var first *string
	err = tx.QueryRow(ctx, `
		SELECT status, mode, approved_by::text FROM grants
		WHERE id = $1 AND customer_org_id = $2 FOR UPDATE`, grantID, customerOrgID).Scan(&status, &mode, &first)
	if err != nil {
		return nil, false, mapErr(err)
	}
	if status != "pending" {
		return nil, false, ErrInvalidTransition
	}
	approved := true
	switch {
	case mode == "rw" && first == nil:
		approved = false
		_, err = tx.Exec(ctx, `UPDATE grants SET approved_by = $2::uuid, updated_at = now() WHERE id = $1`, grantID, userID)
	case mode == "rw" && *first == userID:
		return nil, false, ErrSameApprover
	case mode == "rw":
		_, err = tx.Exec(ctx, `
			UPDATE grants SET second_approver = $2::uuid, status = 'approved', approved_at = now(),
			       expires_at = now() + make_interval(secs => requested_ttl_secs), updated_at = now()
			WHERE id = $1`, grantID, userID)
	default:
		_, err = tx.Exec(ctx, `
			UPDATE grants SET approved_by = $2::uuid, status = 'approved', approved_at = now(),
			       expires_at = now() + make_interval(secs => requested_ttl_secs), updated_at = now()
			WHERE id = $1`, grantID, userID)
	}
	if err != nil {
		return nil, false, mapErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	v, err := db.GetGrantView(ctx, customerOrgID, grantID)
	return v, approved, err
}

// transition moves a grant from one of `from` to `to`, scoped to orgCond.
func (db *DB) transition(ctx context.Context, grantID, orgCond, orgID, to string, from ...string) (*GrantView, error) {
	var id string
	err := db.pool.QueryRow(ctx, `
		UPDATE grants g SET status = $3, updated_at = now()
		WHERE g.id = $1 AND `+orgCond+` AND g.status = ANY($4)
		RETURNING g.id`, grantID, orgID, to, from).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Distinguish "not yours / missing" from "wrong state".
		if _, gerr := db.GetGrantView(ctx, orgID, grantID); gerr != nil {
			return nil, gerr
		}
		return nil, ErrInvalidTransition
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return db.GetGrantView(ctx, orgID, grantID)
}

// DenyGrant: pending → denied (customer side).
func (db *DB) DenyGrant(ctx context.Context, customerOrgID, grantID string) (*GrantView, error) {
	return db.transition(ctx, grantID, "g.customer_org_id = $2", customerOrgID, "denied", "pending")
}

// RevokeGrant: approved → revoked (either side may give access up).
func (db *DB) RevokeGrant(ctx context.Context, orgID, grantID string) (*GrantView, error) {
	return db.transition(ctx, grantID, "(g.vendor_org_id = $2 OR g.customer_org_id = $2)", orgID, "revoked", "approved")
}

// RevokeSiteGrants revokes every approved grant on a site's resources (the
// customer's "cut access" button) and returns them.
func (db *DB) RevokeSiteGrants(ctx context.Context, customerOrgID, siteID string) ([]GrantView, error) {
	rows, err := db.pool.Query(ctx, `
		UPDATE grants g SET status = 'revoked', updated_at = now()
		FROM resources r
		WHERE r.id = g.resource_id AND r.site_id = $2 AND g.customer_org_id = $1 AND g.status = 'approved'
		RETURNING g.id`, customerOrgID, siteID)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]GrantView, 0, len(ids))
	for _, id := range ids {
		if v, err := db.GetGrantView(ctx, customerOrgID, id); err == nil {
			out = append(out, *v)
		}
	}
	return out, nil
}

// ExpireDueGrants marks approved grants past expires_at as expired.
func (db *DB) ExpireDueGrants(ctx context.Context) ([]GrantView, error) {
	rows, err := db.pool.Query(ctx, `
		UPDATE grants SET status = 'expired', updated_at = now()
		WHERE status = 'approved' AND expires_at <= now()
		RETURNING id, customer_org_id`)
	if err != nil {
		return nil, err
	}
	type pair struct{ id, org string }
	var due []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.org); err != nil {
			rows.Close()
			return nil, err
		}
		due = append(due, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]GrantView, 0, len(due))
	for _, p := range due {
		if v, err := db.GetGrantView(ctx, p.org, p.id); err == nil {
			out = append(out, *v)
		}
	}
	return out, nil
}

// OrgMaxGrantTTL reads orgs.settings.max_grant_ttl ("8h"); fallback when unset/invalid.
func (db *DB) OrgMaxGrantTTL(ctx context.Context, orgID string, fallback time.Duration) time.Duration {
	var s *string
	if err := db.pool.QueryRow(ctx, `SELECT settings->>'max_grant_ttl' FROM orgs WHERE id = $1`, orgID).Scan(&s); err != nil || s == nil {
		return fallback
	}
	d, err := time.ParseDuration(*s)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// OrgLogsQueryText reports whether the customer org opted into full query text.
func (db *DB) OrgLogsQueryText(ctx context.Context, orgID string) bool {
	var on bool
	_ = db.pool.QueryRow(ctx, `SELECT COALESCE((settings->>'log_query_text')::boolean, false) FROM orgs WHERE id = $1`, orgID).Scan(&on)
	return on
}

// OrgApproverEmails lists members who may approve grants.
func (db *DB) OrgApproverEmails(ctx context.Context, orgID string) ([]string, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT u.email FROM org_members m JOIN users u ON u.id = m.user_id
		WHERE m.org_id = $1 AND m.role IN ('owner','admin','approver') ORDER BY u.email`, orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ─── Sessions, queries, audit ───────────────────────────────────────────────

// AccessSession is one stream under a grant.
type AccessSession struct {
	ID          string     `json:"id"`
	GrantID     string     `json:"grant_id"`
	OpenedAt    time.Time  `json:"opened_at"`
	ClosedAt    *time.Time `json:"closed_at,omitempty"`
	CloseReason string     `json:"close_reason,omitempty"`
	BytesUp     int64      `json:"bytes_up"`
	BytesDown   int64      `json:"bytes_down"`
	QueryCount  int        `json:"query_count"`
}

// ListGrantSessions lists sessions of a grant visible to orgID.
func (db *DB) ListGrantSessions(ctx context.Context, orgID, grantID string) ([]AccessSession, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT s.id, s.grant_id, s.opened_at, s.closed_at, COALESCE(s.close_reason, ''),
		       s.bytes_up, s.bytes_down, s.query_count
		FROM access_sessions s JOIN grants g ON g.id = s.grant_id
		WHERE s.grant_id = $1 AND (g.vendor_org_id = $2 OR g.customer_org_id = $2)
		ORDER BY s.opened_at DESC LIMIT 500`, grantID, orgID)
	return collect(rows, err, func(r pgx.Row) (*AccessSession, error) {
		var s AccessSession
		err := r.Scan(&s.ID, &s.GrantID, &s.OpenedAt, &s.ClosedAt, &s.CloseReason, &s.BytesUp, &s.BytesDown, &s.QueryCount)
		return &s, err
	})
}

// AccessQuery is a per-query summary.
type AccessQuery struct {
	ID         int64     `json:"id"`
	Command    string    `json:"command"`
	Tables     []string  `json:"tables"`
	RowCount   *int      `json:"row_count"`
	DurationMS *int      `json:"duration_ms"`
	Blocked    bool      `json:"blocked"`
	QueryText  string    `json:"query_text,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// ListSessionQueries lists a session's query summaries. Query text is only
// returned when includeText (the customer side).
func (db *DB) ListSessionQueries(ctx context.Context, orgID, sessionID string, includeText bool) ([]AccessQuery, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT q.id, q.command, COALESCE(q.tables, '{}'), q.row_count, q.duration_ms, q.blocked,
		       CASE WHEN $3 THEN COALESCE(q.query_text, '') ELSE '' END, q.created_at
		FROM access_queries q
		JOIN access_sessions s ON s.id = q.session_id
		JOIN grants g ON g.id = s.grant_id
		WHERE q.session_id = $1 AND (g.vendor_org_id = $2 OR g.customer_org_id = $2)
		ORDER BY q.created_at LIMIT 5000`, sessionID, orgID, includeText)
	return collect(rows, err, func(r pgx.Row) (*AccessQuery, error) {
		var q AccessQuery
		err := r.Scan(&q.ID, &q.Command, &q.Tables, &q.RowCount, &q.DurationMS, &q.Blocked, &q.QueryText, &q.CreatedAt)
		return &q, err
	})
}

// SessionCustomer returns the customer org of a session (for text visibility).
func (db *DB) SessionCustomer(ctx context.Context, sessionID string) (string, error) {
	var org string
	err := db.pool.QueryRow(ctx, `
		SELECT g.customer_org_id FROM access_sessions s JOIN grants g ON g.id = s.grant_id WHERE s.id = $1`,
		sessionID).Scan(&org)
	return org, mapErr(err)
}

// OrgAuditEvent is one org-scoped audit row.
type OrgAuditEvent struct {
	ID        int64          `json:"id"`
	Action    string         `json:"action"`
	UserEmail string         `json:"user_email,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// OrgAuditFilter narrows QueryOrgAudit.
type OrgAuditFilter struct {
	ActionPrefix string
	From, To     time.Time
	Limit        int
}

// QueryOrgAudit returns an org's audit events, newest first.
func (db *DB) QueryOrgAudit(ctx context.Context, orgID string, f OrgAuditFilter) ([]OrgAuditEvent, error) {
	if f.Limit <= 0 || f.Limit > 10000 {
		f.Limit = 1000
	}
	var from, to any
	if !f.From.IsZero() {
		from = f.From
	}
	if !f.To.IsZero() {
		to = f.To
	}
	rows, err := db.pool.Query(ctx, `
		SELECT a.id, a.action, COALESCE(u.email, ''), a.detail, a.created_at
		FROM audit_log a LEFT JOIN users u ON u.id = a.user_id
		WHERE a.org_id = $1 AND a.action LIKE $2 || '%'
		  AND ($3::timestamptz IS NULL OR a.created_at >= $3)
		  AND ($4::timestamptz IS NULL OR a.created_at < $4)
		ORDER BY a.created_at DESC, a.id DESC LIMIT $5`, orgID, f.ActionPrefix, from, to, f.Limit)
	return collect(rows, err, func(r pgx.Row) (*OrgAuditEvent, error) {
		var e OrgAuditEvent
		err := r.Scan(&e.ID, &e.Action, &e.UserEmail, &e.Detail, &e.CreatedAt)
		return &e, err
	})
}

// ─── Webhooks ───────────────────────────────────────────────────────────────

// Webhook is an outgoing grant-event webhook.
type Webhook struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	URL       string    `json:"url"`
	Secret    string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateWebhook stores a webhook; the caller shows the secret once.
func (db *DB) CreateWebhook(ctx context.Context, orgID, url, secret, createdBy string) (*Webhook, error) {
	var w Webhook
	err := db.pool.QueryRow(ctx, `
		INSERT INTO bridge_webhooks (id, org_id, url, secret, created_by) VALUES ($1, $2, $3, $4, $5::uuid)
		RETURNING id, org_id, url, secret, created_at`, newID("whk_"), orgID, url, secret, createdBy).
		Scan(&w.ID, &w.OrgID, &w.URL, &w.Secret, &w.CreatedAt)
	return &w, mapErr(err)
}

// ListWebhooks lists an org's webhooks (with secrets, for signing).
func (db *DB) ListWebhooks(ctx context.Context, orgID string) ([]Webhook, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, org_id, url, secret, created_at FROM bridge_webhooks WHERE org_id = $1 ORDER BY created_at`, orgID)
	return collect(rows, err, func(r pgx.Row) (*Webhook, error) {
		var w Webhook
		err := r.Scan(&w.ID, &w.OrgID, &w.URL, &w.Secret, &w.CreatedAt)
		return &w, err
	})
}

// DeleteWebhook deletes one of the org's webhooks.
func (db *DB) DeleteWebhook(ctx context.Context, orgID, id string) error {
	return affected(db.pool.Exec(ctx, `DELETE FROM bridge_webhooks WHERE id = $1 AND org_id = $2`, id, orgID))
}

// QuerySummary is one per-query record from a connector's pgproxy.
type QuerySummary struct {
	SessionID  string
	Command    string
	Tables     []string
	RowCount   int
	DurationMS int
	Blocked    bool
	QueryText  string // stored only when the caller decided the customer opted in
}

// InsertAccessQuery stores a summary and bumps the session's query count.
func (db *DB) InsertAccessQuery(ctx context.Context, q QuerySummary) error {
	cmd := q.Command
	if q.Blocked {
		cmd = "blocked:" + q.Command
	}
	_, err := db.pool.Exec(ctx, `
		WITH ins AS (
		  INSERT INTO access_queries (session_id, command, tables, row_count, duration_ms, blocked, query_text)
		  VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''))
		  RETURNING session_id)
		UPDATE access_sessions SET query_count = query_count + 1 WHERE id IN (SELECT session_id FROM ins)`,
		q.SessionID, cmd, q.Tables, q.RowCount, q.DurationMS, q.Blocked, q.QueryText)
	return err
}
