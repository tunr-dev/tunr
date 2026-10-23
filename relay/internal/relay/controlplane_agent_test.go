package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tunr-dev/tunr/relay/internal/db"
	"github.com/tunr-dev/tunr/relay/internal/db/dbtest"
)

func TestDeployAgentAppGuards(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	uid := dbtest.User(t, d, "dev@vendor.test")
	cp := NewControlPlane(nil, d, "tunr.test", NewRunnerClient("http://127.0.0.1:1", "s"), nil)
	if err := d.CreateApp(ctx, db.App{ID: "a_web", UserID: uid, Name: "site", EdgeSecret: "e"}); err != nil {
		t.Fatal(err)
	}
	src := strings.NewReader("x")
	noop := func(map[string]any) {}

	_, err := cp.DeployAgentApp(ctx, AgentDeploy{UserID: uid, ClaimPlan: "free", Name: "site"}, src, noop)
	if err == nil || !strings.Contains(err.Error(), "already a web app") {
		t.Fatalf("web app name reused for an agent: %v", err)
	}
	_, err = cp.DeployAgentApp(ctx, AgentDeploy{UserID: uid, ClaimPlan: "free", Name: "bot", VolumeMB: 4096}, src, noop)
	if err == nil || !strings.Contains(err.Error(), "MB volume") {
		t.Fatalf("free plan volume cap: %v", err)
	}
	// Free allows one hosted agent: with one already there, a second is refused.
	if err := d.CreateApp(ctx, db.App{ID: "a_agent", UserID: uid, Name: "first-bot", EdgeSecret: "e"}); err != nil {
		t.Fatal(err)
	}
	_ = d.QueryRowForTest(ctx, `UPDATE apps SET kind = 'agent' WHERE id = 'a_agent' RETURNING id`).Scan(new(string))
	_, err = cp.DeployAgentApp(ctx, AgentDeploy{UserID: uid, ClaimPlan: "free", Name: "second-bot"}, src, noop)
	if err == nil || !strings.Contains(err.Error(), "hosted agents") {
		t.Fatalf("second hosted agent on free: %v", err)
	}
	// And an agent can't be redeployed as a web app through the normal path.
	_, _, err = cp.deploy(ctx, uid, "free", deployRequest{Name: "first-bot", Kind: "web"}, src, noop)
	if err == nil || !strings.Contains(err.Error(), "hosted agent") {
		t.Fatalf("agent redeployed as web app: %v", err)
	}
}

// A hosted agent's app follows its org's admins, not whoever deployed it.
func TestAgentAppFollowsOrgAdmins(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	owner := dbtest.User(t, d, "owner@vendor.test")
	deployer := dbtest.User(t, d, "ex-admin@vendor.test")
	cp := NewControlPlane(nil, d, "tunr.test", NewRunnerClient("http://127.0.0.1:1", "s"), nil)
	org, err := d.CreateOrg(ctx, owner, "Vendor", "vendor-co", "vendor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddOrgMember(ctx, org.ID, "ex-admin@vendor.test", "admin", true); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateApp(ctx, db.App{ID: "a_bot", UserID: deployer, Name: "bot", EdgeSecret: "e"}); err != nil {
		t.Fatal(err)
	}
	ag, err := d.CreateAgent(ctx, org.ID, "bot")
	if err != nil {
		t.Fatal(err)
	}
	_ = d.QueryRowForTest(ctx, `UPDATE apps SET kind = 'agent' WHERE id = 'a_bot' RETURNING id`).Scan(new(string))
	_ = d.QueryRowForTest(ctx, `UPDATE agents SET app_id = 'a_bot' WHERE id = $1 RETURNING id`, ag.ID).Scan(new(string))

	app, _, _ := d.GetAppByName(ctx, "bot")
	if !cp.canManage(ctx, app, deployer) || !cp.canManage(ctx, app, owner) {
		t.Fatal("current admins must manage the agent app")
	}
	// Demoted: the deployer loses control although apps.user_id is still theirs.
	if _, err := d.AddOrgMember(ctx, org.ID, "ex-admin@vendor.test", "member", true); err != nil {
		t.Fatal(err)
	}
	if cp.canManage(ctx, app, deployer) {
		t.Fatal("demoted deployer still manages the agent app")
	}
	rec := httptest.NewRecorder()
	cp.deleteApp(rec, httptest.NewRequest(http.MethodDelete, "/v1/apps?name=bot", nil), deployer)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("demoted deployer deleted the agent app: %d", rec.Code)
	}
}
