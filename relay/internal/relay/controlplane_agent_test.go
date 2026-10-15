package relay

import (
	"context"
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
