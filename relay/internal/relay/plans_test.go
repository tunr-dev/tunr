package relay

import (
	"context"
	"strings"
	"testing"
)

func TestQuotaFor(t *testing.T) {
	cases := []struct {
		plan    string
		apps    int
		memMB   int
		tunnels int
	}{
		{"free", 3, 256, 2},
		{"pro", 25, 512, 10},
		{"team", 100, 1024, 25},
		{"", 3, 256, 2},         // missing plan → free
		{"platinum", 3, 256, 2}, // unknown plan → free, never unlimited
	}
	for _, c := range cases {
		q := quotaFor(c.plan)
		if q.Apps != c.apps || q.AppMemoryMB != c.memMB || q.ConcurrentTunnels != c.tunnels {
			t.Errorf("quotaFor(%q) = %+v", c.plan, q)
		}
	}
}

func TestResolveUserPlanWithoutDB(t *testing.T) {
	ctx := context.Background()
	if got := resolveUserPlan(ctx, nil, "u1", "pro"); got != "pro" {
		t.Errorf("claim fallback: got %q, want pro", got)
	}
	if got := resolveUserPlan(ctx, nil, "u1", ""); got != "free" {
		t.Errorf("empty claim: got %q, want free", got)
	}
}

func TestQuotaMessagesPointAtTheFix(t *testing.T) {
	free := appQuotaMsg("free", quotaFor("free"))
	if !strings.Contains(free, "3 apps") || !strings.Contains(free, "tunr apps delete") || !strings.Contains(free, "Upgrade") {
		t.Errorf("free app message missing count/fix/upgrade: %q", free)
	}
	if pro := tunnelQuotaMsg("pro", quotaFor("pro")); strings.Contains(pro, "Upgrade") {
		t.Errorf("pro users shouldn't be told to upgrade: %q", pro)
	}
}
