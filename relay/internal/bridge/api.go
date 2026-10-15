package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/tunr-dev/tunr/relay/internal/audit"
	"github.com/tunr-dev/tunr/relay/internal/auth"
	"github.com/tunr-dev/tunr/relay/internal/db"
	"github.com/tunr-dev/tunr/relay/internal/logger"
)

// api.go — /api/v1/bridge management API (roadmap §7, P1-T4).
//
// Every org-scoped route lives under /orgs/{org}/ and resolves {org} (ID or
// slug) through the caller's membership first. A non-member gets 404, never
// 403: whether an org exists is not the caller's business. All DB calls then
// receive that org's ID, and the DB layer filters by it.
//
// Human JWTs only. Agent tokens are rejected here by auth.Verify (typ claim);
// the agent-facing surface (grant requests, /bridge/access) arrives in P3/P4.

const maxBody = 64 << 10

type ctxKey int

const ctxUser ctxKey = iota

type user struct{ ID, Email, Plan string }

func (s *Server) registerAPI(mux *http.ServeMux) {
	h := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, s.humanAuth(fn)) }
	const p = "/api/v1/bridge"

	h("GET "+p+"/orgs", s.listOrgs)
	h("POST "+p+"/orgs", s.createOrg)
	h("GET "+p+"/orgs/{org}", s.getOrg)
	h("GET "+p+"/orgs/{org}/members", s.listMembers)
	h("POST "+p+"/orgs/{org}/members", s.addMember)

	h("GET "+p+"/orgs/{org}/links", s.listLinks)
	h("POST "+p+"/orgs/{org}/links", s.createLink)
	h("DELETE "+p+"/orgs/{org}/links/{id}", s.revokeLink)

	h("GET "+p+"/orgs/{org}/sites", s.listSites)
	h("POST "+p+"/orgs/{org}/sites", s.createSite)
	h("GET "+p+"/orgs/{org}/sites/{id}", s.getSite)
	h("DELETE "+p+"/orgs/{org}/sites/{id}", s.deleteSite)
	h("POST "+p+"/orgs/{org}/sites/{id}/rotate-token", s.rotateSiteToken)

	h("GET "+p+"/orgs/{org}/resources", s.listResources)
	h("PATCH "+p+"/orgs/{org}/resources/{id}", s.patchResource)
	h("GET "+p+"/orgs/{org}/catalog", s.catalog)

	h("GET "+p+"/orgs/{org}/agents", s.listAgents)
	h("POST "+p+"/orgs/{org}/agents", s.createAgent)
	h("PATCH "+p+"/orgs/{org}/agents/{id}", s.patchAgent)
	h("GET "+p+"/orgs/{org}/agents/{id}/tokens", s.listAgentTokens)
	h("POST "+p+"/orgs/{org}/agents/{id}/tokens", s.issueAgentToken)
	h("DELETE "+p+"/orgs/{org}/agents/{id}/tokens/{tid}", s.revokeAgentToken)
}

// ── middleware & helpers ─────────────────────────────────────────────────────

func (s *Server) humanAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			apiError(w, http.StatusUnauthorized, "auth_required", "Authorization: Bearer <token> required")
			return
		}
		claims, err := s.jwt.Verify(tok)
		if err != nil || claims.UserID == "" {
			apiError(w, http.StatusUnauthorized, "invalid_token", "invalid or expired token")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		ctx := context.WithValue(r.Context(), ctxUser, user{ID: claims.UserID, Email: claims.Email, Plan: claims.Plan})
		next(w, r.WithContext(ctx))
	})
}

func userFrom(r *http.Request) user { u, _ := r.Context().Value(ctxUser).(user); return u }

// member resolves {org} for the caller; writes 404 and returns nil otherwise.
func (s *Server) member(w http.ResponseWriter, r *http.Request) *db.OrgMembership {
	m, err := s.db.GetOrgForMember(r.Context(), userFrom(r).ID, r.PathValue("org"))
	if err != nil {
		s.dbError(w, err, "org")
		return nil
	}
	return m
}

