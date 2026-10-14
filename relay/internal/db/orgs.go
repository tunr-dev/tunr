package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// orgs.go — Agent Bridge organisations, membership and vendor ↔ customer links.
// Schema: migrations/005_orgs.sql.
//
// Tenant rule: every read that returns org-owned data takes the caller's org
// (or user) and filters by it in SQL. Nothing here trusts an ID on its own.

// Org is a vendor, customer, or both.
type Org struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	Kind      string    `json:"kind"`
	Personal  bool      `json:"personal"`
	CreatedAt time.Time `json:"created_at"`
}

// IsVendor reports whether the org may own agents.
func (o *Org) IsVendor() bool { return o.Kind == "vendor" || o.Kind == "both" }

// IsCustomer reports whether the org may own sites and resources.
func (o *Org) IsCustomer() bool { return o.Kind == "customer" || o.Kind == "both" }

// OrgMembership is an org as seen by one member.
type OrgMembership struct {
	Org
	Role string `json:"role"`
}

// OrgMember is one row of an org's member list.
type OrgMember struct {
	UserID    string    `json:"user_id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

const orgCols = `o.id, o.name, o.slug, o.kind, o.personal_user_id IS NOT NULL, o.created_at`

func scanOrg(row pgx.Row, extra ...any) (*Org, error) {
	var o Org
	dest := append([]any{&o.ID, &o.Name, &o.Slug, &o.Kind, &o.Personal, &o.CreatedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, mapErr(err)
	}
	return &o, nil
}

// CreateOrg creates a team org with ownerUserID as its owner.
func (db *DB) CreateOrg(ctx context.Context, ownerUserID, name, slug, kind string) (*Org, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	o, err := scanOrg(tx.QueryRow(ctx, `
		INSERT INTO orgs AS o (id, name, slug, kind) VALUES ($1, $2, $3, $4)
		RETURNING `+orgCols, newID("org_"), name, slug, kind))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2::uuid, 'owner')`,
		o.ID, ownerUserID); err != nil {
		return nil, mapErr(err)
	}
	return o, mapErr(tx.Commit(ctx))
}

// EnsurePersonalOrg returns the user's personal org, creating it on first use.
// Called lazily by bridge endpoints — the signup flow is deliberately untouched.
// Safe under concurrency: personal_user_id is UNIQUE.
func (db *DB) EnsurePersonalOrg(ctx context.Context, userID, email string) (*Org, error) {
	if o, err := db.personalOrg(ctx, userID); !errors.Is(err, ErrNotFound) {
		return o, err
	}

	base := personalSlugBase(email)
	for attempt := 0; attempt < 5; attempt++ {
		slug := base
		if attempt > 0 {
			slug = base + "-" + randHex(2)
		}
		err := db.insertPersonalOrg(ctx, userID, email, slug)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrConflict) {
			return nil, err
		}
		// Conflict: either the slug is taken (retry with a suffix) or a
		// concurrent call created the personal org (the lookup below wins).
		if o, lerr := db.personalOrg(ctx, userID); lerr == nil {
			return o, nil
		}
	}
	return db.personalOrg(ctx, userID)
}

