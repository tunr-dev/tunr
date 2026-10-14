package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tunr-dev/tunr/relay/internal/db"
	"github.com/tunr-dev/tunr/relay/internal/db/dbtest"
)

func TestEnsurePersonalOrgIdempotent(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	uid := dbtest.User(t, d, "Ali.Veli+test@example.com")

	// Concurrent first calls must still produce exactly one personal org.
	var wg sync.WaitGroup
	ids := make([]string, 8)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o, err := d.EnsurePersonalOrg(ctx, uid, "Ali.Veli+test@example.com")
			if err != nil {
				t.Errorf("EnsurePersonalOrg: %v", err)
				return
			}
			ids[i] = o.ID
		}(i)
	}
	wg.Wait()
	for _, id := range ids[1:] {
		if id != ids[0] {
			t.Fatalf("got different personal orgs: %v", ids)
		}
	}

	orgs, err := d.ListOrgsForUser(ctx, uid)
	if err != nil || len(orgs) != 1 {
		t.Fatalf("ListOrgsForUser = %v, %v; want exactly one", orgs, err)
	}
	if o := orgs[0]; !o.Personal || o.Role != "owner" || o.Slug != "ali-veli-test" || o.Kind != "both" {
		t.Errorf("personal org = %+v", o)
	}

	// Same local part, different user → slug gets a suffix, not a failure.
	uid2 := dbtest.User(t, d, "ali.veli+test@other.example")
	o2, err := d.EnsurePersonalOrg(ctx, uid2, "ali.veli+test@other.example")
	if err != nil {
		t.Fatal(err)
	}
	if o2.Slug == "ali-veli-test" || o2.ID == ids[0] {
		t.Errorf("second personal org reused slug/id: %+v", o2)
	}
}

// fixture: vendor org V (user v) with an agent; customer org C (user c) with a
// site and resource, linked to V; and an unrelated org X (user x) that must see
// none of it.
type fixture struct {
	d                      *db.DB
	vUser, cUser, xUser    string
	vendor, customer, xOrg *db.Org
	site                   *db.Site
	res                    *db.Resource
	agent                  *db.Agent
	token                  *db.AgentToken
	grant                  *db.Grant
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{d: dbtest.New(t)}
	f.vUser = dbtest.User(t, f.d, "v@vendor.test")
	f.cUser = dbtest.User(t, f.d, "c@customer.test")
	f.xUser = dbtest.User(t, f.d, "x@other.test")

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var err error
	f.vendor, err = f.d.CreateOrg(ctx, f.vUser, "Vendor", "vendor-co", "vendor")
	must(err)
	f.customer, err = f.d.CreateOrg(ctx, f.cUser, "Acme", "acme", "customer")
	must(err)
	f.xOrg, err = f.d.CreateOrg(ctx, f.xUser, "Other", "other-co", "both")
	must(err)

	_, err = f.d.CreateLink(ctx, f.customer.ID, f.vendor.ID, f.cUser)
	must(err)
	f.site, err = f.d.CreateSite(ctx, f.customer.ID, "istanbul-ofis", "hash-1")
	must(err)
	f.res, err = f.d.UpsertResource(ctx, f.customer.ID, f.site.ID, "erp-db", "postgres")
	must(err)
	f.agent, err = f.d.CreateAgent(ctx, f.vendor.ID, "invoice-reconciler")
	must(err)
	f.token, err = f.d.CreateAgentToken(ctx, f.vendor.ID, f.agent.ID, f.vUser, time.Now().Add(time.Hour))
	must(err)
	f.grant, err = f.d.CreateGrantRequest(ctx, f.vendor.ID, f.agent.ID, f.res.ID, "ro", "October close", 2*time.Hour)
	must(err)
	return f
}

func TestBridgeHappyPath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	cat, err := f.d.Catalog(ctx, f.vendor.ID)
	if err != nil || len(cat) != 1 || cat[0].Address != "acme/erp-db" {
		t.Fatalf("Catalog = %+v, %v", cat, err)
	}
	if f.grant.Status != "pending" || f.grant.CustomerOrgID != f.customer.ID || f.grant.RequestedTTL != 2*time.Hour {
		t.Errorf("grant = %+v", f.grant)
	}
	ok, err := f.d.AgentTokenUsable(ctx, f.vendor.ID, f.agent.ID, f.token.ID)
	if err != nil || !ok {
		t.Fatalf("fresh token not usable: %v %v", ok, err)
	}
	if err := f.d.RevokeAgentToken(ctx, f.vendor.ID, f.agent.ID, f.token.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := f.d.AgentTokenUsable(ctx, f.vendor.ID, f.agent.ID, f.token.ID); ok {
		t.Error("revoked token still usable")
	}

	desc := "Logo ERP, read replica"
	r, err := f.d.UpdateResource(ctx, f.customer.ID, f.res.ID, db.ResourcePatch{
		Description: &desc, Limits: json.RawMessage(`{"max_rows":10000}`),
	})
	if err != nil || r.Description != desc || string(r.Limits) != `{"max_rows": 10000}` {
		t.Fatalf("UpdateResource = %+v (%s), %v", r, r.Limits, err)
	}

	// Revoking the link hides the catalog and blocks new grant requests.
	links, _ := f.d.ListLinks(ctx, f.vendor.ID)
	if _, err := f.d.RevokeLink(ctx, f.vendor.ID, links[0].ID); err != nil {
		t.Fatal(err)
	}
	if cat, _ := f.d.Catalog(ctx, f.vendor.ID); len(cat) != 0 {
		t.Errorf("catalog after link revoke = %+v", cat)
	}
	if _, err := f.d.CreateGrantRequest(ctx, f.vendor.ID, f.agent.ID, f.res.ID, "ro", "", time.Hour); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("grant request without link: err = %v, want ErrNotFound", err)
	}
}

