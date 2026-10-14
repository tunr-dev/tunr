package bridge

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tunr-dev/tunr/relay/internal/audit"
	"github.com/tunr-dev/tunr/relay/internal/auth"
	"github.com/tunr-dev/tunr/relay/internal/db"
	"github.com/tunr-dev/tunr/relay/internal/logger"
)

// grants.go — grant requests, decisions, revocation, kill, audit export,
// webhooks (P4-T1…T4), for humans (/orgs/{org}/…) and agents (/agent/…).
//
// Policy (roadmap §4, with the P3 rule that raw TCP can't be read-only):
//   - postgres → ro only (rw is not offered in the MVP)
//   - tcp      → rw only, which always needs two different approvers
//   - http     → not available yet
//   - TTL 1m … the customer org's max (settings.max_grant_ttl, default 8h)
//   - limits may narrow the resource's limits, never widen them
//   - auto-approve only for ro, only for the named linked vendor, only
//     within its max_ttl
//   - reason is required (≤ 500 chars): the customer reads it

const (
	defaultMaxGrantTTL = 8 * time.Hour
	minGrantTTL        = time.Minute
)

func (s *Server) registerGrants(mux *http.ServeMux) {
	h := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, s.humanAuth(fn)) }
	a := func(pattern string, fn func(http.ResponseWriter, *http.Request, *auth.AgentClaims)) {
		mux.Handle(pattern, s.agentAuth(fn))
	}
	const p = "/api/v1/bridge"

	h("GET "+p+"/orgs/{org}/grants", s.listGrants)
	h("POST "+p+"/orgs/{org}/grants", s.requestGrantHuman)
	h("GET "+p+"/orgs/{org}/grants/{id}", s.getGrant)
	h("POST "+p+"/orgs/{org}/grants/{id}/approve", s.approveGrant)
	h("POST "+p+"/orgs/{org}/grants/{id}/deny", s.denyGrant)
	h("POST "+p+"/orgs/{org}/grants/{id}/revoke", s.revokeGrant)
	h("GET "+p+"/orgs/{org}/grants/{id}/sessions", s.grantSessions)
	h("GET "+p+"/orgs/{org}/sessions/{id}/queries", s.sessionQueries)
	h("POST "+p+"/orgs/{org}/sites/{id}/kill", s.killSite)
	h("GET "+p+"/orgs/{org}/audit", s.orgAudit)
	h("GET "+p+"/orgs/{org}/audit/export", s.orgAuditExport)
	h("GET "+p+"/orgs/{org}/webhooks", s.listWebhooks)
	h("POST "+p+"/orgs/{org}/webhooks", s.createWebhook)
	h("DELETE "+p+"/orgs/{org}/webhooks/{id}", s.deleteWebhook)

	a("GET "+p+"/agent/me", s.agentMe)
	a("GET "+p+"/agent/catalog", s.agentCatalog)
	a("GET "+p+"/agent/grants", s.agentGrants)
	a("POST "+p+"/agent/grants", s.agentRequestGrant)
	a("GET "+p+"/agent/grants/{id}", s.agentGetGrant)
}

// ── agent auth ───────────────────────────────────────────────────────────────

// agentAuth accepts agent tokens only (signature, typ, aud) that are not
// revoked and belong to an active agent.
func (s *Server) agentAuth(next func(http.ResponseWriter, *http.Request, *auth.AgentClaims)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			apiError(w, http.StatusUnauthorized, "auth_required", "agent token required")
			return
		}
		c, err := s.jwt.VerifyAgent(tok)
		if err != nil {
			apiError(w, http.StatusUnauthorized, "invalid_token", "invalid or expired agent token")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
		usable, err := s.db.AgentTokenUsable(ctx, c.OrgID, c.AgentID(), c.TokenID())
		cancel()
		if err != nil || !usable {
			apiError(w, http.StatusUnauthorized, "invalid_token", "agent token revoked or agent disabled")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next(w, r, c)
	})
}

