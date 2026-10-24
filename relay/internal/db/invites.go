package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// invites.go — vendor → customer connect invites (migration 010).
//
// An invite carries no rights. Accepting one only does what the customer
// could do by hand: link the vendor and (optionally) create a site. Grants are
// still requested and approved one by one.

// Invite is a vendor's one-time connect link.
type Invite struct {
	ID            string     `json:"id"`
	VendorOrgID   string     `json:"vendor_org_id"`
	VendorSlug    string     `json:"vendor_slug"`
	VendorName    string     `json:"vendor_name"`
	CustomerHint  string     `json:"customer_hint,omitempty"`
	Note          string     `json:"note,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	AcceptedAt    *time.Time `json:"accepted_at,omitempty"`
	CustomerOrgID string     `json:"customer_org_id,omitempty"`
	CustomerSlug  string     `json:"customer_slug,omitempty"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	Status        string     `json:"status"` // pending | accepted | expired | revoked
}

// ErrInviteUsed means the invite was accepted, revoked or has expired.
var ErrInviteUsed = errors.New("invite already used, revoked or expired")

const inviteSelect = `
	SELECT i.id, i.vendor_org_id, v.slug, v.name, COALESCE(i.customer_hint, ''), COALESCE(i.note, ''),
	       i.created_at, i.expires_at, i.accepted_at, COALESCE(i.customer_org_id, ''), COALESCE(c.slug, ''),
	       i.revoked_at,
	       CASE WHEN i.revoked_at IS NOT NULL THEN 'revoked'
	            WHEN i.accepted_at IS NOT NULL THEN 'accepted'
	            WHEN i.expires_at <= now() THEN 'expired' ELSE 'pending' END
	FROM bridge_invites i
	JOIN orgs v ON v.id = i.vendor_org_id
	LEFT JOIN orgs c ON c.id = i.customer_org_id`

func scanInvite(row pgx.Row) (*Invite, error) {
	var i Invite
	if err := row.Scan(&i.ID, &i.VendorOrgID, &i.VendorSlug, &i.VendorName, &i.CustomerHint, &i.Note,
		&i.CreatedAt, &i.ExpiresAt, &i.AcceptedAt, &i.CustomerOrgID, &i.CustomerSlug, &i.RevokedAt, &i.Status); err != nil {
		return nil, mapErr(err)
	}
	return &i, nil
}

// CreateInvite stores an invite; only the token's hash is kept.
func (db *DB) CreateInvite(ctx context.Context, vendorOrgID, tokenHash, hint, note, createdBy string, expiresAt time.Time) (*Invite, error) {
	var id string
	err := db.pool.QueryRow(ctx, `
		INSERT INTO bridge_invites (id, vendor_org_id, token_hash, customer_hint, note, created_by, expires_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, '')::uuid, $7)
		RETURNING id`, newID("inv_"), vendorOrgID, tokenHash, hint, note, createdBy, expiresAt).Scan(&id)
	if err != nil {
		return nil, mapErr(err)
	}
	return scanInvite(db.pool.QueryRow(ctx, inviteSelect+` WHERE i.id = $1`, id))
}

// ListInvites lists a vendor's invites, newest first.
func (db *DB) ListInvites(ctx context.Context, vendorOrgID string) ([]Invite, error) {
	rows, err := db.pool.Query(ctx, inviteSelect+`
		WHERE i.vendor_org_id = $1 ORDER BY i.created_at DESC LIMIT 200`, vendorOrgID)
	return collect(rows, err, scanInvite)
}

// RevokeInvite revokes a pending invite of the vendor.
func (db *DB) RevokeInvite(ctx context.Context, vendorOrgID, inviteID string) error {
	return affected(db.pool.Exec(ctx, `
		UPDATE bridge_invites SET revoked_at = now()
		WHERE id = $1 AND vendor_org_id = $2 AND revoked_at IS NULL AND accepted_at IS NULL`, inviteID, vendorOrgID))
}

// InviteByToken looks an invite up by its token hash.
func (db *DB) InviteByToken(ctx context.Context, tokenHash string) (*Invite, error) {
	return scanInvite(db.pool.QueryRow(ctx, inviteSelect+` WHERE i.token_hash = $1`, tokenHash))
}

// ConsumeInvite marks a pending invite accepted by userID for customerOrgID,
// once: a second call (or a revoked/expired invite) returns ErrInviteUsed.
func (db *DB) ConsumeInvite(ctx context.Context, tokenHash, userID, customerOrgID string) (*Invite, error) {
	var id string
	err := db.pool.QueryRow(ctx, `
		UPDATE bridge_invites SET accepted_at = now(), accepted_by = $2::uuid, customer_org_id = $3
		WHERE token_hash = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()
		RETURNING id`, tokenHash, userID, customerOrgID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInviteUsed
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return scanInvite(db.pool.QueryRow(ctx, inviteSelect+` WHERE i.id = $1`, id))
}
