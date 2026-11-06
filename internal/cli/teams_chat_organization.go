// Favorites changes based on the observed current-user section requests.
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
	"strconv"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
)

const teamsConversationFoldersPath = "/api/csa/amer/api/v1/teams/users/me/conversationfolders"

var teamsFavoriteLimiter = cliutil.NewAdaptiveLimiterAuto(1)

type teamsFolderItem struct {
	ConversationID string `json:"conversationId"`
	ThreadType     string `json:"threadType"`
}

type teamsFolder struct {
	ID        string             `json:"id"`
	Type      string             `json:"folderType"`
	IsDeleted *bool              `json:"isDeleted"`
	Items     *[]teamsFolderItem `json:"conversationFolderItems"`
}

type teamsFolderSnapshot struct {
	Folders          []teamsFolder `json:"conversationFolders"`
	HierarchyVersion *int64        `json:"folderHierarchyVersion"`
	IsPartial        *bool         `json:"isPartialResponse"`
}

type teamsFavoriteState struct {
	FolderID string
	Version  int64
	Present  bool
}

// A partial or ambiguous folder hierarchy cannot safely supply a version or
// determine whether a move would affect another section.
func teamsReadFavoriteState(data json.RawMessage, conversation string) (teamsFavoriteState, error) {
	var snapshot teamsFolderSnapshot
	if len(data) == 0 || json.Unmarshal(data, &snapshot) != nil || snapshot.HierarchyVersion == nil || *snapshot.HierarchyVersion < 0 || snapshot.IsPartial == nil || *snapshot.IsPartial || snapshot.Folders == nil {
		return teamsFavoriteState{}, errors.New("Teams did not provide a complete versioned section snapshot; no Favorites change is safe")
	}
	var favorite *teamsFolder
	seen := map[string]bool{}
	elsewhere := false
	for i := range snapshot.Folders {
		f := &snapshot.Folders[i]
		if !validTeamsOrganizationID(f.ID) || seen[f.ID] || f.IsDeleted == nil {
			return teamsFavoriteState{}, errors.New("Teams returned an ambiguous section snapshot")
		}
		seen[f.ID] = true
		if *f.IsDeleted {
			continue
		}
		if f.Items == nil || f.Type == "" {
			return teamsFavoriteState{}, errors.New("Teams returned an incomplete section in the snapshot")
		}
		if f.Type == "Favorites" {
			if favorite != nil {
				return teamsFavoriteState{}, errors.New("Teams returned multiple Favorites sections")
			}
			favorite = f
		} else {
			for _, item := range *f.Items {
				if item.ConversationID == conversation {
					elsewhere = true
				}
			}
		}
	}
	if favorite == nil {
		return teamsFavoriteState{}, errors.New("Teams did not return an identifiable Favorites section")
	}
	if elsewhere {
		return teamsFavoriteState{}, errors.New("conversation is also in another section; its move semantics have not been established")
	}
	count := 0
	for _, item := range *favorite.Items {
		if item.ConversationID == conversation {
			count++
		}
	}
	if count > 1 {
		return teamsFavoriteState{}, errors.New("Teams returned duplicate Favorites membership")
	}
	return teamsFavoriteState{FolderID: favorite.ID, Version: *snapshot.HierarchyVersion, Present: count == 1}, nil
}

type teamsFavoriteAction struct {
	Action   string `json:"action"`
	FolderID string `json:"folderId"`
	ItemID   string `json:"itemId"`
}

type teamsFavoritePayload struct {
	Actions                         []teamsFavoriteAction `json:"actions"`
	FolderHierarchyVersion          int64                 `json:"folderHierarchyVersion"`
	SupportsAdditionalSystemFolders bool                  `json:"supportsAdditionalSystemGeneratedFolders"`
	SupportsSliceItems              bool                  `json:"supportsSliceItems"`
}

func teamsFavoriteBody(state teamsFavoriteState, conversation string, favorite bool) teamsFavoritePayload {
	action := "RemoveItem"
	if favorite {
		action = "AddItem"
	}
	return teamsFavoritePayload{Actions: []teamsFavoriteAction{{Action: action, FolderID: state.FolderID, ItemID: conversation}}, FolderHierarchyVersion: state.Version, SupportsAdditionalSystemFolders: true, SupportsSliceItems: true}
}

// Exactly one request is sent. Transport failures, server errors, and invalid
// success responses are ambiguous and are never retried automatically.
func performTeamsFavoriteWrite(ctx context.Context, c *client.Client, state teamsFavoriteState, conversation string, favorite bool) error {
	if c == nil || c.HTTPClient == nil || c.Config == nil || c.Config.AuthSource != "web-session" || !teamsOrigin(c.BaseURL) {
		return authErr(errors.New("a verified-origin Teams web session is required"))
	}
	if _, ok := c.HTTPClient.Transport.(teamsOriginTransport); !ok {
		return errors.New("Teams session origin guard is unavailable")
	}
	if !validTeamsOrganizationID(state.FolderID) || !validTeamsOrganizationID(conversation) || state.Version < 0 {
		return errors.New("invalid Favorites write target")
	}
	body, err := json.Marshal(teamsFavoriteBody(state, conversation, favorite))
	if err != nil {
		return err
	}
	u, err := url.Parse(c.BaseURL + teamsConversationFoldersPath)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("supportsAdditionalSystemGeneratedFolders", "true")
	q.Set("supportsSliceItems", "true")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://teams.cloud.microsoft")
	if err := teamsFavoriteLimiter.Wait(ctx); err != nil {
		return err
	}
	hc := *c.HTTPClient
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return apiErr(errors.New("outcome_unknown: Favorites request did not complete; independently inspect the section before trying again"))
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 2_000_001))
	if readErr != nil || len(data) > 2_000_000 {
		return apiErr(errors.New("outcome_unknown: Favorites response could not be read; independently inspect the section before trying again"))
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		teamsFavoriteLimiter.OnRateLimit()
		return rateLimitErr(&cliutil.RateLimitError{URL: "Teams section service", RetryAfter: cliutil.RetryAfter(resp)})
	}
	if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500 || resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return apiErr(fmt.Errorf("outcome_unknown: Teams returned HTTP %d; independently inspect the section before trying again", resp.StatusCode))
	}
	if resp.StatusCode != http.StatusOK {
		return classifyAPIErrorOnly(fmt.Errorf("Teams section request returned HTTP %d", resp.StatusCode))
	}
	updated, err := teamsReadFavoriteState(data, conversation)
	if err != nil || updated.FolderID != state.FolderID || updated.Present != favorite {
		return apiErr(errors.New("outcome_unknown: Teams returned an unexpected section response; independently inspect the section before trying again"))
	}
	teamsFavoriteLimiter.OnSuccess()
	return nil
}

