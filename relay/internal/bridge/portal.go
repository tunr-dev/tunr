package bridge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tunr-dev/tunr/relay/internal/logger"
)

// portal.go — the embedded customer portal / vendor console (P6, roadmap
// decision K1: embedded for the demo and self-hosters; the Next.js dashboard
// can adopt the same API later).
//
// Static files under /portal/ with a strict CSP (no inline script, same-
// origin only). Login is a magic link compatible with the dashboard's
// magic_tokens scheme: sha256(token) stored, single use, 15 minutes. The
// token travels in the URL *fragment*, so it never reaches server logs or
// Referer headers. The answer to a login request never says whether the
// address has an account.

//go:embed portal/*
var portalFiles embed.FS

const portalCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

var emailRe = regexp.MustCompile(`^[^\s@]{1,64}@[^\s@]{1,253}\.[^\s@]{2,}$`)

// loginLimiter: per e-mail and global sliding windows (in memory; one relay).
type loginLimiter struct {
	mu     sync.Mutex
	byMail map[string][]time.Time
	all    []time.Time
}

func (l *loginLimiter) allow(email string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	prune := func(ts []time.Time, win time.Duration) []time.Time {
		out := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < win {
				out = append(out, t)
			}
		}
		return out
	}
	l.all = prune(l.all, time.Minute)
	l.byMail[email] = prune(l.byMail[email], 10*time.Minute)
	if len(l.all) >= 30 || len(l.byMail[email]) >= 5 {
		return false
	}
	l.all = append(l.all, now)
	l.byMail[email] = append(l.byMail[email], now)
	return true
}

func (s *Server) registerPortal(mux *http.ServeMux) {
	sub, _ := fs.Sub(portalFiles, "portal")
	static := http.StripPrefix("/portal/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /portal", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/portal/", http.StatusMovedPermanently)
	})
	mux.Handle("GET /portal/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", portalCSP)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	}))
	s.logins = &loginLimiter{byMail: map[string][]time.Time{}}
	mux.HandleFunc("POST /api/v1/bridge/portal/login", s.portalLogin)
	mux.HandleFunc("POST /api/v1/bridge/portal/verify", s.portalVerify)

	h := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, s.humanAuth(fn)) }
	h("GET /api/v1/bridge/me", s.portalMe)
	h("GET /api/v1/bridge/orgs/{org}/settings", s.getOrgSettings)
	h("PATCH /api/v1/bridge/orgs/{org}/settings", s.patchOrgSettings)
}

func (s *Server) portalLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var in struct{ Email string }
	if !decode(w, r, &in) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if !emailRe.MatchString(email) {
		apiError(w, http.StatusBadRequest, "invalid_email", "enter a valid e-mail address")
		return
	}
	if !s.logins.allow(email, time.Now()) {
		w.Header().Set("Retry-After", "600")
		apiError(w, http.StatusTooManyRequests, "rate_limited", "too many login links requested; try again in a few minutes")
		return
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	if err := s.db.SavePortalMagicToken(ctx, email, hex.EncodeToString(sum[:]), time.Now().Add(15*time.Minute)); err != nil {
		s.dbError(w, err, "login")
		return
	}
	link := fmt.Sprintf("%s/portal/#/verify?email=%s&token=%s", s.cfg.PublicURL, url.QueryEscape(email), token)
	text := "Sign in to tunr Bridge:\n\n" + link + "\n\nThe link works once and expires in 15 minutes. If you didn't ask for it, ignore this e-mail.\n"
	htmlBody := `<p>Sign in to <b>tunr Bridge</b>:</p><p><a href="` + html.EscapeString(link) +
		`">Sign in</a></p><p style="color:#666">The link works once and expires in 15 minutes. If you didn't ask for it, ignore this e-mail.</p>`
	sent := s.notify.SendMail(ctx, []string{email}, "Your tunr Bridge sign-in link", text, htmlBody)

	out := map[string]any{"ok": true, "message": "If the address can receive mail, a sign-in link is on its way."}
	if s.cfg.DevMode && !sent {
		// Local development without a mail provider: hand the link back.
		logger.Info("portal login link (dev): %s", link)
		out["dev_link"] = link
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) portalVerify(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var in struct{ Email, Token string }
	if !decode(w, r, &in) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	sum := sha256.Sum256([]byte(in.Token))
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	ok, err := s.db.ConsumePortalMagicToken(ctx, email, hex.EncodeToString(sum[:]))
	if err != nil {
		s.dbError(w, err, "login")
		return
	}
	if !ok || len(in.Token) != 64 {
		apiError(w, http.StatusUnauthorized, "invalid_link", "this sign-in link is invalid, used or expired")
		return
	}
	u, _, err := s.db.GetOrCreateUser(ctx, email)
	if err != nil {
		s.dbError(w, err, "user")
		return
	}
	tok, err := s.jwt.Issue(u.ID, u.Email, u.Plan)
	if err != nil {
		s.dbError(w, err, "token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok, "email": u.Email, "user_id": u.ID})
}

func (s *Server) portalMe(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	writeJSON(w, http.StatusOK, map[string]string{"user_id": u.ID, "email": u.Email})
}

// Org settings the portal may change. Anything else is refused.
func (s *Server) getOrgSettings(w http.ResponseWriter, r *http.Request) {
	m := s.member(w, r)
	if m == nil {
		return
	}
	st, err := s.db.OrgSettings(r.Context(), m.ID)
	if err != nil {
		s.dbError(w, err, "settings")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) patchOrgSettings(w http.ResponseWriter, r *http.Request) {
	m := s.admin(w, r)
	if m == nil {
		return
	}
	var in struct {
		LogQueryText *bool   `json:"log_query_text"`
		MaxGrantTTL  *string `json:"max_grant_ttl"`
	}
	if !decode(w, r, &in) {
		return
	}
	patch := map[string]any{}
	if in.LogQueryText != nil {
		patch["log_query_text"] = *in.LogQueryText
	}
	if in.MaxGrantTTL != nil {
		d, err := time.ParseDuration(*in.MaxGrantTTL)
		if err != nil || d < time.Hour || d > 24*time.Hour {
			apiError(w, http.StatusBadRequest, "invalid_max_grant_ttl", "max_grant_ttl must be between 1h and 24h")
			return
		}
		patch["max_grant_ttl"] = d.String()
	}
	if len(patch) == 0 {
		apiError(w, http.StatusBadRequest, "invalid_request", "nothing to change")
		return
	}
	st, err := s.db.UpdateOrgSettings(r.Context(), m.ID, patch)
	if err != nil {
		s.dbError(w, err, "settings")
		return
	}
	s.audit(r, m.ID, "bridge.settings_changed", patch)
	writeJSON(w, http.StatusOK, st)
}
