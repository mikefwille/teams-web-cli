package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/config"
	"github.com/mikefwille/teams-web-cli/internal/platform"
)

func TestTeamsPlatformBudgetsPermitObservedFamiliesAndRejectUnknown(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	c := client.New(&config.Config{BaseURL: "https://teams.cloud.microsoft", AuthHeaderVal: "Bearer synthetic"}, time.Second, 0)
	c.HTTPClient.Transport = &teamsTestTransport{}
	c.NoCache = true
	session := &platform.Session{Source: "teams-web", ProfileName: "synthetic", CredentialFingerprint: "synthetic", GateOutcome: platform.GateVerified, ObservedIdentity: map[string]string{"account_id": "synthetic"}, Paths: platform.Paths{CacheDir: t.TempDir(), StateDir: t.TempDir()}}
	if err := c.BindPlatformSession(session); err != nil {
		t.Fatal(err)
	}
	if err := configureTeamsPlatformBudgets(c, session); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/csa/amer/api/v3/teams/users/me", "/api/chatsvc/amer/v1/threads/example/members"} {
		if _, err := c.GetNoCache(context.Background(), path, nil); err != nil {
			t.Fatalf("observed family %s rejected: %v", path, err)
		}
	}
	_, err := c.GetNoCache(context.Background(), "/unobserved/path", nil)
	var missing *platform.MissingEndpointBudgetError
	if !errors.As(err, &missing) {
		t.Fatalf("unobserved family did not fail closed: %v", err)
	}
}