// ── policy ───────────────────────────────────────────────────────────────────

type grantInput struct {
	Resource string          `json:"resource"` // "<customer>/<name>"
	Mode     string          `json:"mode"`     // optional: defaults by kind
	TTL      string          `json:"ttl"`      // "2h"
	Reason   string          `json:"reason"`
	Limits   json.RawMessage `json:"limits"`
	AgentID  string          `json:"agent_id"` // human requests only
}

type policyErr struct{ code, msg string }

func (e *policyErr) Error() string { return e.msg }

// buildGrant validates a request against the resource and customer policy.
func (s *Server) buildGrant(ctx context.Context, vendorOrgID, agentID string, in grantInput) (*db.NewGrant, error) {
	customer, name, ok := strings.Cut(in.Resource, "/")
	if !ok || !slugRe.MatchString(customer) || !slugRe.MatchString(name) {
		return nil, &policyErr{"bad_resource", `resource must be "<customer>/<name>"`}
	}
	res, err := s.db.ResolveVendorResource(ctx, vendorOrgID, customer, name)
	if err != nil {
		return nil, err
	}
	switch res.Kind {
	case "postgres":
		if in.Mode == "" {
			in.Mode = "ro"
		}
		if in.Mode != "ro" {
			return nil, &policyErr{"mode_not_allowed", "postgres resources are read-only (ro)"}
		}
	case "tcp":
		if in.Mode == "" {
			in.Mode = "rw"
		}
		if in.Mode != "rw" {
			return nil, &policyErr{"mode_not_enforceable",
				"raw tcp can't be made read-only; request rw (needs two customer approvers)"}
		}
	default:
		return nil, &policyErr{"kind_unsupported", res.Kind + " resources are not available yet"}
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" || len(in.Reason) > 500 {
		return nil, &policyErr{"invalid_reason", "reason is required (max 500 chars); the customer reads it"}
	}
	ttl, err := time.ParseDuration(in.TTL)
	maxTTL := s.db.OrgMaxGrantTTL(ctx, res.OrgID, defaultMaxGrantTTL)
	if err != nil || ttl < minGrantTTL || ttl > maxTTL {
		return nil, &policyErr{"invalid_ttl", fmt.Sprintf("ttl must be between %s and %s", minGrantTTL, maxTTL)}
	}
	var limits json.RawMessage
	if in.Limits != nil && string(in.Limits) != "null" {
		var want, have auth.GrantLimits
		if err := strictUnmarshal(in.Limits, &want); err != nil || want.MaxRows < 0 || want.MaxBytes < 0 || want.QPS < 0 {
			return nil, &policyErr{"invalid_limits", "limits: {max_rows, max_bytes, qps}, non-negative"}
		}
		_ = json.Unmarshal(res.Limits, &have)
		wider := func(w, h int64) bool { return h > 0 && (w == 0 || w > h) }
		if wider(int64(want.MaxRows), int64(have.MaxRows)) || wider(want.MaxBytes, have.MaxBytes) || wider(int64(want.QPS), int64(have.QPS)) {
			return nil, &policyErr{"invalid_limits", "grant limits can narrow the resource's limits, not widen them"}
		}
		limits, _ = json.Marshal(want)
	}
	auto := false
	if in.Mode == "ro" && res.AutoApprove != nil {
		var aa autoApprove
		if json.Unmarshal(res.AutoApprove, &aa) == nil && aa.Mode == "ro" && aa.VendorOrgID == vendorOrgID {
			if max, err := time.ParseDuration(aa.MaxTTL); err == nil && ttl <= max {
				auto = true
			}
		}
	}
	return &db.NewGrant{VendorOrgID: vendorOrgID, AgentID: agentID, ResourceID: res.ID, Mode: in.Mode,
		Reason: in.Reason, TTL: ttl, Limits: limits, AutoApprove: auto}, nil
}

