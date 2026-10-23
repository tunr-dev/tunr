package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// hosted.go — Agent Bridge hosted agents (P7). Schema: migrations/008_hosted_agents.sql.

// SetAgentBootstrap rotates the agent's bootstrap secret (hash) and trigger
// secret before a deploy, and marks it hosted. orgID must own the agent.
func (db *DB) SetAgentBootstrap(ctx context.Context, orgID, agentID, bootstrapHash, triggerSecret string) error {
	return affected(db.pool.Exec(ctx, `
		UPDATE agents SET kind = 'hosted', bootstrap_hash = $3, trigger_secret = $4
		WHERE id = $1 AND org_id = $2 AND status = 'active'`, agentID, orgID, bootstrapHash, triggerSecret))
}

// LinkAgentApp records which app runs the agent and marks the app as an agent.
func (db *DB) LinkAgentApp(ctx context.Context, orgID, agentID, appID string, volumeMB int) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := affected(tx.Exec(ctx, `UPDATE agents SET app_id = $3 WHERE id = $1 AND org_id = $2`, agentID, orgID, appID)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE apps SET kind = 'agent', volume_mb = $2 WHERE id = $1`, appID, volumeMB); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AgentByBootstrap resolves a bootstrap secret hash to an active hosted agent.
func (db *DB) AgentByBootstrap(ctx context.Context, bootstrapHash string) (*Agent, error) {
	return scanAgent(db.pool.QueryRow(ctx, `SELECT `+agentCols+` FROM agents
		WHERE bootstrap_hash = $1 AND kind = 'hosted' AND status = 'active'`, bootstrapHash))
}

// HostedApp is an agent app as the trigger layer needs it.
type HostedApp struct {
	AppID, Name, OrgID, AgentID, AgentName, TriggerSecret string
	Active                                                bool // agent exists and is active
}

// ListHostedApps returns every agent app that isn't deleted, including those
// whose agent is disabled or gone (Active=false): the relay must keep
// guarding their hostnames rather than let traffic through to the container.
func (db *DB) ListHostedApps(ctx context.Context) ([]HostedApp, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT a.id, a.name, COALESCE(g.org_id, ''), COALESCE(g.id, ''), COALESCE(g.name, ''),
		       COALESCE(g.trigger_secret, ''), COALESCE(g.status = 'active', false)
		FROM apps a LEFT JOIN agents g ON g.app_id = a.id
		WHERE a.kind = 'agent' AND a.status <> 'deleted'`)
	return collect(rows, err, func(r pgx.Row) (*HostedApp, error) {
		var h HostedApp
		err := r.Scan(&h.AppID, &h.Name, &h.OrgID, &h.AgentID, &h.AgentName, &h.TriggerSecret, &h.Active)
		return &h, err
	})
}

// CountHostedAgentApps counts a user's live agent apps (plan quota).
func (db *DB) CountHostedAgentApps(ctx context.Context, userID string) (int, error) {
	var n int
	err := db.pool.QueryRow(ctx, `SELECT count(*) FROM apps WHERE user_id = $1::uuid AND kind = 'agent' AND status <> 'deleted'`, userID).Scan(&n)
	return n, err
}

// Trigger is a cron or webhook trigger of an agent app.
type Trigger struct {
	ID        string     `json:"id"`
	AppID     string     `json:"app_id"`
	OrgID     string     `json:"org_id"`
	Kind      string     `json:"kind"`
	Name      string     `json:"name"`
	Spec      string     `json:"spec,omitempty"`
	Path      string     `json:"path"`
	Secret    string     `json:"-"`
	LastFired *time.Time `json:"last_fired,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

const triggerCols = `id, app_id, org_id, kind, name, spec, path, secret, last_fired, created_at`

func scanTrigger(r pgx.Row) (*Trigger, error) {
	var t Trigger
	if err := r.Scan(&t.ID, &t.AppID, &t.OrgID, &t.Kind, &t.Name, &t.Spec, &t.Path, &t.Secret, &t.LastFired, &t.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &t, nil
}

// UpsertTrigger creates or updates a trigger on one of the org's agent apps.
func (db *DB) UpsertTrigger(ctx context.Context, orgID, agentID, kind, name, spec, path, secret string) (*Trigger, error) {
	return scanTrigger(db.pool.QueryRow(ctx, `
		INSERT INTO app_triggers (id, app_id, org_id, kind, name, spec, path, secret)
		SELECT $1, g.app_id, g.org_id, $4, $5, $6, $7, $8 FROM agents g
		WHERE g.id = $2 AND g.org_id = $3 AND g.app_id IS NOT NULL
		ON CONFLICT (app_id, kind, name) DO UPDATE SET spec = EXCLUDED.spec, path = EXCLUDED.path, secret = EXCLUDED.secret
		RETURNING `+triggerCols, newID("trg_"), agentID, orgID, kind, name, spec, path, secret))
}

// ListAgentTriggers lists triggers of one of the org's agents.
func (db *DB) ListAgentTriggers(ctx context.Context, orgID, agentID string) ([]Trigger, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT t.id, t.app_id, t.org_id, t.kind, t.name, t.spec, t.path, t.secret, t.last_fired, t.created_at
		FROM app_triggers t JOIN agents g ON g.app_id = t.app_id
		WHERE g.id = $1 AND g.org_id = $2 ORDER BY t.created_at`, agentID, orgID)
	return collect(rows, err, scanTrigger)
}