// pp:data-source live
func newTeamsFavoriteStatusCmd(flags *rootFlags) *cobra.Command {
	var conversation string
	cmd := &cobra.Command{Use: "favorite-status", Short: "Read one conversation's stored Favorites membership", Long: "Read the stored Favorites membership from a versioned bootstrap snapshot. Teams can render a conversation in a different section; this read does not determine its visible placement.", Args: cobra.NoArgs, Annotations: map[string]string{"mcp:read-only": "true", "pp:data-source": "live"}}
	cmd.Flags().StringVar(&conversation, "conversation", "", "Opaque conversation ID")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if !validTeamsOrganizationID(conversation) {
			return usageErr(errors.New("--conversation must be a valid opaque conversation ID"))
		}
		if dryRunOK(flags) {
			return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "favorite-status"})
		}
		body, err := fetchTeamsBootstrap(cmd, flags)
		if err != nil {
			return err
		}
		state, err := teamsReadFavoriteState(body.ConversationFolders, conversation)
		if err != nil {
			return err
		}
		return flags.printJSON(cmd, teamsFavoriteMembershipResult(state, conversation))
	}
	return cmd
}

// A stored folder item can persist while Teams renders the conversation in a
// different section. Do not report that item as proof of visible placement.
func teamsFavoriteMembershipResult(state teamsFavoriteState, conversation string) map[string]any {
	return map[string]any{"source": "live_bootstrap", "conversation": conversation, "favorite_membership_record": state.Present, "rendered_section": "not_determined", "folder_hierarchy_version": state.Version}
}

func configureTeamsFavoriteChange(cmd *cobra.Command, flags *rootFlags, favorite bool) {
	var conversation, ifVersion string
	var execute, confirm bool
	name, description := "unfavorite", "Remove a conversation's stored Favorites membership"
	if favorite {
		name, description = "favorite", "Add a conversation's stored Favorites membership"
	}
	cmd.Short = description
	cmd.Args = cobra.NoArgs
	cmd.Annotations = map[string]string{"mcp:read-only-unless-flags": "execute,confirm"}
	cmd.Flags().StringVar(&conversation, "conversation", "", "Opaque conversation ID")
	cmd.Flags().StringVar(&ifVersion, "if-version", "", "Require this folder hierarchy version at execution")
	cmd.Flags().BoolVar(&execute, "execute", false, "Execute the previewed change")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm the change")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if !validTeamsOrganizationID(conversation) {
			return usageErr(errors.New("--conversation must be a valid opaque conversation ID"))
		}
		var expectedVersion int64
		if ifVersion != "" {
			var err error
			expectedVersion, err = strconv.ParseInt(ifVersion, 10, 64)
			if err != nil || expectedVersion < 0 {
				return usageErr(errors.New("--if-version must be a nonnegative integer"))
			}
		}
		preview := map[string]any{"executed": false, "action": name, "conversation": conversation, "target_favorite_membership_record": favorite, "current_membership_record": "not_checked", "rendered_section": "not_determined"}
		if ifVersion != "" {
			preview["if_version"] = expectedVersion
		}
		if flags.dryRun || !execute {
			return flags.printJSON(cmd, preview)
		}
		if !confirm {
			return usageErr(errors.New("--execute requires --confirm"))
		}
		if cliutil.IsAnyHarness() {
			return errors.New("Favorites writes are disabled in verification and dogfood harnesses")
		}
		c, err := flags.newClient()
		if err != nil {
			return err
		}
		body, err := fetchTeamsBootstrapWithClient(cmd, flags, c)
		if err != nil {
			return err
		}
		state, err := teamsReadFavoriteState(body.ConversationFolders, conversation)
		if err != nil {
			return err
		}
		if ifVersion != "" && expectedVersion != state.Version {
			return errors.New("section hierarchy changed since the supplied version; inspect and preview again")
		}
		if state.Present == favorite {
			return flags.printJSON(cmd, map[string]any{"executed": false, "action": name, "membership_record_already_in_state": true, "favorite_membership_record": favorite, "rendered_section": "not_determined"})
		}
		ctx, cancel := boundCtx(cmd.Context(), flags)
		defer cancel()
		if err := performTeamsFavoriteWrite(ctx, c, state, conversation, favorite); err != nil {
			return err
		}
		return flags.printJSON(cmd, map[string]any{"executed": true, "action": name, "status": "accepted", "verification": "membership_response_only", "rendered_section": "not_determined"})
	}
}

func newTeamsFavoriteCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "favorite"}
	configureTeamsFavoriteChange(cmd, flags, true)
	return cmd
}

func newTeamsUnfavoriteCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "unfavorite"}
	configureTeamsFavoriteChange(cmd, flags, false)
	return cmd
}