// admin is member + owner/admin role.
func (s *Server) admin(w http.ResponseWriter, r *http.Request) *db.OrgMembership {
	m := s.member(w, r)
	if m == nil {
		return nil
	}
	if m.Role != "owner" && m.Role != "admin" {
		apiError(w, http.StatusForbidden, "forbidden", "requires owner or admin role")
		return nil
	}
	return m
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) dbError(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, db.ErrNotFound):
		apiError(w, http.StatusNotFound, "not_found", what+" not found")
	case errors.Is(err, db.ErrConflict):
		apiError(w, http.StatusConflict, "conflict", what+" already exists")
	default:
		logger.Warn("bridge api: %s: %v", what, err)
		apiError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		apiError(w, http.StatusBadRequest, "invalid_request", "trailing data after JSON body")
		return false
	}
	return true
}

func (s *Server) audit(r *http.Request, orgID, action string, detail map[string]any) {
	if err := s.db.AuditOrg(r.Context(), orgID, userFrom(r).ID, action, detail); err != nil {
		logger.Warn("bridge audit: %v", err)
	}
}

// slugRe: 3–32 chars, lowercase letters, digits, inner hyphens. Used for org
// slugs and site/resource/agent names (they appear in "org/resource" addresses).
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`)

func validName(w http.ResponseWriter, field, v string) bool {
	if !slugRe.MatchString(v) || strings.Contains(v, "--") {
		apiError(w, http.StatusBadRequest, "invalid_"+field,
			field+" must be 3-32 chars: lowercase letters, digits and single inner hyphens")
		return false
	}
	return true
}

// ── orgs & members ───────────────────────────────────────────────────────────

func (s *Server) listOrgs(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if _, err := s.db.EnsurePersonalOrg(r.Context(), u.ID, u.Email); err != nil {
		s.dbError(w, err, "personal org")
		return
	}
	orgs, err := s.db.ListOrgsForUser(r.Context(), u.ID)
	if err != nil {
		s.dbError(w, err, "orgs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"orgs": orgs})
}

func (s *Server) createOrg(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name, Slug, Kind string }
	if !decode(w, r, &in) || !validName(w, "slug", in.Slug) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 {
		apiError(w, http.StatusBadRequest, "invalid_name", "name is required (max 100 chars)")
		return
	}
	if in.Kind == "" {
		in.Kind = "both"
	}
	if in.Kind != "vendor" && in.Kind != "customer" && in.Kind != "both" {
		apiError(w, http.StatusBadRequest, "invalid_kind", "kind must be vendor, customer or both")
		return
	}
	o, err := s.db.CreateOrg(r.Context(), userFrom(r).ID, in.Name, in.Slug, in.Kind)
	if err != nil {
		s.dbError(w, err, "org")
		return
	}
	s.audit(r, o.ID, audit.ActionBridgeOrgCreated, map[string]any{"slug": o.Slug, "kind": o.Kind})
	writeJSON(w, http.StatusCreated, db.OrgMembership{Org: *o, Role: "owner"})
}

func (s *Server) getOrg(w http.ResponseWriter, r *http.Request) {
	if m := s.member(w, r); m != nil {
		writeJSON(w, http.StatusOK, m)
	}
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) {
	m := s.member(w, r)
	if m == nil {
		return
	}
	members, err := s.db.ListOrgMembers(r.Context(), m.ID)
	if err != nil {
		s.dbError(w, err, "members")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": members})
}

func (s *Server) addMember(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	var in struct{ Email, Role string }
	if !decode(w, r, &in) {
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if !strings.Contains(in.Email, "@") || len(in.Email) > 254 {
		apiError(w, http.StatusBadRequest, "invalid_email", "a valid email is required")
		return
	}
	switch in.Role {
	case "admin", "approver", "member":
	case "owner":
		if m.Role != "owner" {
			apiError(w, http.StatusForbidden, "forbidden", "only an owner can add an owner")
			return
		}
	default:
		apiError(w, http.StatusBadRequest, "invalid_role", "role must be owner, admin, approver or member")
		return
	}
	if m.Personal {
		apiError(w, http.StatusBadRequest, "personal_org", "personal orgs cannot have members; create a team org")
		return
	}
	added, err := s.db.AddOrgMember(r.Context(), m.ID, in.Email, in.Role)
	if err != nil {
		s.dbError(w, err, "member")
		return
	}
	s.audit(r, m.ID, audit.ActionBridgeMemberAdded, map[string]any{"user_id": added.UserID, "role": added.Role})
	writeJSON(w, http.StatusCreated, added)
}

// ── links ────────────────────────────────────────────────────────────────────

func (s *Server) listLinks(w http.ResponseWriter, r *http.Request) {
	m := s.member(w, r)
	if m == nil {
		return
	}
	links, err := s.db.ListLinks(r.Context(), m.ID)
	if err != nil {
		s.dbError(w, err, "links")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": links})
}

// createLink: the CUSTOMER links a vendor. A vendor can never link itself to a
// customer — that would let it grant itself visibility.
func (s *Server) createLink(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	if !m.IsCustomer() {
		apiError(w, http.StatusBadRequest, "org_kind", "only customer orgs can link vendors")
		return
	}
	var in struct {
		VendorSlug string `json:"vendor_slug"`
	}
	if !decode(w, r, &in) {
		return
	}
	vendor, err := s.db.GetOrgBySlug(r.Context(), in.VendorSlug)
	if err != nil {
		s.dbError(w, err, "vendor org")
		return
	}
	if vendor.ID == m.ID {
		apiError(w, http.StatusBadRequest, "self_link", "an org cannot link itself")
		return
	}
	if !vendor.IsVendor() {
		apiError(w, http.StatusBadRequest, "org_kind", "target org is not a vendor")
		return
	}
	l, err := s.db.CreateLink(r.Context(), m.ID, vendor.ID, userFrom(r).ID)
	if err != nil {
		s.dbError(w, err, "link")
		return
	}
	detail := map[string]any{"link_id": l.ID, "vendor_org_id": vendor.ID, "customer_org_id": m.ID}
	s.audit(r, m.ID, audit.ActionBridgeLinkCreated, detail)
	s.audit(r, vendor.ID, audit.ActionBridgeLinkCreated, detail)
	writeJSON(w, http.StatusCreated, l)
}

func (s *Server) revokeLink(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	l, err := s.db.RevokeLink(r.Context(), m.ID, r.PathValue("id"))
	if err != nil {
		s.dbError(w, err, "link")
		return
	}
	detail := map[string]any{"link_id": l.ID, "by_org_id": m.ID}
	s.audit(r, l.CustomerOrg.ID, audit.ActionBridgeLinkRevoked, detail)
	s.audit(r, l.VendorOrg.ID, audit.ActionBridgeLinkRevoked, detail)
	writeJSON(w, http.StatusOK, l)
}

// ── sites ────────────────────────────────────────────────────────────────────

type siteSecret struct {
	Site      *db.Site `json:"site"`
	Token     string   `json:"token"`     // shown once
	RelayKey  string   `json:"relay_key"` // pinned by the connector
	RelayURL  string   `json:"relay_url"`
	ConnectCm string   `json:"connect_command"`
}

func (s *Server) siteSecret(site *db.Site, token string) siteSecret {
	return siteSecret{
		Site: site, Token: token, RelayKey: s.signer.KeyString(), RelayURL: s.cfg.PublicURL,
		ConnectCm: fmt.Sprintf("tunr connect --relay %s --site %s --token %s --relay-key %s",
			s.cfg.PublicURL, site.Name, token, s.signer.KeyString()),
	}
}

func (s *Server) listSites(w http.ResponseWriter, r *http.Request) {
	m := s.member(w, r)
	if m == nil {
		return
	}
	sites, err := s.db.ListSites(r.Context(), m.ID)
	if err != nil {
		s.dbError(w, err, "sites")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": sites})
}

func (s *Server) createSite(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	if !m.IsCustomer() {
		apiError(w, http.StatusBadRequest, "org_kind", "only customer orgs can have sites")
		return
	}
	var in struct{ Name string }
	if !decode(w, r, &in) || !validName(w, "name", in.Name) {
		return
	}
	token, hash, err := auth.GenerateSiteToken()
	if err != nil {
		s.dbError(w, err, "site token")
		return
	}
	site, err := s.db.CreateSite(r.Context(), m.ID, in.Name, hash)
	if err != nil {
		s.dbError(w, err, "site")
		return
	}
	s.audit(r, m.ID, audit.ActionBridgeSiteCreated, map[string]any{"site_id": site.ID, "name": site.Name})
	writeJSON(w, http.StatusCreated, s.siteSecret(site, token))
}

func (s *Server) getSite(w http.ResponseWriter, r *http.Request) {
	m := s.member(w, r)
	if m == nil {
		return
	}
	site, err := s.db.GetSite(r.Context(), m.ID, r.PathValue("id"))
	if err != nil {
		s.dbError(w, err, "site")
		return
	}
	writeJSON(w, http.StatusOK, site)
}

func (s *Server) deleteSite(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	id := r.PathValue("id")
	if err := s.db.DeleteSite(r.Context(), m.ID, id); err != nil {
		s.dbError(w, err, "site")
		return
	}
	s.hub.Disconnect(id, "site deleted")
	s.audit(r, m.ID, audit.ActionBridgeSiteDeleted, map[string]any{"site_id": id})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) rotateSiteToken(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	site, err := s.db.GetSite(r.Context(), m.ID, r.PathValue("id"))
	if err != nil {
		s.dbError(w, err, "site")
		return
	}
	token, hash, err := auth.GenerateSiteToken()
	if err != nil {
		s.dbError(w, err, "site token")
		return
	}
	if err := s.db.RotateSiteToken(r.Context(), m.ID, site.ID, hash); err != nil {
		s.dbError(w, err, "site")
		return
	}
	s.hub.Disconnect(site.ID, "site token rotated")
	s.audit(r, m.ID, audit.ActionBridgeSiteTokenRotated, map[string]any{"site_id": site.ID})
	writeJSON(w, http.StatusOK, s.siteSecret(site, token))
}

// ── resources & catalog ──────────────────────────────────────────────────────

func (s *Server) listResources(w http.ResponseWriter, r *http.Request) {
	m := s.member(w, r)
	if m == nil {
		return
	}
	res, err := s.db.ListResources(r.Context(), m.ID)
	if err != nil {
		s.dbError(w, err, "resources")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resources": res})
}

// autoApprove is the only accepted shape of resources.auto_approve. Auto
// approval is read-only by construction: rw always needs two humans (§4 rule 2).
type autoApprove struct {
	VendorOrgID string `json:"vendor_org_id"`
	Mode        string `json:"mode"`
	MaxTTL      string `json:"max_ttl"`
}

const maxAutoApproveTTL = 24 * time.Hour

func (s *Server) patchResource(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	var in struct {
		Description *string         `json:"description"`
		AutoApprove json.RawMessage `json:"auto_approve"`
		Limits      json.RawMessage `json:"limits"`
		Status      *string         `json:"status"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Description != nil && len(*in.Description) > 500 {
		apiError(w, http.StatusBadRequest, "invalid_description", "description max 500 chars")
		return
	}
	if in.Status != nil && *in.Status != "active" && *in.Status != "disabled" {
		apiError(w, http.StatusBadRequest, "invalid_status", "status must be active or disabled")
		return
	}
	if in.Limits != nil {
		var lim auth.GrantLimits
		if err := strictUnmarshal(in.Limits, &lim); err != nil || lim.MaxRows < 0 || lim.MaxBytes < 0 || lim.QPS < 0 {
			apiError(w, http.StatusBadRequest, "invalid_limits", "limits: {max_rows, max_bytes, qps}, non-negative")
			return
		}
	}
	if in.AutoApprove != nil && string(in.AutoApprove) != "null" {
		var aa autoApprove
		if err := strictUnmarshal(in.AutoApprove, &aa); err != nil {
			apiError(w, http.StatusBadRequest, "invalid_auto_approve", err.Error())
			return
		}
		ttl, err := time.ParseDuration(aa.MaxTTL)
		if aa.Mode != "ro" || aa.VendorOrgID == "" || err != nil || ttl <= 0 || ttl > maxAutoApproveTTL {
			apiError(w, http.StatusBadRequest, "invalid_auto_approve",
				`auto_approve: {"vendor_org_id":"org_…","mode":"ro","max_ttl":"≤24h"}; rw is never auto-approved`)
			return
		}
		linked, err := s.db.HasActiveLink(r.Context(), aa.VendorOrgID, m.ID)
		if err != nil || !linked {
			apiError(w, http.StatusBadRequest, "invalid_auto_approve", "auto_approve vendor must be actively linked")
			return
		}
	}
	res, err := s.db.UpdateResource(r.Context(), m.ID, r.PathValue("id"), db.ResourcePatch{
		Description: in.Description, AutoApprove: in.AutoApprove, Limits: in.Limits, Status: in.Status,
	})
	if err != nil {
		s.dbError(w, err, "resource")
		return
	}
	s.audit(r, m.ID, audit.ActionBridgeResourceUpdated, map[string]any{"resource_id": res.ID})
	writeJSON(w, http.StatusOK, res)
}