// DeleteTrigger deletes one of the org's triggers.
func (db *DB) DeleteTrigger(ctx context.Context, orgID, triggerID string) error {
	return affected(db.pool.Exec(ctx, `DELETE FROM app_triggers WHERE id = $1 AND org_id = $2`, triggerID, orgID))
}

// ListCronTriggers returns every cron trigger of a live agent app.
func (db *DB) ListCronTriggers(ctx context.Context) ([]Trigger, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT t.id, t.app_id, t.org_id, t.kind, t.name, t.spec, t.path, t.secret, t.last_fired, t.created_at
		FROM app_triggers t JOIN apps a ON a.id = t.app_id
		WHERE t.kind = 'cron' AND a.status <> 'deleted'`)
	return collect(rows, err, scanTrigger)
}

// ClaimCronFire atomically marks a trigger fired for the given minute. Only
// one caller (one relay instance) wins per minute.
func (db *DB) ClaimCronFire(ctx context.Context, triggerID string, minute time.Time) (bool, error) {
	tag, err := db.pool.Exec(ctx, `
		UPDATE app_triggers SET last_fired = $2
		WHERE id = $1 AND (last_fired IS NULL OR last_fired < $2)`, triggerID, minute)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// WebhookTrigger resolves an app name + webhook name to its trigger.
func (db *DB) WebhookTrigger(ctx context.Context, appName, name string) (*Trigger, error) {
	t, err := scanTrigger(db.pool.QueryRow(ctx, `
		SELECT t.id, t.app_id, t.org_id, t.kind, t.name, t.spec, t.path, t.secret, t.last_fired, t.created_at
		FROM app_triggers t JOIN apps a ON a.id = t.app_id
		WHERE a.name = $1 AND t.kind = 'webhook' AND t.name = $2 AND a.status <> 'deleted'`, appName, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// AppKind returns an app's kind ("web" | "agent").
func (db *DB) AppKind(ctx context.Context, appID string) (string, error) {
	var k string
	err := db.pool.QueryRow(ctx, `SELECT kind FROM apps WHERE id = $1`, appID).Scan(&k)
	return k, mapErr(err)
}

// AgentAppAdmin reports whether appID is a hosted agent's app and, if so,
// whether userID is currently an owner or admin of the org that owns that
// agent. isAgentApp=false means the app has no agent (a web app or an orphan).
func (db *DB) AgentAppAdmin(ctx context.Context, appID, userID string) (isAgentApp, admin bool, err error) {
	err = db.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM agents g WHERE g.app_id = $1),
		       EXISTS (SELECT 1 FROM agents g JOIN org_members m ON m.org_id = g.org_id
		               WHERE g.app_id = $1 AND m.user_id::text = $2 AND m.role IN ('owner', 'admin'))`,
		appID, userID).Scan(&isAgentApp, &admin)
	return isAgentApp, admin, err
}

// SetAppOwner moves an app to another user (a hosted agent redeployed by a
// current org admin).
func (db *DB) SetAppOwner(ctx context.Context, appID, userID string) error {
	_, err := db.pool.Exec(ctx, `UPDATE apps SET user_id = $2::uuid, updated_at = now() WHERE id = $1`, appID, userID)
	return err
}
