// Conservative request budgets for the observed Teams service families.
package cli

import (
	"time"

	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/platform"
)

// These are local pacing limits, not claims about Microsoft's server quotas.
// Each family and method receives a separate tenant-scoped limiter.
func configureTeamsPlatformBudgets(c *client.Client, session *platform.Session) error {
	if session == nil {
		return nil
	}
	for _, class := range []string{"GET /api/csa", "GET /api/chatsvc", "GET /api/mt", "POST /api/chatsvc", "PUT /api/chatsvc", "DELETE /api/chatsvc", "POST /ups/noam"} {
		budget := platform.EndpointBudget{Class: class, Steady: 1, Interval: time.Second, Burst: 1, MaxAttempts: 2, RetryBudget: 10 * time.Second}
		if err := c.SetPlatformEndpointBudget(budget); err != nil {
			return err
		}
	}
	return nil
}
