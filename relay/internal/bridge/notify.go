package bridge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"time"

	"github.com/tunr-dev/tunr/relay/internal/db"
	"github.com/tunr-dev/tunr/relay/internal/logger"
)

// notify.go — grant notifications (P4-T4): e-mail to the customer's
// approvers via Resend (same RESEND_API_KEY / RESEND_FROM as the dashboard),
// and HMAC-signed outgoing webhooks for both orgs.
//
// Delivery is best-effort and asynchronous: a slow mail provider or a dead
// webhook must never delay or fail a grant decision.

// Notifier sends grant events.
type Notifier struct {
	DB          *db.DB
	HTTP        *http.Client
	ResendKey   string
	From        string
	PortalURL   string                    // deep links, e.g. https://app.tunr.sh/bridge
	ResendURL   string                    // overridable in tests
	OnDelivered func(kind, target string) // tests
}

// GrantEvent is the webhook body.
type GrantEvent struct {
	Event string        `json:"event"` // grant.requested | grant.approved | grant.denied | grant.revoked | grant.expired | grant.first_approval
	Grant *db.GrantView `json:"grant"`
	At    time.Time     `json:"at"`
}

// Grant fans a grant event out to e-mail (requests only) and webhooks.
func (n *Notifier) Grant(event string, g *db.GrantView) {
	if n == nil || n.DB == nil || g == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if event == "grant.requested" || event == "grant.first_approval" {
			n.mailApprovers(ctx, event, g)
		}
		body, _ := json.Marshal(GrantEvent{Event: event, Grant: g, At: time.Now().UTC()})
		for _, org := range []string{g.CustomerOrgID, g.VendorOrgID} {
			hooks, err := n.DB.ListWebhooks(ctx, org)
			if err != nil {
				logger.Warn("bridge notify: list webhooks: %v", err)
				continue
			}
			for _, h := range hooks {
				n.post(ctx, h, body)
			}
		}
	}()
}

// SignWebhook returns the X-Tunr-Signature value for body at time t:
// "t=<unix>,v1=<hex hmac-sha256(secret, "<unix>.<body>")>". Receivers should
// reject timestamps older than a few minutes.
func SignWebhook(secret string, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func (n *Notifier) client() *http.Client {
	if n.HTTP != nil {
		return n.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (n *Notifier) post(ctx context.Context, h db.Webhook, body []byte) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "tunr-bridge-webhooks/1")
	req.Header.Set("X-Tunr-Signature", SignWebhook(h.Secret, time.Now(), body))
	resp, err := n.client().Do(req)
	if err != nil {
		logger.Warn("bridge webhook %s: %v", h.ID, err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		logger.Warn("bridge webhook %s: HTTP %d", h.ID, resp.StatusCode)
		return
	}
	if n.OnDelivered != nil {
		n.OnDelivered("webhook", h.URL)
	}
}

// GrantEmail renders the approval request mail (plain text + HTML).
func GrantEmail(event string, g *db.GrantView, portalURL string) (subject, text, htmlBody string) {
	what := "requests access"
	if event == "grant.first_approval" {
		what = "needs a second approval for read-write access"
	}
	subject = fmt.Sprintf("[tunr] %s %s to %s (%s, %s)", g.AgentName, what, g.Address, g.Mode,
		time.Duration(g.RequestedTTLSec)*time.Second)
	link := fmt.Sprintf("%s/grants/%s", portalURL, g.ID)
	text = fmt.Sprintf(`%s (vendor %s) %s:

  Resource:  %s (%s)
  Mode:      %s
  Duration:  %s
  Reason:    %s

Approve or deny: %s
`, g.AgentName, g.VendorSlug, what, g.Address, g.ResourceKind, g.Mode,
		time.Duration(g.RequestedTTLSec)*time.Second, g.Reason, link)
	e := html.EscapeString
	htmlBody = fmt.Sprintf(`<p><b>%s</b> (vendor <b>%s</b>) %s.</p>
<table><tr><td>Resource</td><td>%s (%s)</td></tr><tr><td>Mode</td><td>%s</td></tr>
<tr><td>Duration</td><td>%s</td></tr><tr><td>Reason</td><td>%s</td></tr></table>
<p><a href="%s">Approve or deny</a></p>`, e(g.AgentName), e(g.VendorSlug), e(what), e(g.Address), e(g.ResourceKind),
		e(g.Mode), e((time.Duration(g.RequestedTTLSec) * time.Second).String()), e(g.Reason), e(link))
	return subject, text, htmlBody
}

func (n *Notifier) mailApprovers(ctx context.Context, event string, g *db.GrantView) {
	to, err := n.DB.OrgApproverEmails(ctx, g.CustomerOrgID)
	if err != nil || len(to) == 0 {
		return
	}
	subject, text, htmlBody := GrantEmail(event, g, n.PortalURL)
	if n.ResendKey == "" {
		logger.Info("bridge notify (no RESEND_API_KEY): would mail %d approver(s): %s", len(to), subject)
		return
	}
	from := n.From
	if from == "" {
		from = "tunr <noreply@tunr.sh>"
	}
	body, _ := json.Marshal(map[string]any{"from": from, "to": to, "subject": subject, "text": text, "html": htmlBody})
	url := n.ResendURL
	if url == "" {
		url = "https://api.resend.com/emails"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+n.ResendKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client().Do(req)
	if err != nil {
		logger.Warn("bridge notify: resend: %v", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		logger.Warn("bridge notify: resend HTTP %d", resp.StatusCode)
		return
	}
	if n.OnDelivered != nil {
		n.OnDelivered("email", fmt.Sprint(to))
	}
}