// createGrant validates, inserts, audits and notifies. userID may be "".
func (s *Server) createGrant(w http.ResponseWriter, r *http.Request, vendorOrgID, agentID, userID string, in grantInput) {
	ng, err := s.buildGrant(r.Context(), vendorOrgID, agentID, in)
	var pe *policyErr
	switch {
	case errors.As(err, &pe):
		apiError(w, http.StatusBadRequest, pe.code, pe.msg)
		return
	case errors.Is(err, db.ErrNotFound):
		apiError(w, http.StatusNotFound, "unknown_resource", "no such resource, or the customer has not linked your org")
		return
	case err != nil:
		s.dbError(w, err, "grant")
		return
	}
	g, err := s.db.CreateGrant(r.Context(), *ng)
	if err != nil {
		s.dbError(w, err, "agent or resource") // ErrNotFound: agent not yours/disabled, link gone
		return
	}
	action, event := audit.ActionBridgeGrantRequested, "grant.requested"
	if g.Status == "approved" {
		action, event = audit.ActionBridgeGrantAutoApproved, "grant.approved"
	}
	s.grantAudit(r.Context(), g, userID, action, map[string]any{"reason": g.Reason, "ttl": g.RequestedTTL.String()})
	s.notify.Grant(event, g)
	writeJSON(w, http.StatusCreated, g)
}