func (db *DB) insertPersonalOrg(ctx context.Context, userID, email, slug string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	id := newID("org_")
	if _, err := tx.Exec(ctx, `
		INSERT INTO orgs (id, name, slug, kind, personal_user_id) VALUES ($1, $2, $3, 'both', $4::uuid)`,
		id, email, slug, userID); err != nil {
		return mapErr(err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2::uuid, 'owner')`,
		id, userID); err != nil {
		return mapErr(err)
	}
	return mapErr(tx.Commit(ctx))
}

func (db *DB) personalOrg(ctx context.Context, userID string) (*Org, error) {
	return scanOrg(db.pool.QueryRow(ctx,
		`SELECT `+orgCols+` FROM orgs o WHERE o.personal_user_id = $1::uuid`, userID))
}

// personalSlugBase derives a slug from the email's local part ("Ali.Veli+x@…"
// → "ali-veli-x"), padded to the 3-char minimum.
func personalSlugBase(email string) string {
	local, _, _ := strings.Cut(strings.ToLower(email), "@")
	var b strings.Builder
	dash := false
	for _, ch := range local {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
			b.WriteRune(ch)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if len(s) > 24 {
		s = strings.TrimRight(s[:24], "-")
	}
	if len(s) < 3 {
		s = "user-" + randHex(3)
	}
	return s
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ListOrgsForUser returns every org the user belongs to, with their role.
func (db *DB) ListOrgsForUser(ctx context.Context, userID string) ([]OrgMembership, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT `+orgCols+`, m.role
		FROM orgs o JOIN org_members m ON m.org_id = o.id
		WHERE m.user_id = $1::uuid
		ORDER BY o.personal_user_id IS NULL, o.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgMembership
	for rows.Next() {
		var m OrgMembership
		o, err := scanOrg(rows, &m.Role)
		if err != nil {
			return nil, err
		}
		m.Org = *o
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetOrgForMember resolves an org by ID or slug, but only if userID is a
// member. Non-members get ErrNotFound — same as a missing org.
func (db *DB) GetOrgForMember(ctx context.Context, userID, idOrSlug string) (*OrgMembership, error) {
	var m OrgMembership
	o, err := scanOrg(db.pool.QueryRow(ctx, `
		SELECT `+orgCols+`, m.role
		FROM orgs o JOIN org_members m ON m.org_id = o.id
		WHERE m.user_id = $1::uuid AND (o.id = $2 OR o.slug = $2)`, userID, idOrSlug), &m.Role)
	if err != nil {
		return nil, err
	}
	m.Org = *o
	return &m, nil
}

// GetOrgBySlug returns an org's public identity (used to name a link
// counterparty). It exposes no members, sites or agents.
func (db *DB) GetOrgBySlug(ctx context.Context, slug string) (*Org, error) {
	return scanOrg(db.pool.QueryRow(ctx, `SELECT `+orgCols+` FROM orgs o WHERE o.slug = $1`, slug))
}

// AddOrgMember adds (or re-roles) a user by email, creating the user row if the
// person has never signed in — the same row magic-link login would create.
func (db *DB) AddOrgMember(ctx context.Context, orgID, email, role string) (*OrgMember, error) {
	u, _, err := db.GetOrCreateUser(ctx, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return nil, err
	}
	var m OrgMember
	err = db.pool.QueryRow(ctx, `
		INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2::uuid, $3)
		ON CONFLICT (org_id, user_id) DO UPDATE SET role = EXCLUDED.role
		RETURNING user_id::text, role, created_at`, orgID, u.ID, role).
		Scan(&m.UserID, &m.Role, &m.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	m.Email = u.Email
	return &m, nil
}

// ListOrgMembers lists an org's members.
func (db *DB) ListOrgMembers(ctx context.Context, orgID string) ([]OrgMember, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT m.user_id::text, u.email, m.role, m.created_at
		FROM org_members m JOIN users u ON u.id = m.user_id
		WHERE m.org_id = $1 ORDER BY m.created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgMember
	for rows.Next() {
		var m OrgMember
		if err := rows.Scan(&m.UserID, &m.Email, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ─── Links ───────────────────────────────────────────────────────────────────

// Link is a vendor ↔ customer relationship, as seen from one side.
type Link struct {
	ID          string     `json:"id"`
	VendorOrg   Org        `json:"vendor"`
	CustomerOrg Org        `json:"customer"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

const linkSelect = `
	SELECT l.id, l.status, l.created_at, l.revoked_at,
	       v.id, v.name, v.slug, v.kind, v.personal_user_id IS NOT NULL, v.created_at,
	       c.id, c.name, c.slug, c.kind, c.personal_user_id IS NOT NULL, c.created_at
	FROM links l
	JOIN orgs v ON v.id = l.vendor_org_id
	JOIN orgs c ON c.id = l.customer_org_id`

func scanLink(row pgx.Row) (*Link, error) {
	var l Link
	v, c := &l.VendorOrg, &l.CustomerOrg
	err := row.Scan(&l.ID, &l.Status, &l.CreatedAt, &l.RevokedAt,
		&v.ID, &v.Name, &v.Slug, &v.Kind, &v.Personal, &v.CreatedAt,
		&c.ID, &c.Name, &c.Slug, &c.Kind, &c.Personal, &c.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &l, nil
}

// CreateLink lets vendorOrgID see and request customerOrgID's resources.
// Re-linking a revoked pair reactivates the existing row.
func (db *DB) CreateLink(ctx context.Context, customerOrgID, vendorOrgID, createdBy string) (*Link, error) {
	var id string
	err := db.pool.QueryRow(ctx, `
		INSERT INTO links (id, vendor_org_id, customer_org_id, created_by)
		VALUES ($1, $2, $3, $4::uuid)
		ON CONFLICT (vendor_org_id, customer_org_id)
		DO UPDATE SET status = 'active', revoked_at = NULL
		RETURNING id`, newID("lnk_"), vendorOrgID, customerOrgID, createdBy).Scan(&id)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanLink(db.pool.QueryRow(ctx, linkSelect+` WHERE l.id = $1`, id))
}

// ListLinks returns links where orgID is either side.
func (db *DB) ListLinks(ctx context.Context, orgID string) ([]Link, error) {
	rows, err := db.pool.Query(ctx, linkSelect+`
		WHERE l.vendor_org_id = $1 OR l.customer_org_id = $1
		ORDER BY l.created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Link
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

// RevokeLink revokes a link; either side may do it.
func (db *DB) RevokeLink(ctx context.Context, orgID, linkID string) (*Link, error) {
	tag, err := db.pool.Exec(ctx, `
		UPDATE links SET status = 'revoked', revoked_at = COALESCE(revoked_at, now())
		WHERE id = $1 AND (vendor_org_id = $2 OR customer_org_id = $2)`, linkID, orgID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return scanLink(db.pool.QueryRow(ctx, linkSelect+` WHERE l.id = $1`, linkID))
}

// HasActiveLink reports whether vendor may currently act on customer.
func (db *DB) HasActiveLink(ctx context.Context, vendorOrgID, customerOrgID string) (bool, error) {
	var ok bool
	err := db.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM links
		  WHERE vendor_org_id = $1 AND customer_org_id = $2 AND status = 'active')`,
		vendorOrgID, customerOrgID).Scan(&ok)
	return ok, err
}

// ─── Audit ───────────────────────────────────────────────────────────────────

// AuditOrg records an org-scoped event in audit_log. Unlike AuditLog it is
// synchronous: bridge events are the customer's evidence, so the caller learns
// if one failed to persist. userID may be empty (system or agent actions).
func (db *DB) AuditOrg(ctx context.Context, orgID, userID, action string, detail map[string]any) error {
	var uid any
	if userID != "" {
		uid = userID
	}
	_, err := db.pool.Exec(ctx,
		`INSERT INTO audit_log (org_id, user_id, action, detail) VALUES ($1, $2::uuid, $3, $4)`,
		orgID, uid, action, detail)
	if err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}
