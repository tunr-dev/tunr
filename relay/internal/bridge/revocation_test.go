package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/tunr-dev/tunr/relay/internal/db"
)

func (e *p4) grantStatus(id string) string {
	e.t.Helper()
	g, err := e.d.GetGrantView(context.Background(), mustOrg(e.t, e.d, "acme"), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return g.Status
}

func (e *p4) resourceID(name string) string {
	e.t.Helper()
	res, _ := e.d.ListResources(context.Background(), mustOrg(e.t, e.d, "acme"))
	for _, r := range res {
		if r.Name == name {
			return r.ID
		}
	}
	e.t.Fatalf("no resource %s", name)
	return ""
}

// Each way of withdrawing access ends live streams and the grants behind them.
func TestRevocationCascades(t *testing.T) {
	cases := map[string]func(e *p4){
		"link revoked": func(e *p4) {
			var out struct{ Links []db.Link }
			e.call(e.owner, "GET", "/orgs/acme/links", nil, 200, &out)
			e.call(e.owner, "DELETE", "/orgs/acme/links/"+out.Links[0].ID, nil, 200, nil)
		},
		"resource disabled": func(e *p4) {
			e.call(e.owner, "PATCH", "/orgs/acme/resources/"+e.resourceID("nas"), map[string]any{"status": "disabled"}, 200, nil)
		},
		"agent disabled": func(e *p4) {
			e.call(e.vendor, "PATCH", "/orgs/vendor-co/agents/"+e.agentID, map[string]any{"status": "disabled"}, 200, nil)
		},
	}
	for name, withdraw := range cases {
		t.Run(name, func(t *testing.T) {
			e := newP4(t)
			gid := e.approveRW()
			pending := e.request("rw", "acme/nas", "1h", 201)
			a := e.openStream()
			defer a.Close()

			withdraw(e)
			waitClosed(t, a, "", 2*time.Second)
			e.expectKill(gid)
			if s := e.grantStatus(gid); s != "revoked" {
				t.Errorf("approved grant after withdrawal: %s", s)
			}
			if s := e.grantStatus(pending.ID); s != "denied" {
				t.Errorf("pending grant after withdrawal: %s", s)
			}
		})
	}
}

func TestAgentTokenRevokeEndsStreams(t *testing.T) {
	e := newP4(t)
	gid := e.approveRW()
	a := e.openStream()
	defer a.Close()
	var toks struct{ Tokens []db.AgentToken }
	e.call(e.vendor, "GET", "/orgs/vendor-co/agents/"+e.agentID+"/tokens", nil, 200, &toks)
	e.call(e.vendor, "DELETE", "/orgs/vendor-co/agents/"+e.agentID+"/tokens/"+toks.Tokens[0].ID, nil, 204, nil)
	waitClosed(t, a, "token_revoked", 2*time.Second)
	if s := e.grantStatus(gid); s != "approved" {
		t.Errorf("grant after token revoke: %s (other tokens may still use it)", s)
	}
}

// After "cut all access" a fresh request needs a person again.
func TestKillSiteTurnsOffAutoApprove(t *testing.T) {
	e := newP4(t)
	vend := mustOrg(t, e.d, "vendor-co")
	e.call(e.owner, "PATCH", "/orgs/acme/resources/"+e.resourceID("erp-db"), map[string]any{
		"auto_approve": map[string]string{"vendor_org_id": vend, "mode": "ro", "max_ttl": "2h"}}, 200, nil)
	if g := e.request("", "acme/erp-db", "1h", 201); g.Status != "approved" {
		t.Fatalf("setup: %s", g.Status)
	}
	half := e.approveHalfRW()
	e.call(e.owner, "POST", "/orgs/acme/sites/"+e.siteID+"/kill", nil, 200, nil)
	if g := e.request("", "acme/erp-db", "1h", 201); g.Status != "pending" {
		t.Errorf("request after kill: %s, want pending", g.Status)
	}
	if s := e.grantStatus(half); s != "denied" {
		t.Errorf("half-approved rw grant after kill: %s", s)
	}
}

func (e *p4) approveHalfRW() string {
	g := e.request("rw", "acme/nas", "1h", 201)
	e.call(e.owner, "POST", "/orgs/acme/grants/"+g.ID+"/approve", nil, 200, nil)
	return g.ID
}
