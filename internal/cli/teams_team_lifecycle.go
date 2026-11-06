// Bounded private-team lifecycle based on captured Teams web requests.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
)

const teamsLifecycleBase = "/api/mt/part/amer-02/beta/teams"

var teamsLifecycleLimiter = cliutil.NewAdaptiveLimiterAuto(1)
var teamsOwnedTeamID = regexp.MustCompile(`(?i)^19:[a-z0-9._-]{1,180}@thread\.tacv2$`)

func validTeamsLifecycleName(s string) bool {
	if s == "" || strings.TrimSpace(s) != s || !utf8.ValidString(s) || len(s) > 200 {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

type teamsPrivateCreateBody struct {
	DisplayName        string `json:"displayName"`
	DisplayNamePrefix  string `json:"displayNamePrefix"`
	DisplayNameSuffix  string `json:"displayNameSuffix"`
	Description        string `json:"description"`
	AccessType         int    `json:"accessType"`
	IsTenantWide       bool   `json:"isTenantWide"`
	ValidationRequired bool   `json:"validationRequired"`
	SensitivityLabelID any    `json:"sensitivityLabelId"`
	TopicThreadTopic   string `json:"topicThreadTopic"`
}

func teamsPrivateCreatePayload(name, initialChannel string) teamsPrivateCreateBody {
	return teamsPrivateCreateBody{DisplayName: name, AccessType: 1, TopicThreadTopic: initialChannel}
}

type teamsCreatedTeam struct {
	Value struct {
		ObjectID      string `json:"objectId"`
		SkypeThreadID string `json:"skypeThreadId"`
		DisplayName   string `json:"displayName"`
		AccessType    string `json:"accessType"`
	} `json:"value"`
}

// The provider receives exactly one write. Never retry an ambiguous outcome.
func performTeamsLifecycleWrite(ctx context.Context, c *client.Client, action, teamID, name, initialChannel string) (string, error) {
	if c == nil || c.HTTPClient == nil || c.Config == nil || c.Config.AuthSource != "web-session" || !teamsOrigin(c.BaseURL) {
		return "", authErr(errors.New("a verified-origin Teams web session is required"))
	}
	guard, ok := c.HTTPClient.Transport.(teamsOriginTransport)
	if !ok || guard.scopes == nil || len(guard.scopes["mt"]) == 0 {
		return "", errors.New("an observed Teams management audience is required")
	}
	method, route := http.MethodPost, teamsLifecycleBase+"/create"
	var reader io.Reader
	if action == "create" {
		if !validTeamsLifecycleName(name) || !validTeamsLifecycleName(initialChannel) {
			return "", errors.New("invalid private team or initial channel name")
		}
		body, err := json.Marshal(teamsPrivateCreatePayload(name, initialChannel))
		if err != nil {
			return "", err
		}
		reader = bytes.NewReader(body)
	} else if action == "delete" {
		if !teamsOwnedTeamID.MatchString(teamID) {
			return "", errors.New("invalid Teams team ID")
		}
		method, route = http.MethodDelete, teamsLifecycleBase+"/"+url.PathEscape(teamID)+"/delete"
	} else {
		return "", errors.New("unsupported team lifecycle action")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+route, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://teams.cloud.microsoft")
	if err := teamsLifecycleLimiter.Wait(ctx); err != nil {
		return "", err
	}
	hc := *c.HTTPClient
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return "", apiErr(errors.New("outcome_unknown: team request did not complete; independently inspect Teams before another write"))
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1_000_001))
	if readErr != nil || len(data) > 1_000_000 {
		return "", apiErr(errors.New("outcome_unknown: team response could not be read; independently inspect Teams before another write"))
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		teamsLifecycleLimiter.OnRateLimit()
		return "", rateLimitErr(&cliutil.RateLimitError{URL: "Teams management service", RetryAfter: cliutil.RetryAfter(resp)})
	}
	if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500 || resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return "", apiErr(fmt.Errorf("outcome_unknown: Teams returned HTTP %d; independently inspect Teams before another write", resp.StatusCode))
	}
	if action == "create" {
		if resp.StatusCode != http.StatusCreated {
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return "", apiErr(errors.New("outcome_unknown: Teams returned an unexpected create status; inspect Teams before another write"))
			}
			return "", classifyAPIErrorOnly(fmt.Errorf("Teams create request returned HTTP %d", resp.StatusCode))
		}
		var created teamsCreatedTeam
		if json.Unmarshal(data, &created) != nil || !teamsOwnedTeamID.MatchString(created.Value.ObjectID) || created.Value.ObjectID != created.Value.SkypeThreadID || created.Value.DisplayName != name || !strings.EqualFold(created.Value.AccessType, "private") {
			return "", apiErr(errors.New("outcome_unknown: Teams returned an unexpected create response; inspect Teams before another write"))
		}
		teamsLifecycleLimiter.OnSuccess()
		return created.Value.ObjectID, nil
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return "", apiErr(errors.New("outcome_unknown: Teams returned an unexpected delete status; inspect Teams before another write"))
		}
		return "", classifyAPIErrorOnly(fmt.Errorf("Teams delete request returned HTTP %d", resp.StatusCode))
	}
	var deleted bool
	if json.Unmarshal(data, &deleted) != nil || !deleted {
		return "", apiErr(errors.New("outcome_unknown: Teams returned an unexpected delete response; inspect Teams before another write"))
	}
	teamsLifecycleLimiter.OnSuccess()
	return teamID, nil
}