func (s *Server) grantAudit(ctx context.Context, g *db.GrantView, userID, action string, extra map[string]any) {
	detail := map[string]any{"grant_id": g.ID, "agent_id": g.AgentID, "agent": g.AgentName,
		"resource": g.Address, "mode": g.Mode}
	for k, v := range extra {
		detail[k] = v
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()
	for _, org := range []string{g.CustomerOrgID, g.VendorOrgID} {
		if err := s.db.AuditOrg(actx, org, userID, action, detail); err != nil {
			logger.Warn("bridge audit: %v", err)
		}
	}
}

// ── human endpoints ──────────────────────────────────────────────────────────

func (s *Server) listGrants(w http.ResponseWriter, r *http.Request) {
	m := s.member(w, r)
	if m == nil {
		return
	}
	side := r.URL.Query().Get("side")
	if side == "" {
		side = "customer"
		if !m.IsCustomer() {
			side = "vendor"
		}
	}
	if side != "vendor" && side != "customer" {
		apiError(w, http.StatusBadRequest, "invalid_side", "side must be vendor or customer")
		return
	}
	gs, err := s.db.ListGrantViews(r.Context(), m.ID, db.GrantFilter{Side: side, Status: r.URL.Query().Get("status")})
	if err != nil {
		s.dbError(w, err, "grants")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": gs})
}

func (s *Server) requestGrantHuman(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	if !m.IsVendor() {
		apiError(w, http.StatusBadRequest, "org_kind", "only vendor orgs request grants")
		return
	}
	var in grantInput
	if !decode(w, r, &in) {
		return
	}
	if in.AgentID == "" {
		apiError(w, http.StatusBadRequest, "invalid_request", "agent_id is required")
		return
	}
	s.createGrant(w, r, m.ID, in.AgentID, userFrom(r).ID, in)
}

func (s *Server) getGrant(w http.ResponseWriter, r *http.Request) {
	m := s.member(w, r)
	if m == nil {
		return
	}
	g, err := s.db.GetGrantView(r.Context(), m.ID, r.PathValue("id"))
	if err != nil {
		s.dbError(w, err, "grant")
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// approver is member + owner/admin/approver role on the customer side.
func (s *Server) approver(w http.ResponseWriter, r *http.Request) *db.OrgMembership {
	m := s.member(w, r)
	if m == nil {
		return nil
	}
	if m.Role != "owner" && m.Role != "admin" && m.Role != "approver" {
		apiError(w, http.StatusForbidden, "forbidden", "requires owner, admin or approver role")
		return nil
	}
	return m
}

func (s *Server) grantStateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrInvalidTransition):
		apiError(w, http.StatusConflict, "invalid_state", err.Error())
	case errors.Is(err, db.ErrSameApprover):
		apiError(w, http.StatusConflict, "same_approver", err.Error())
	default:
		s.dbError(w, err, "grant")
	}
}

func (s *Server) approveGrant(w http.ResponseWriter, r *http.Request) {
	m := s.approver(w, r)
	if m == nil {
		return
	}
	g, approved, err := s.db.ApproveGrant(r.Context(), m.ID, r.PathValue("id"), userFrom(r).ID)
	if err != nil {
		s.grantStateError(w, err)
		return
	}
	if approved {
		s.grantAudit(r.Context(), g, userFrom(r).ID, audit.ActionBridgeGrantApproved,
			map[string]any{"expires_at": g.ExpiresAt})
		s.notify.Grant("grant.approved", g)
	} else {
		s.grantAudit(r.Context(), g, userFrom(r).ID, audit.ActionBridgeGrantFirstApproval, nil)
		s.notify.Grant("grant.first_approval", g)
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) denyGrant(w http.ResponseWriter, r *http.Request) {
	m := s.approver(w, r)
	if m == nil {
		return
	}
	g, err := s.db.DenyGrant(r.Context(), m.ID, r.PathValue("id"))
	if err != nil {
		s.grantStateError(w, err)
		return
	}
	s.grantAudit(r.Context(), g, userFrom(r).ID, audit.ActionBridgeGrantDenied, nil)
	s.notify.Grant("grant.denied", g)
	writeJSON(w, http.StatusOK, g)
}

// revokeGrant: a customer approver, or a vendor admin giving access up.
func (s *Server) revokeGrant(w http.ResponseWriter, r *http.Request) {
	m := s.member(w, r)
	if m == nil {
		return
	}
	g0, err := s.db.GetGrantView(r.Context(), m.ID, r.PathValue("id"))
	if err != nil {
		s.dbError(w, err, "grant")
		return
	}
	okRole := map[string]bool{"owner": true, "admin": true}
	if g0.CustomerOrgID == m.ID {
		okRole["approver"] = true
	}
	if !okRole[m.Role] {
		apiError(w, http.StatusForbidden, "forbidden", "not allowed to revoke this grant")
		return
	}
	start := time.Now()
	g, err := s.db.RevokeGrant(r.Context(), m.ID, g0.ID)
	if err != nil {
		s.grantStateError(w, err)
		return
	}
	s.cutGrant(g.ID, g.SiteID, "revoked")
	s.grantAudit(r.Context(), g, userFrom(r).ID, audit.ActionBridgeGrantRevoked,
		map[string]any{"by_org": m.ID, "cut_ms": time.Since(start).Milliseconds()})
	s.notify.Grant("grant.revoked", g)
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) grantSessions(w http.ResponseWriter, r *http.Request) {
	m := s.member(w, r)
	if m == nil {
		return
	}
	if _, err := s.db.GetGrantView(r.Context(), m.ID, r.PathValue("id")); err != nil {
		s.dbError(w, err, "grant")
		return
	}
	ss, err := s.db.ListGrantSessions(r.Context(), m.ID, r.PathValue("id"))
	if err != nil {
		s.dbError(w, err, "sessions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": ss})
}

// sessionQueries: query text is only ever shown to the customer.
func (s *Server) sessionQueries(w http.ResponseWriter, r *http.Request) {
	m := s.member(w, r)
	if m == nil {
		return
	}
	cust, err := s.db.SessionCustomer(r.Context(), r.PathValue("id"))
	if err != nil {
		s.dbError(w, err, "session")
		return
	}
	qs, err := s.db.ListSessionQueries(r.Context(), m.ID, r.PathValue("id"), cust == m.ID)
	if err != nil {
		s.dbError(w, err, "queries")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queries": qs})
}

// killSite is the customer's "cut access" button: every approved grant on
// the site is revoked (so agents can't simply reconnect) and every open
// stream is closed on both legs.
func (s *Server) killSite(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	start := time.Now()
	site, err := s.db.GetSite(r.Context(), m.ID, r.PathValue("id"))
	if err != nil {
		s.dbError(w, err, "site")
		return
	}
	revoked, err := s.db.RevokeSiteGrants(r.Context(), m.ID, site.ID)
	if err != nil {
		s.dbError(w, err, "grants")
		return
	}
	closed := s.access.KillSite(site.ID, "killed")
	_ = s.hub.Send(site.ID, msg(MsgKill, KillData{GrantID: "*"}))
	elapsed := time.Since(start)
	for i := range revoked {
		s.notify.Grant("grant.revoked", &revoked[i])
	}
	s.audit(r, m.ID, audit.ActionBridgeKill, map[string]any{
		"site_id": site.ID, "grants_revoked": len(revoked), "streams_closed": closed, "cut_ms": elapsed.Milliseconds(),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"site_id": site.ID, "grants_revoked": len(revoked), "streams_closed": closed, "cut_ms": elapsed.Milliseconds(),
	})
}

// ── audit ────────────────────────────────────────────────────────────────────

func auditFilter(r *http.Request) db.OrgAuditFilter {
	q := r.URL.Query()
	f := db.OrgAuditFilter{ActionPrefix: q.Get("action")}
	if t, err := time.Parse(time.DateOnly, q.Get("from")); err == nil {
		f.From = t
	}
	if t, err := time.Parse(time.DateOnly, q.Get("to")); err == nil {
		f.To = t.Add(24 * time.Hour)
	}
	if l, err := strconv.Atoi(q.Get("limit")); err == nil {
		f.Limit = l
	}
	return f
}

func (s *Server) orgAudit(w http.ResponseWriter, r *http.Request) {
	m := s.member(w, r)
	if m == nil {
		return
	}
	f := auditFilter(r)
	if f.Limit == 0 || f.Limit > 1000 {
		f.Limit = 200
	}
	ev, err := s.db.QueryOrgAudit(r.Context(), m.ID, f)
	if err != nil {
		s.dbError(w, err, "audit")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": ev})
}

func (s *Server) orgAuditExport(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	f := auditFilter(r)
	f.Limit = 10000
	ev, err := s.db.QueryOrgAudit(r.Context(), m.ID, f)
	if err != nil {
		s.dbError(w, err, "audit")
		return
	}
	name := fmt.Sprintf("tunr-audit-%s-%s", m.Slug, time.Now().UTC().Format("20060102"))
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"id", "created_at", "action", "user", "detail"})
		for _, e := range ev {
			d, _ := json.Marshal(e.Detail)
			_ = cw.Write([]string{strconv.FormatInt(e.ID, 10), e.CreatedAt.UTC().Format(time.RFC3339),
				e.Action, e.UserEmail, csvSafe(string(d))})
		}
		cw.Flush()
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.json"`)
	writeJSON(w, http.StatusOK, map[string]any{"org": m.Slug, "exported_at": time.Now().UTC(), "events": ev})
}

// csvSafe defuses spreadsheet formula injection in exported cells.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// ── webhooks ─────────────────────────────────────────────────────────────────

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	hs, err := s.db.ListWebhooks(r.Context(), m.ID)
	if err != nil {
		s.dbError(w, err, "webhooks")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": hs})
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	var in struct{ URL string }
	if !decode(w, r, &in) {
		return
	}
	u, err := url.Parse(in.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || len(in.URL) > 500 {
		apiError(w, http.StatusBadRequest, "invalid_url", "webhook url must be https")
		return
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	secret := "whsec_" + base64.RawURLEncoding.EncodeToString(b)
	h, err := s.db.CreateWebhook(r.Context(), m.ID, in.URL, secret, userFrom(r).ID)
	if err != nil {
		s.dbError(w, err, "webhook")
		return
	}
	s.audit(r, m.ID, audit.ActionBridgeWebhookCreated, map[string]any{"webhook_id": h.ID, "url": h.URL})
	writeJSON(w, http.StatusCreated, map[string]any{"webhook": h, "secret": secret})
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	if err := s.db.DeleteWebhook(r.Context(), m.ID, r.PathValue("id")); err != nil {
		s.dbError(w, err, "webhook")
		return
	}
	s.audit(r, m.ID, audit.ActionBridgeWebhookDeleted, map[string]any{"webhook_id": r.PathValue("id")})
	w.WriteHeader(http.StatusNoContent)
}

// ── agent endpoints ──────────────────────────────────────────────────────────

func (s *Server) agentMe(w http.ResponseWriter, r *http.Request, c *auth.AgentClaims) {
	a, err := s.db.GetAgent(r.Context(), c.OrgID, c.AgentID())
	if err != nil {
		s.dbError(w, err, "agent")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": a, "token_id": c.TokenID(), "token_expires_at": c.ExpiresAt.Time})
}

// CatalogItem is a catalog entry with this agent's current grant, if any.
type CatalogItem struct {
	db.CatalogEntry
	GrantID        string     `json:"grant_id,omitempty"`
	GrantStatus    string     `json:"grant_status,omitempty"` // approved | pending | ""
	GrantMode      string     `json:"grant_mode,omitempty"`
	GrantExpiresAt *time.Time `json:"grant_expires_at,omitempty"`
}

func (s *Server) agentCatalog(w http.ResponseWriter, r *http.Request, c *auth.AgentClaims) {
	cat, err := s.db.Catalog(r.Context(), c.OrgID)
	if err != nil {
		s.dbError(w, err, "catalog")
		return
	}
	gs, err := s.db.ListGrantViews(r.Context(), c.OrgID, db.GrantFilter{Side: "vendor", AgentID: c.AgentID()})
	if err != nil {
		s.dbError(w, err, "grants")
		return
	}
	out := make([]CatalogItem, 0, len(cat))
	for _, e := range cat {
		item := CatalogItem{CatalogEntry: e}
		for _, g := range gs { // newest first; approved beats pending
			if g.ResourceID != e.ResourceID || (g.Status != "approved" && g.Status != "pending") {
				continue
			}
			if item.GrantStatus == "" || (item.GrantStatus == "pending" && g.Status == "approved") {
				item.GrantID, item.GrantStatus, item.GrantMode, item.GrantExpiresAt = g.ID, g.Status, g.Mode, g.ExpiresAt
			}
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"resources": out})
}

func (s *Server) agentGrants(w http.ResponseWriter, r *http.Request, c *auth.AgentClaims) {
	gs, err := s.db.ListGrantViews(r.Context(), c.OrgID, db.GrantFilter{
		Side: "vendor", AgentID: c.AgentID(), Status: r.URL.Query().Get("status")})
	if err != nil {
		s.dbError(w, err, "grants")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": gs})
}

func (s *Server) agentRequestGrant(w http.ResponseWriter, r *http.Request, c *auth.AgentClaims) {
	var in grantInput
	if !decode(w, r, &in) {
		return
	}
	if in.AgentID != "" && in.AgentID != c.AgentID() {
		apiError(w, http.StatusForbidden, "forbidden", "an agent can only request grants for itself")
		return
	}
	s.createGrant(w, r, c.OrgID, c.AgentID(), "", in)
}

func (s *Server) agentGetGrant(w http.ResponseWriter, r *http.Request, c *auth.AgentClaims) {
	g, err := s.db.GetGrantView(r.Context(), c.OrgID, r.PathValue("id"))
	if err != nil || g.AgentID != c.AgentID() {
		apiError(w, http.StatusNotFound, "not_found", "grant not found")
		return
	}
	writeJSON(w, http.StatusOK, g)
}
