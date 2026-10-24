package bridge

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tunr-dev/tunr/relay/internal/audit"
	"github.com/tunr-dev/tunr/relay/internal/auth"
	"github.com/tunr-dev/tunr/relay/internal/db"
)

// invites.go — the vendor's one-link onboarding: "connect your ERP to us".
//
// A vendor admin creates an invite and sends the link to a customer. The
// customer signs in, and accepting does what they could do by hand in the
// portal: create (or pick) their customer org, link the vendor, and create a
// site — then shows the connector install command. The invite grants nothing:
// every grant is still requested and approved one by one.

const (
	invitePrefix     = "tunr_inv_"
	defaultInviteTTL = 7 * 24 * time.Hour
	maxInviteTTL     = 30 * 24 * time.Hour
)

func (s *Server) registerInvites(mux *http.ServeMux) {
	h := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, s.humanAuth(fn)) }
	const p = "/api/v1/bridge"
	h("GET "+p+"/orgs/{org}/invites", s.listInvites)
	h("POST "+p+"/orgs/{org}/invites", s.createInvite)
	h("DELETE "+p+"/orgs/{org}/invites/{id}", s.revokeInvite)
	mux.HandleFunc("GET "+p+"/invites/{token}", s.previewInvite) // no auth: shows only the vendor's name
	h("POST "+p+"/invites/{token}/accept", s.acceptInvite)
}