func strictUnmarshal(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (s *Server) catalog(w http.ResponseWriter, r *http.Request) {
	m := s.member(w, r)
	if m == nil {
		return
	}
	cat, err := s.db.Catalog(r.Context(), m.ID)
	if err != nil {
		s.dbError(w, err, "catalog")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resources": cat})
}

// ── agents & tokens ──────────────────────────────────────────────────────────

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	m := s.member(w, r)
	if m == nil {
		return
	}
	agents, err := s.db.ListAgents(r.Context(), m.ID)
	if err != nil {
		s.dbError(w, err, "agents")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	if !m.IsVendor() {
		apiError(w, http.StatusBadRequest, "org_kind", "only vendor orgs can have agents")
		return
	}
	var in struct{ Name string }
	if !decode(w, r, &in) || !validName(w, "name", in.Name) {
		return
	}
	a, err := s.db.CreateAgent(r.Context(), m.ID, in.Name)
	if err != nil {
		s.dbError(w, err, "agent")
		return
	}
	s.audit(r, m.ID, audit.ActionBridgeAgentCreated, map[string]any{"agent_id": a.ID, "name": a.Name})
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) patchAgent(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	var in struct{ Status string }
	if !decode(w, r, &in) {
		return
	}
	if in.Status != "active" && in.Status != "disabled" {
		apiError(w, http.StatusBadRequest, "invalid_status", "status must be active or disabled")
		return
	}
	a, err := s.db.SetAgentStatus(r.Context(), m.ID, r.PathValue("id"), in.Status)
	if err != nil {
		s.dbError(w, err, "agent")
		return
	}
	s.audit(r, m.ID, audit.ActionBridgeAgentStatusChanged, map[string]any{"agent_id": a.ID, "status": a.Status})
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) listAgentTokens(w http.ResponseWriter, r *http.Request) {
	m := s.member(w, r)
	if m == nil {
		return
	}
	toks, err := s.db.ListAgentTokens(r.Context(), m.ID, r.PathValue("id"))
	if err != nil {
		s.dbError(w, err, "agent tokens")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": toks})
}

func (s *Server) issueAgentToken(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	var in struct{ TTL string }
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	ttl := auth.MaxAgentTokenTTL
	if in.TTL != "" {
		d, err := time.ParseDuration(in.TTL)
		if err != nil || d < time.Minute || d > auth.MaxAgentTokenTTL {
			apiError(w, http.StatusBadRequest, "invalid_ttl", "ttl must be between 1m and 24h")
			return
		}
		ttl = d
	}
	agentID := r.PathValue("id")
	rec, err := s.db.CreateAgentToken(r.Context(), m.ID, agentID, userFrom(r).ID, time.Now().Add(ttl))
	if err != nil {
		// Also ErrNotFound for a disabled agent: no tokens for disabled agents.
		s.dbError(w, err, "agent")
		return
	}
	tok, err := s.jwt.IssueAgent(m.ID, agentID, rec.ID, rec.ExpiresAt)
	if err != nil {
		s.dbError(w, err, "agent token")
		return
	}
	s.audit(r, m.ID, audit.ActionBridgeAgentTokenIssued, map[string]any{
		"agent_id": agentID, "token_id": rec.ID, "expires_at": rec.ExpiresAt,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"token": tok, "token_id": rec.ID, "agent_id": agentID, "expires_at": rec.ExpiresAt,
	})
}

func (s *Server) revokeAgentToken(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	agentID, tid := r.PathValue("id"), r.PathValue("tid")
	if err := s.db.RevokeAgentToken(r.Context(), m.ID, agentID, tid); err != nil {
		s.dbError(w, err, "agent token")
		return
	}
	s.audit(r, m.ID, audit.ActionBridgeAgentTokenRevoked, map[string]any{"agent_id": agentID, "token_id": tid})
	w.WriteHeader(http.StatusNoContent)
}
