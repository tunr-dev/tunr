package bridge

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tunr-dev/tunr/relay/internal/db"
	"github.com/tunr-dev/tunr/relay/internal/db/dbtest"
)

// reconnect replaces the fake connector with one announcing decls, using a
// freshly rotated site token. It returns the welcome or the dial/read error.
func (e *p4) reconnect(decls ...ResourceDecl) (*websocket.Conn, Message, error) {
	e.t.Helper()
	var sec siteSecret
	e.call(e.owner, "POST", "/orgs/acme/sites/"+e.siteID+"/rotate-token", nil, 200, &sec)
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.srv.URL, "http")+"/bridge/connect",
		http.Header{"Authorization": {"Bearer " + sec.Token}})
	if err != nil {
		return nil, Message{}, err
	}
	e.t.Cleanup(func() { c.Close() })
	_ = c.WriteJSON(msg(MsgHello, HelloData{ConnectorVersion: "test", Resources: decls}))
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var w Message
	err = c.ReadJSON(&w)
	return c, w, err
}

func (e *p4) accessCheck(resource string) int {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+"/bridge/access/check?resource="+resource, nil)
	req.Header.Set("Authorization", "Bearer "+e.agentTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestMSSQLResourceAndProfile(t *testing.T) {
	e := newP4(t)
	verified := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
	_, w, err := e.reconnect(
		ResourceDecl{Name: "erp-db", Kind: "postgres"},
		ResourceDecl{Name: "nebim", Kind: "mssql", Profile: "nebim-v3",
			ProfileTables: []string{"cdItem", "trStock", "dbo.cdCurrAcc"}, ProfileVerifiedAt: verified},
	)
	if err != nil || w.Type != MsgWelcome {
		t.Fatalf("welcome: %v %+v", err, w)
	}

	var cat struct{ Resources []CatalogItem }
	e.call(e.agentTok, "GET", "/agent/catalog", nil, 200, &cat)
	var nebim *CatalogItem
	for i := range cat.Resources {
		if cat.Resources[i].Address == "acme/nebim" {
			nebim = &cat.Resources[i]
		}
	}
	if nebim == nil || nebim.Kind != "mssql" || nebim.Profile != "nebim-v3" || len(nebim.ProfileTables) != 3 ||
		nebim.ProfileVerifiedAt == nil || !nebim.ProfileVerifiedAt.Equal(time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("catalog entry = %+v", nebim)
	}

	if g := e.request("", "acme/nebim", "1h", 201); g.Mode != "ro" || g.ResourceKind != "mssql" {
		t.Errorf("mssql grant = %+v", g)
	}
	e.request("rw", "acme/nebim", "1h", 400)

	// A profile that isn't plain is refused at hello.
	for _, bad := range []ResourceDecl{
		{Name: "x1", Kind: "mssql", Profile: "Nebim V3"},
		{Name: "x2", Kind: "mssql", Profile: "nebim-v3", ProfileTables: []string{"t; DROP"}},
		{Name: "x3", Kind: "mssql", ProfileTables: []string{"cdItem"}},
		{Name: "x4", Kind: "mssql", Profile: "nebim-v3", ProfileVerifiedAt: "yesterday"},
		{Name: "x5", Kind: "oracle"},
	} {
		c, w, err := e.reconnect(bad)
		if err == nil && w.Type == MsgWelcome {
			t.Errorf("hello with %+v accepted", bad)
		}
		if c != nil {
			c.Close()
		}
	}
}

func TestGrantActsOnBehalfOfIssuer(t *testing.T) {
	e := newP4(t)
	g := e.request("", "acme/erp-db", "1h", 201)
	if g.OnBehalfOf != "dev@vendor.test" || g.RequestedBy == "" {
		t.Fatalf("grant should act for the token's issuer: %+v", g)
	}
	e.call(e.owner, "POST", "/orgs/acme/grants/"+g.ID+"/approve", nil, 200, nil)
	if code := e.accessCheck("acme/erp-db"); code != 200 {
		t.Fatalf("access check = %d, want 200", code)
	}

	// The issuer leaves the vendor org: the grant stops working at once, the
	// customer sees why, and the agent can't ask for a new one with that token.
	var n int
	if err := e.d.QueryRowForTest(context.Background(), `
		WITH d AS (DELETE FROM org_members m USING users u
		           WHERE m.user_id = u.id AND u.email = 'dev@vendor.test' RETURNING 1)
		SELECT count(*) FROM d`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("remove member: %v (%d)", err, n)
	}
	if code := e.accessCheck("acme/erp-db"); code != http.StatusForbidden {
		t.Errorf("access after the issuer left = %d, want 403", code)
	}
	var seen db.GrantView
	e.call(e.owner, "GET", "/orgs/acme/grants/"+g.ID, nil, 200, &seen)
	if !seen.OnBehalfGone || seen.OnBehalfOf != "dev@vendor.test" {
		t.Errorf("customer view = %+v", seen)
	}
	e.request("", "acme/erp-db", "1h", http.StatusForbidden)
}

func TestConnectInvite(t *testing.T) {
	e := newP4(t)
	var created struct {
		Invite db.Invite
		Token  string
		URL    string
	}
	e.call(e.vendor, "POST", "/orgs/vendor-co/invites", map[string]string{"customer_hint": "Kuzucular", "note": "ERP'nizi bağlayın"}, 201, &created)
	if !strings.HasPrefix(created.Token, invitePrefix) || created.URL != "https://portal.test/connect/"+created.Token ||
		created.Invite.Status != "pending" {
		t.Fatalf("invite = %+v", created)
	}
	// Only the vendor's admins create invites; a customer org can't.
	e.call(e.owner, "POST", "/orgs/acme/invites", map[string]string{}, 400, nil)

	// Anyone with the link sees who is asking — and nothing else.
	resp, err := http.Get(e.srv.URL + "/api/v1/bridge/invites/" + created.Token)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("preview: %v %v", err, resp)
	}
	resp.Body.Close()
	if r, _ := http.Get(e.srv.URL + "/api/v1/bridge/invites/tunr_inv_nope"); r.StatusCode != 404 {
		t.Errorf("unknown invite = %d", r.StatusCode)
	}

	// A new customer accepts: org, link and first site in one step.
	newbie := dbtest.User(t, e.d, "it@kuzucular.test")
	tok, _ := e.s.jwt.Issue(newbie, "it@kuzucular.test", "free")
	var acc struct {
		Org  db.OrgMembership
		Link db.Link
		Site siteSecret
	}
	e.call(tok, "POST", "/invites/"+created.Token+"/accept",
		map[string]string{"org_name": "Kuzucular Yapı", "org_slug": "kuzucular", "site_name": "merkez"}, 201, &acc)
	if acc.Org.Slug != "kuzucular" || acc.Org.Role != "owner" || acc.Link.Status != "active" ||
		!strings.HasPrefix(acc.Site.Token, "tunr_site_") || !strings.Contains(acc.Site.ConnectCm, "--site merkez") {
		t.Fatalf("accept = %+v", acc)
	}
	// One use only.
	e.call(tok, "POST", "/invites/"+created.Token+"/accept", map[string]string{"org": "kuzucular"}, 410, nil)

	var list struct{ Invites []db.Invite }
	e.call(e.vendor, "GET", "/orgs/vendor-co/invites", nil, 200, &list)
	if len(list.Invites) != 1 || list.Invites[0].Status != "accepted" || list.Invites[0].CustomerSlug != "kuzucular" {
		t.Errorf("vendor's list = %+v", list.Invites)
	}

	// A revoked invite can't be accepted.
	e.call(e.vendor, "POST", "/orgs/vendor-co/invites", map[string]string{}, 201, &created)
	e.call(e.vendor, "DELETE", "/orgs/vendor-co/invites/"+created.Invite.ID, nil, 204, nil)
	e.call(tok, "POST", "/invites/"+created.Token+"/accept", map[string]string{"org": "kuzucular"}, 410, nil)
}