func hashInvite(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// connectURL is where the customer opens the invite: the relay's own portal
// (#/connect/<token>) when PortalURL points at it, else the dashboard's
// /connect/<token> on PortalURL's origin.
func (s *Server) connectURL(token string) string {
	u, err := url.Parse(s.cfg.PortalURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "https://app.tunr.sh/connect/" + token
	}
	if strings.TrimRight(u.Path, "/") == "/portal" {
		return u.Scheme + "://" + u.Host + "/portal/#/connect/" + token
	}
	return u.Scheme + "://" + u.Host + "/connect/" + token
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	if !m.IsVendor() {
		apiError(w, http.StatusBadRequest, "org_kind", "only vendor orgs send connect invites")
		return
	}
	var in struct {
		CustomerHint string `json:"customer_hint"`
		Note         string `json:"note"`
		TTL          string `json:"ttl"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.CustomerHint, in.Note = strings.TrimSpace(in.CustomerHint), strings.TrimSpace(in.Note)
	if len(in.CustomerHint) > 100 || len(in.Note) > 500 {
		apiError(w, http.StatusBadRequest, "invalid_request", "customer_hint max 100 chars, note max 500")
		return
	}
	ttl := defaultInviteTTL
	if in.TTL != "" {
		d, err := time.ParseDuration(in.TTL)
		if err != nil || d < time.Hour || d > maxInviteTTL {
			apiError(w, http.StatusBadRequest, "invalid_ttl", "ttl must be between 1h and 720h")
			return
		}
		ttl = d
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		s.dbError(w, err, "invite token")
		return
	}
	token := invitePrefix + base64.RawURLEncoding.EncodeToString(b)
	inv, err := s.db.CreateInvite(r.Context(), m.ID, hashInvite(token), in.CustomerHint, in.Note, userFrom(r).ID, time.Now().Add(ttl))
	if err != nil {
		s.dbError(w, err, "invite")
		return
	}
	s.audit(r, m.ID, audit.ActionBridgeInviteCreated, map[string]any{"invite_id": inv.ID, "customer_hint": inv.CustomerHint})
	writeJSON(w, http.StatusCreated, map[string]any{"invite": inv, "token": token, "url": s.connectURL(token)})
}

func (s *Server) listInvites(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	invs, err := s.db.ListInvites(r.Context(), m.ID)
	if err != nil {
		s.dbError(w, err, "invites")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": invs})
}

func (s *Server) revokeInvite(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	if err := s.db.RevokeInvite(r.Context(), m.ID, r.PathValue("id")); err != nil {
		s.dbError(w, err, "pending invite")
		return
	}
	s.audit(r, m.ID, audit.ActionBridgeInviteRevoked, map[string]any{"invite_id": r.PathValue("id")})
	w.WriteHeader(http.StatusNoContent)
}

// invitePreview is what anyone holding the link may see.
type invitePreview struct {
	VendorSlug   string    `json:"vendor_slug"`
	VendorName   string    `json:"vendor_name"`
	CustomerHint string    `json:"customer_hint,omitempty"`
	Note         string    `json:"note,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	Status       string    `json:"status"`
}

func (s *Server) previewInvite(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if ok, wait := s.apiLimit.allow("ip:"+s.clientIP(r), time.Now()); !ok {
		rateLimited(w, wait)
		return
	}
	tok := r.PathValue("token")
	if !strings.HasPrefix(tok, invitePrefix) || len(tok) > 100 {
		apiError(w, http.StatusNotFound, "not_found", "invite not found")
		return
	}
	inv, err := s.db.InviteByToken(r.Context(), hashInvite(tok))
	if err != nil {
		s.dbError(w, err, "invite")
		return
	}
	writeJSON(w, http.StatusOK, invitePreview{VendorSlug: inv.VendorSlug, VendorName: inv.VendorName,
		CustomerHint: inv.CustomerHint, Note: inv.Note, ExpiresAt: inv.ExpiresAt, Status: inv.Status})
}

// acceptInvite: body {"org": "<existing customer org slug>"} or
// {"org_name": "…", "org_slug": "…"} to create one, plus optional
// {"site_name": "…"} to create the first site.
func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Org      string `json:"org"`
		OrgName  string `json:"org_name"`
		OrgSlug  string `json:"org_slug"`
		SiteName string `json:"site_name"`
	}
	if !decode(w, r, &in) {
		return
	}
	tok := r.PathValue("token")
	if !strings.HasPrefix(tok, invitePrefix) || len(tok) > 100 {
		apiError(w, http.StatusNotFound, "not_found", "invite not found")
		return
	}
	hash := hashInvite(tok)
	inv, err := s.db.InviteByToken(r.Context(), hash)
	if err != nil {
		s.dbError(w, err, "invite")
		return
	}
	if inv.Status != "pending" {
		apiError(w, http.StatusGone, "invite_"+inv.Status, "this invite is "+inv.Status+"; ask the vendor for a new link")
		return
	}
	if in.SiteName != "" && !validName(w, "site_name", in.SiteName) {
		return
	}
	u := userFrom(r)

	var cust *db.OrgMembership
	switch {
	case in.Org != "":
		cust, err = s.db.GetOrgForMember(r.Context(), u.ID, in.Org)
		if err != nil {
			s.dbError(w, err, "org")
			return
		}
		if cust.Role != "owner" && cust.Role != "admin" {
			apiError(w, http.StatusForbidden, "forbidden", "requires owner or admin role in "+cust.Slug)
			return
		}
		if !cust.IsCustomer() {
			apiError(w, http.StatusBadRequest, "org_kind", cust.Slug+" is not a customer org")
			return
		}
	default:
		if !validName(w, "org_slug", in.OrgSlug) {
			return
		}
		in.OrgName = strings.TrimSpace(in.OrgName)
		if in.OrgName == "" || len(in.OrgName) > 100 {
			apiError(w, http.StatusBadRequest, "invalid_name", "org_name is required (max 100 chars)")
			return
		}
		o, err := s.db.CreateOrg(r.Context(), u.ID, in.OrgName, in.OrgSlug, "customer")
		if err != nil {
			s.dbError(w, err, "org")
			return
		}
		s.audit(r, o.ID, audit.ActionBridgeOrgCreated, map[string]any{"slug": o.Slug, "kind": o.Kind, "via_invite": inv.ID})
		cust = &db.OrgMembership{Org: *o, Role: "owner"}
	}
	if cust.ID == inv.VendorOrgID {
		apiError(w, http.StatusBadRequest, "self_link", "an org cannot link itself")
		return
	}

	inv, err = s.db.ConsumeInvite(r.Context(), hash, u.ID, cust.ID)
	if errors.Is(err, db.ErrInviteUsed) {
		apiError(w, http.StatusGone, "invite_used", err.Error())
		return
	}
	if err != nil {
		s.dbError(w, err, "invite")
		return
	}
	link, err := s.db.CreateLink(r.Context(), cust.ID, inv.VendorOrgID, u.ID)
	if err != nil {
		s.dbError(w, err, "link")
		return
	}
	detail := map[string]any{"link_id": link.ID, "vendor_org_id": inv.VendorOrgID, "customer_org_id": cust.ID, "invite_id": inv.ID}
	s.audit(r, cust.ID, audit.ActionBridgeLinkCreated, detail)
	s.audit(r, inv.VendorOrgID, audit.ActionBridgeLinkCreated, detail)
	s.audit(r, inv.VendorOrgID, audit.ActionBridgeInviteAccepted, detail)

	out := map[string]any{"org": cust, "link": link}
	if in.SiteName != "" {
		token, thash, err := auth.GenerateSiteToken()
		if err != nil {
			s.dbError(w, err, "site token")
			return
		}
		site, err := s.db.CreateSite(r.Context(), cust.ID, in.SiteName, thash)
		if err != nil {
			s.dbError(w, err, "site")
			return
		}
		s.audit(r, cust.ID, audit.ActionBridgeSiteCreated, map[string]any{"site_id": site.ID, "name": site.Name, "via_invite": inv.ID})
		out["site"] = s.siteSecret(site, token)
	}
	writeJSON(w, http.StatusCreated, out)
}
