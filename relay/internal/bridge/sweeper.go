package bridge

import (
	"context"
	"time"

	"github.com/tunr-dev/tunr/relay/internal/audit"
	"github.com/tunr-dev/tunr/relay/internal/logger"
)

// sweeper.go — grant expiry (P4-T2). Its own goroutine, never the app idle
// sweeper's (CLAUDE.md: one slow call there wedges scale-to-zero box-wide).
// Every DB call carries its own deadline.
//
// Streams already close at expires_at on their own (access.go); the sweeper
// makes the grant row say so, kills anything left on the connector side,
// audits it and notifies.

var sweepInterval = 5 * time.Second

// StartGrantSweeper runs until ctx ends.
func (s *Server) StartGrantSweeper(ctx context.Context) {
	if !s.Enabled() {
		return
	}
	go func() {
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sweepOnce(ctx)
			}
		}
	}()
}

func (s *Server) sweepOnce(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, dbTimeout)
	expired, err := s.db.ExpireDueGrants(cctx)
	cancel()
	if err != nil {
		logger.Warn("bridge sweeper: %v", err)
		return
	}
	for i := range expired {
		g := &expired[i]
		s.cutGrant(g.ID, g.SiteID, "expired")
		detail := map[string]any{"grant_id": g.ID, "agent_id": g.AgentID, "resource": g.Address}
		actx, acancel := context.WithTimeout(ctx, dbTimeout)
		for _, org := range []string{g.CustomerOrgID, g.VendorOrgID} {
			if err := s.db.AuditOrg(actx, org, "", audit.ActionBridgeGrantExpired, detail); err != nil {
				logger.Warn("bridge audit: %v", err)
			}
		}
		acancel()
		s.notify.Grant("grant.expired", g)
	}
}

// cutGrant closes every live stream of a grant on both legs: the relay's
// agent sockets and, via bridge_kill, the connector's target connections.
func (s *Server) cutGrant(grantID, siteID, reason string) {
	s.access.KillGrant(grantID, reason)
	_ = s.hub.Send(siteID, msg(MsgKill, KillData{GrantID: grantID}))
}
