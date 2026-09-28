package relay

import (
	"context"
	"fmt"

	"github.com/tunr-dev/tunr/relay/internal/db"
)

// PlanQuota is what a plan is allowed to hold at once. The pricing page must
// only advertise limits that live here — anything else is a promise the relay
// doesn't keep.
type PlanQuota struct {
	Apps              int // deployed apps (redeploying an existing app is always allowed)
	AppMemoryMB       int // memory limit handed to the runner per app
	DeploysPerDay     int // rolling 24 h, across all of the user's apps
	ConcurrentTunnels int // open tunnels per signed-in user
}

// planQuotas — keep in sync with the Free/Pro cards on tunr.sh.
// Per-minute request limits live in rate_limiter.go (planLimits).
var planQuotas = map[string]PlanQuota{
	"free": {Apps: 3, AppMemoryMB: 256, DeploysPerDay: 20, ConcurrentTunnels: 2},
	"pro":  {Apps: 25, AppMemoryMB: 512, DeploysPerDay: 200, ConcurrentTunnels: 10},
	// Team isn't sold yet (sharing/roles aren't built); the row exists so a
	// team subscription never lands on free limits.
	"team": {Apps: 100, AppMemoryMB: 1024, DeploysPerDay: 500, ConcurrentTunnels: 25},
}

// quotaFor returns the quota for a plan name; unknown plans get free limits.
func quotaFor(plan string) PlanQuota {
	if q, ok := planQuotas[plan]; ok {
		return q
	}
	return planQuotas["free"]
}

// resolveUserPlan reads the user's current plan from the database, falling back to
// the JWT claim. The DB wins because the Paddle webhook updates it the moment
// a subscription changes, while the claim is frozen at login — without this an
// upgraded user would keep free limits until they logged in again.
func resolveUserPlan(ctx context.Context, database *db.DB, userID, claimPlan string) string {
	if database != nil && userID != "" {
		if u, err := database.GetUserByID(ctx, userID); err == nil && u.Plan != "" {
			return u.Plan
		}
	}
	if claimPlan == "" {
		return "free"
	}
	return claimPlan
}

// upgradeHint is appended to every quota error so the fix is one step away.
func upgradeHint(plan string) string {
	if plan == "free" {
		return " Upgrade to Pro for more: https://app.tunr.sh/dashboard/settings/billing"
	}
	return ""
}

func appQuotaMsg(plan string, q PlanQuota) string {
	return fmt.Sprintf("%s plan allows %d apps. Delete one with `tunr apps delete <name>`.%s",
		planTitle(plan), q.Apps, upgradeHint(plan))
}

func deployQuotaMsg(plan string, q PlanQuota) string {
	return fmt.Sprintf("%s plan allows %d deploys per 24 hours. Try again later.%s",
		planTitle(plan), q.DeploysPerDay, upgradeHint(plan))
}

func tunnelQuotaMsg(plan string, q PlanQuota) string {
	return fmt.Sprintf("%s plan allows %d open tunnels at once. Close one first.%s",
		planTitle(plan), q.ConcurrentTunnels, upgradeHint(plan))
}

func planTitle(plan string) string {
	switch plan {
	case "pro":
		return "Pro"
	case "team":
		return "Team"
	default:
		return "Free"
	}
}