// Delete is limited to the observed self-created, single-human team shape.
// Service bots are permitted, but another human or an unknown identity fails
// closed; a partial roster cannot establish that it is safe.
func validateTeamsOwnedTeam(threadData, memberData []byte, teamID, expectedName, ownObject string) error {
	var thread struct {
		ID               string `json:"id"`
		ThreadProperties struct {
			IsCreator *bool  `json:"isCreator"`
			Name      string `json:"spaceThreadTopic"`
			Type      string `json:"threadType"`
		} `json:"threadProperties"`
		MemberProperties struct {
			Role string `json:"role"`
		} `json:"memberProperties"`
	}
	if json.Unmarshal(threadData, &thread) != nil || thread.ID != teamID || thread.ThreadProperties.IsCreator == nil || !*thread.ThreadProperties.IsCreator || thread.ThreadProperties.Name != expectedName || thread.ThreadProperties.Type != "space" || !strings.EqualFold(thread.MemberProperties.Role, "admin") {
		return errors.New("team identity, name, or current owner role could not be verified")
	}
	var roster struct {
		Members []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"members"`
		TotalMemberCount *int            `json:"totalMemberCount"`
		IsDeleted        *bool           `json:"isDeleted"`
		Continuation     json.RawMessage `json:"continuationToken"`
	}
	if json.Unmarshal(memberData, &roster) != nil || roster.Members == nil || roster.TotalMemberCount == nil || *roster.TotalMemberCount != len(roster.Members) || roster.IsDeleted == nil || *roster.IsDeleted || len(roster.Members) > 100 || len(roster.Continuation) > 0 && string(roster.Continuation) != "null" && string(roster.Continuation) != `""` {
		return errors.New("a complete current team roster could not be verified")
	}
	if ownObject == "" {
		return errors.New("selected Teams identity is unavailable")
	}
	ownMRI := "8:orgid:" + ownObject
	ownCount := 0
	for _, member := range roster.Members {
		if strings.EqualFold(member.ID, ownMRI) {
			if !strings.EqualFold(member.Role, "admin") {
				return errors.New("selected account is not a team owner")
			}
			ownCount++
		} else if !strings.HasPrefix(strings.ToLower(member.ID), "28:") || len(member.ID) <= 3 {
			return errors.New("team has another human or an unrecognized member")
		}
	}
	if ownCount != 1 {
		return errors.New("selected account does not appear exactly once as owner")
	}
	return nil
}

func teamsOwnedTeamPreflight(cmd *cobra.Command, flags *rootFlags, c *client.Client, teamID, expectedName string) error {
	principal, ok := teamsPrincipal(c.Config.AuthHeader())
	if !ok {
		return authErr(errors.New("Teams account identity is unavailable"))
	}
	ctx, cancel := boundCtx(cmd.Context(), flags)
	defer cancel()
	thread, err := c.GetNoCache(ctx, "/api/chatsvc/amer/v1/users/ME/conversations/"+url.PathEscape(teamID), map[string]string{"view": "msnp24Equivalent"})
	if err != nil {
		return errors.New("could not read the selected team before deletion")
	}
	members, err := c.GetNoCache(ctx, "/api/chatsvc/amer/v1/threads/"+url.PathEscape(teamID)+"/members", map[string]string{"view": "msnp24Equivalent", "selectMemberRoles": "User|Admin|Guest", "pageSize": "100"})
	if err != nil {
		return errors.New("could not read the selected team's members before deletion")
	}
	return validateTeamsOwnedTeam(thread, members, teamID, expectedName, principal.Object)
}

func newTeamsPrivateCreateCmd(flags *rootFlags) *cobra.Command {
	var name, initialChannel string
	var execute, confirm bool
	cmd := &cobra.Command{Use: "create", Short: "Preview or create a private team with an initial channel", Args: cobra.NoArgs, Annotations: map[string]string{"mcp:read-only-unless-flags": "execute,confirm"}}
	cmd.Flags().StringVar(&name, "name", "", "Private team display name")
	cmd.Flags().StringVar(&initialChannel, "initial-channel", "", "Initial channel name")
	cmd.Flags().BoolVar(&execute, "execute", false, "Execute the previewed creation")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm the creation")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if !validTeamsLifecycleName(name) || !validTeamsLifecycleName(initialChannel) {
			return usageErr(errors.New("--name and --initial-channel must be nonempty names without control characters"))
		}
		if flags.dryRun || !execute {
			return flags.printJSON(cmd, map[string]any{"executed": false, "action": "create private team", "name": name, "initial_channel": initialChannel, "invitations_in_request": false})
		}
		if !confirm {
			return usageErr(errors.New("--execute requires --confirm"))
		}
		if cliutil.IsAnyHarness() {
			return errors.New("team writes are disabled in verification and dogfood harnesses")
		}
		c, err := flags.newClient()
		if err != nil {
			return err
		}
		ctx, cancel := boundCtx(cmd.Context(), flags)
		defer cancel()
		id, err := performTeamsLifecycleWrite(ctx, c, "create", "", name, initialChannel)
		if err != nil {
			return err
		}
		return flags.printJSON(cmd, map[string]any{"executed": true, "action": "create private team", "status": "accepted", "team_id": id, "verification": "not_checked"})
	}
	return cmd
}

func newTeamsOwnedDeleteCmd(flags *rootFlags) *cobra.Command {
	var expectedName string
	var execute, confirm bool
	cmd := &cobra.Command{Use: "delete [team-id]", Short: "Preview or delete a self-created single-human team", Annotations: map[string]string{"mcp:read-only-unless-flags": "execute,confirm"}}
	cmd.Flags().StringVar(&expectedName, "if-name", "", "Require the exact current team name")
	cmd.Flags().BoolVar(&execute, "execute", false, "Execute the previewed deletion")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm the deletion")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 || !teamsOwnedTeamID.MatchString(args[0]) || !validTeamsLifecycleName(expectedName) {
			return usageErr(errors.New("a valid team ID and exact --if-name are required"))
		}
		if flags.dryRun || !execute {
			return flags.printJSON(cmd, map[string]any{"executed": false, "action": "delete self-created single-human team", "team_id": args[0], "if_name": expectedName, "preflight": "not_checked"})
		}
		if !confirm {
			return usageErr(errors.New("--execute requires --confirm"))
		}
		if cliutil.IsAnyHarness() {
			return errors.New("team writes are disabled in verification and dogfood harnesses")
		}
		c, err := flags.newClient()
		if err != nil {
			return err
		}
		if c == nil || c.HTTPClient == nil || c.Config == nil || c.Config.AuthSource != "web-session" || !teamsOrigin(c.BaseURL) {
			return authErr(errors.New("a verified-origin Teams web session is required"))
		}
		guard, ok := c.HTTPClient.Transport.(teamsOriginTransport)
		if !ok || guard.scopes == nil || len(guard.scopes["mt"]) == 0 {
			return errors.New("an observed Teams management audience is required")
		}
		if err := teamsOwnedTeamPreflight(cmd, flags, c, args[0], expectedName); err != nil {
			return err
		}
		ctx, cancel := boundCtx(cmd.Context(), flags)
		defer cancel()
		_, err = performTeamsLifecycleWrite(ctx, c, "delete", args[0], "", "")
		if err != nil {
			return err
		}
		return flags.printJSON(cmd, map[string]any{"executed": true, "action": "delete team", "status": "accepted", "team_id": args[0], "verification": "not_checked"})
	}
	return cmd
}

func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		for _, group := range root.Commands() {
			if group.Name() == "teams" {
				addNovelCommandIfAbsent(group, newTeamsPrivateCreateCmd(flags))
				addNovelCommandIfAbsent(group, newTeamsOwnedDeleteCmd(flags))
				return
			}
		}
	})
}