// Every org-scoped read or write, attempted by an org that doesn't own the
// row, must behave exactly as if the row did not exist.
func TestBridgeTenantIsolation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	x := f.xOrg.ID
	notFound := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, db.ErrNotFound) {
			t.Errorf("%s by foreign org: err = %v, want ErrNotFound", name, err)
		}
	}
	empty := func(name string, n int, err error) {
		t.Helper()
		if err != nil || n != 0 {
			t.Errorf("%s by foreign org: %d rows, err %v; want 0", name, n, err)
		}
	}

	// Membership
	_, err := f.d.GetOrgForMember(ctx, f.xUser, f.customer.ID)
	notFound("GetOrgForMember(id)", err)
	_, err = f.d.GetOrgForMember(ctx, f.xUser, "acme")
	notFound("GetOrgForMember(slug)", err)

	// Sites
	_, err = f.d.GetSite(ctx, x, f.site.ID)
	notFound("GetSite", err)
	notFound("RotateSiteToken", f.d.RotateSiteToken(ctx, x, f.site.ID, "evil"))
	notFound("DeleteSite", f.d.DeleteSite(ctx, x, f.site.ID))
	sites, err := f.d.ListSites(ctx, x)
	empty("ListSites", len(sites), err)

	// Resources
	_, err = f.d.GetResource(ctx, x, f.res.ID)
	notFound("GetResource", err)
	off := "disabled"
	_, err = f.d.UpdateResource(ctx, x, f.res.ID, db.ResourcePatch{Status: &off})
	notFound("UpdateResource", err)
	_, err = f.d.UpsertResource(ctx, x, f.site.ID, "sneaky", "tcp")
	notFound("UpsertResource on foreign site", err)
	res, err := f.d.ListResources(ctx, x)
	empty("ListResources", len(res), err)
	cat, err := f.d.Catalog(ctx, x)
	empty("Catalog (unlinked)", len(cat), err)

	// Agents and tokens
	_, err = f.d.GetAgent(ctx, x, f.agent.ID)
	notFound("GetAgent", err)
	_, err = f.d.SetAgentStatus(ctx, x, f.agent.ID, "disabled")
	notFound("SetAgentStatus", err)
	_, err = f.d.CreateAgentToken(ctx, x, f.agent.ID, f.xUser, time.Now().Add(time.Hour))
	notFound("CreateAgentToken", err)
	toks, err := f.d.ListAgentTokens(ctx, x, f.agent.ID)
	empty("ListAgentTokens", len(toks), err)
	notFound("RevokeAgentToken", f.d.RevokeAgentToken(ctx, x, f.agent.ID, f.token.ID))
	if ok, _ := f.d.AgentTokenUsable(ctx, x, f.agent.ID, f.token.ID); ok {
		t.Error("AgentTokenUsable true for foreign org")
	}
	agents, err := f.d.ListAgents(ctx, x)
	empty("ListAgents", len(agents), err)

	// Links
	links, err := f.d.ListLinks(ctx, x)
	empty("ListLinks", len(links), err)
	own, _ := f.d.ListLinks(ctx, f.customer.ID)
	_, err = f.d.RevokeLink(ctx, x, own[0].ID)
	notFound("RevokeLink", err)

	// Grants: foreign org can neither see nor create against these rows.
	_, err = f.d.GetGrant(ctx, x, f.grant.ID)
	notFound("GetGrant", err)
	for _, side := range []string{"vendor", "customer"} {
		g, err := f.d.ListGrants(ctx, x, side, "")
		empty("ListGrants "+side, len(g), err)
	}
	_, err = f.d.CreateGrantRequest(ctx, x, f.agent.ID, f.res.ID, "ro", "", time.Hour)
	notFound("CreateGrantRequest with foreign agent", err)

	// Nothing above may have changed the owners' view.
	if s, err := f.d.GetSite(ctx, f.customer.ID, f.site.ID); err != nil || s.Name != "istanbul-ofis" {
		t.Errorf("owner lost site: %+v %v", s, err)
	}
	if a, err := f.d.GetAgent(ctx, f.vendor.ID, f.agent.ID); err != nil || a.Status != "active" {
		t.Errorf("owner's agent changed: %+v %v", a, err)
	}
	if ok, _ := f.d.AgentTokenUsable(ctx, f.vendor.ID, f.agent.ID, f.token.ID); !ok {
		t.Error("owner's token was revoked by a foreign org")
	}
}

func TestBridgeConflicts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.d.CreateOrg(ctx, f.xUser, "Dup", "acme", "both"); !errors.Is(err, db.ErrConflict) {
		t.Errorf("duplicate slug: %v", err)
	}
	if _, err := f.d.CreateSite(ctx, f.customer.ID, "istanbul-ofis", "hash-2"); !errors.Is(err, db.ErrConflict) {
		t.Errorf("duplicate site: %v", err)
	}
	if _, err := f.d.CreateAgent(ctx, f.vendor.ID, "invoice-reconciler"); !errors.Is(err, db.ErrConflict) {
		t.Errorf("duplicate agent: %v", err)
	}
	if err := f.d.AuditOrg(ctx, f.customer.ID, f.cUser, "bridge.test", map[string]any{"k": "v"}); err != nil {
		t.Errorf("AuditOrg: %v", err)
	}
}
