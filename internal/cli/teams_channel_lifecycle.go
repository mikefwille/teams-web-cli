// Bounded self-owned standard-channel lifecycle based on Teams web captures.
package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
)

type teamsChannelThread struct {
	ID               string `json:"id"`
	ThreadProperties struct {
		Topic             string `json:"topic"`
		SpaceID           string `json:"spaceId"`
		GroupID           string `json:"groupId"`
		Privacy           string `json:"privacy"`
		ThreadType        string `json:"threadType"`
		ProductThreadType string `json:"productThreadType"`
	} `json:"threadProperties"`
}

type teamsChannelCreateBody struct {
	DisplayName      string `json:"displayName"`
	Description      string `json:"description"`
	GroupID          string `json:"groupId"`
	ChannelType      string `json:"channelType"`
	ChatModalityType string `json:"chatModalityType"`
}
type teamsChannelDeleteBody struct {
	ID          string `json:"id"`
	ChannelType string `json:"channelType"`
	DisplayName string `json:"displayName"`
	IsGeneral   bool   `json:"isGeneral"`
}
type teamsChannelReceipt struct {
	Version   int    `json:"version"`
	State     string `json:"state"`
	TeamID    string `json:"team_id"`
	ChannelID string `json:"channel_id"`
	Name      string `json:"name"`
	GroupID   string `json:"group_id"`
}

func teamsChannelName(name string) bool {
	return validTeamsLifecycleName(name) && !strings.EqualFold(name, "general")
}
func teamsChannelCreatePayload(name, group string) teamsChannelCreateBody {
	return teamsChannelCreateBody{DisplayName: name, GroupID: group, ChannelType: "Standard", ChatModalityType: "Conversational"}
}
func teamsChannelDeletePayload(id, name string) teamsChannelDeleteBody {
	return teamsChannelDeleteBody{ID: id, ChannelType: "Standard", DisplayName: name, IsGeneral: false}
}

// Fetch a current standard-channel conversation and verify its parent team.
func readTeamsChannelThread(cmd *cobra.Command, flags *rootFlags, c *client.Client, teamID, channelID, expectedName, expectedGroup string) (string, error) {
	ctx, cancel := boundCtx(cmd.Context(), flags)
	defer cancel()
	data, err := c.GetNoCache(ctx, "/api/chatsvc/amer/v1/users/ME/conversations/"+url.PathEscape(channelID), map[string]string{"view": "msnp24Equivalent"})
	if err != nil {
		return "", errors.New("could not read the selected standard channel")
	}
	return validateTeamsChannelThread(data, teamID, channelID, expectedName, expectedGroup)
}
func validateTeamsChannelThread(data []byte, teamID, channelID, expectedName, expectedGroup string) (string, error) {
	var thread teamsChannelThread
	if json.Unmarshal(data, &thread) != nil || thread.ID != channelID || thread.ThreadProperties.SpaceID != teamID || thread.ThreadProperties.ThreadType != "topic" || thread.ThreadProperties.ProductThreadType != "TeamsStandardChannel" || thread.ThreadProperties.Privacy != "public" || !teamsPeopleUUID.MatchString(thread.ThreadProperties.GroupID) || (expectedName != "" && thread.ThreadProperties.Topic != expectedName) || (expectedGroup != "" && thread.ThreadProperties.GroupID != expectedGroup) {
		return "", errors.New("standard channel identity, name, group, or parent team could not be verified")
	}
	return thread.ThreadProperties.GroupID, nil
}

// In the observed account General uses the team space's own thread ID. Require
// the current team conversation and bootstrap's explicitly marked General row
// to agree on the team and group before using that ID as a create anchor.
func readTeamsGeneralAnchor(cmd *cobra.Command, flags *rootFlags, c *client.Client, teamID, expectedName string) (string, error) {
	ctx, cancel := boundCtx(cmd.Context(), flags)
	defer cancel()
	data, err := c.GetNoCache(ctx, "/api/chatsvc/amer/v1/users/ME/conversations/"+url.PathEscape(teamID), map[string]string{"view": "msnp24Equivalent"})
	if err != nil {
		return "", errors.New("could not read the team space for General")
	}
	bootstrap, err := fetchTeamsBootstrapWithClient(cmd, flags, c)
	if err != nil {
		return "", errors.New("could not read the General channel bootstrap")
	}
	return validateTeamsGeneralAnchor(data, bootstrap, teamID, expectedName)
}
func validateTeamsGeneralAnchor(data []byte, bootstrap teamsBootstrap, teamID, expectedName string) (string, error) {
	var thread struct {
		ID               string `json:"id"`
		ThreadProperties struct {
			Type      string `json:"threadType"`
			Name      string `json:"spaceThreadTopic"`
			GroupID   string `json:"groupId"`
			IsCreator *bool  `json:"isCreator"`
		} `json:"threadProperties"`
		MemberProperties struct {
			Role string `json:"role"`
		} `json:"memberProperties"`
	}
	if json.Unmarshal(data, &thread) != nil || thread.ID != teamID || thread.ThreadProperties.Type != "space" || thread.ThreadProperties.Name != expectedName || thread.ThreadProperties.IsCreator == nil || !*thread.ThreadProperties.IsCreator || !strings.EqualFold(thread.MemberProperties.Role, "admin") || !teamsPeopleUUID.MatchString(thread.ThreadProperties.GroupID) {
		return "", errors.New("General's team identity, name, creator, role, or group could not be verified")
	}
	group := thread.ThreadProperties.GroupID
	matches := 0
	for _, row := range collectTeamsChannels(bootstrap) {
		parent, _ := row["parentTeamId"].(string)
		rowGroup, _ := row["groupId"].(string)
		general, _ := row["isGeneral"].(bool)
		deleted, _ := row["isDeleted"].(bool)
		if teamsObjectID(row) == teamID && parent == teamID && general && !deleted && rowGroup == group {
			matches++
		}
	}
	if matches != 1 {
		return "", errors.New("a matching current General bootstrap row could not be verified")
	}
	return group, nil
}

func teamsChannelReceiptDir(principal teamsTokenPrincipal, create bool) (string, error) {
	if principal.Tenant == "" || principal.Object == "" {
		return "", errors.New("Teams account identity is unavailable")
	}
	base, err := cliutil.DataDir()
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(principal.Tenant + "\x00" + principal.Object))
	dir := filepath.Join(base, "teams-channel-receipts", hex.EncodeToString(h[:]))
	if create {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", err
		}
	}
	for _, p := range []string{filepath.Dir(dir), dir} {
		st, err := os.Lstat(p)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0 {
			return "", errors.New("channel receipt directory is not private")
		}
	}
	return dir, nil
}
func teamsChannelReceiptPath(dir, teamID, channelID string) string {
	h := sha256.Sum256([]byte(teamID + "\x00" + channelID))
	return filepath.Join(dir, hex.EncodeToString(h[:])+".json")
}
func writeTeamsChannelReceipt(dir string, receipt teamsChannelReceipt) error {
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(teamsChannelReceiptPath(dir, receipt.TeamID, receipt.ChannelID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
func readTeamsChannelReceipt(dir, teamID, channelID, name string) (teamsChannelReceipt, error) {
	var receipt teamsChannelReceipt
	if readPrivateSessionFile(teamsChannelReceiptPath(dir, teamID, channelID), &receipt) != nil || receipt.Version != 1 || receipt.State != "active" || receipt.TeamID != teamID || receipt.ChannelID != channelID || receipt.Name != name || !teamsPeopleUUID.MatchString(receipt.GroupID) {
		return teamsChannelReceipt{}, errors.New("no matching private receipt for a channel created by this account")
	}
	return receipt, nil
}

// Advance the local receipt before a delete so an ambiguous write cannot be
// sent again by this command without independent reconciliation.
func markTeamsChannelReceipt(dir string, receipt teamsChannelReceipt, state string) error {
	if state != "delete_in_progress" && state != "deleted" {
		return errors.New("invalid channel receipt state")
	}
	receipt.State = state
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	return cliutil.AtomicWritePrivateFile(teamsChannelReceiptPath(dir, receipt.TeamID, receipt.ChannelID), append(data, '\n'), 0600, 0700)
}

// Hold an account/channel-scoped cross-process claim from the first receipt
// read through the provider outcome. The persisted in-progress state then
// prevents a later process from retrying an ambiguous delete.
func claimTeamsChannelDelete(dir, teamID, channelID, name string, preflight func(teamsChannelReceipt) error, write func(teamsChannelReceipt) (string, error)) (string, bool, error) {
	var id string
	var receiptUpdated bool
	locked, err := cliutil.TryWithFileLock(teamsChannelReceiptPath(dir, teamID, channelID), func() error {
		receipt, err := readTeamsChannelReceipt(dir, teamID, channelID, name)
		if err != nil {
			return err
		}
		if err = preflight(receipt); err != nil {
			return err
		}
		if err = markTeamsChannelReceipt(dir, receipt, "delete_in_progress"); err != nil {
			return err
		}
		id, err = write(receipt)
		if err != nil {
			return err
		}
		receiptUpdated = markTeamsChannelReceipt(dir, receipt, "deleted") == nil
		return nil
	})
	if err != nil {
		return "", false, err
	}
	if !locked {
		return "", false, errors.New("another delete for this channel is already in progress")
	}
	return id, receiptUpdated, nil
}

// Delete uses a minimal subset of the much larger observed UI model. Its live
// service acceptance must be established separately; never retry uncertainty.
func performTeamsChannelWrite(ctx context.Context, c *client.Client, action, teamID, channelID, name, group string) (string, error) {
	if c == nil || c.HTTPClient == nil || c.Config == nil || c.Config.AuthSource != "web-session" || !teamsOrigin(c.BaseURL) {
		return "", authErr(errors.New("a verified-origin Teams web session is required"))
	}
	guard, ok := c.HTTPClient.Transport.(teamsOriginTransport)
	if !ok || guard.scopes == nil || len(guard.scopes["mt"]) == 0 {
		return "", errors.New("an observed Teams management audience is required")
	}
	if !teamsOwnedTeamID.MatchString(teamID) || !teamsChannelName(name) || !teamsPeopleUUID.MatchString(group) {
		return "", errors.New("invalid team, channel name, or group")
	}
	route := teamsLifecycleBase + "/" + url.PathEscape(teamID) + "/channels"
	method := http.MethodPost
	var body any = teamsChannelCreatePayload(name, group)
	if action == "delete" {
		if !teamsOwnedTeamID.MatchString(channelID) || channelID == teamID {
			return "", errors.New("invalid channel ID")
		}
		route += "/" + url.PathEscape(channelID)
		method = http.MethodDelete
		body = teamsChannelDeletePayload(channelID, name)
	} else if action != "create" {
		return "", errors.New("unsupported channel action")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+route, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://teams.cloud.microsoft")
	if err = teamsLifecycleLimiter.Wait(ctx); err != nil {
		return "", err
	}
	hc := *c.HTTPClient
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return "", apiErr(errors.New("outcome_unknown: channel request did not complete; inspect Teams before another write"))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1_000_001))
	if err != nil || len(raw) > 1_000_000 {
		return "", apiErr(errors.New("outcome_unknown: channel response could not be read; inspect Teams before another write"))
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		teamsLifecycleLimiter.OnRateLimit()
		return "", rateLimitErr(&cliutil.RateLimitError{URL: "Teams management service", RetryAfter: cliutil.RetryAfter(resp)})
	}
	if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500 || (resp.StatusCode >= 300 && resp.StatusCode < 400) {
		return "", apiErr(fmt.Errorf("outcome_unknown: Teams returned HTTP %d; inspect Teams before another write", resp.StatusCode))
	}
	if action == "create" {
		if resp.StatusCode != http.StatusCreated {
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return "", apiErr(errors.New("outcome_unknown: unexpected create status; inspect Teams"))
			}
			return "", classifyAPIErrorOnly(fmt.Errorf("Teams channel create returned HTTP %d", resp.StatusCode))
		}
		var result struct {
			Value struct {
				ObjectID         string `json:"objectId"`
				DisplayName      string `json:"displayName"`
				GroupID          string `json:"groupId"`
				ChannelState     string `json:"channelState"`
				ChatModalityType string `json:"chatModalityType"`
			} `json:"value"`
		}
		v := &result.Value
		if json.Unmarshal(raw, &result) != nil || !teamsOwnedTeamID.MatchString(v.ObjectID) || v.ObjectID == teamID || v.DisplayName != name || v.GroupID != group || v.ChannelState != "Active" || v.ChatModalityType != "Conversational" {
			return "", apiErr(errors.New("outcome_unknown: unexpected channel create response; inspect Teams"))
		}
		teamsLifecycleLimiter.OnSuccess()
		return v.ObjectID, nil
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return "", apiErr(errors.New("outcome_unknown: unexpected delete status; inspect Teams"))
		}
		return "", classifyAPIErrorOnly(fmt.Errorf("Teams channel delete returned HTTP %d", resp.StatusCode))
	}
	var deleted bool
	if json.Unmarshal(raw, &deleted) != nil || !deleted {
		return "", apiErr(errors.New("outcome_unknown: unexpected channel delete response; inspect Teams"))
	}
	teamsLifecycleLimiter.OnSuccess()
	return channelID, nil
}

func newTeamsChannelCreateCmd(flags *rootFlags) *cobra.Command {
	var teamID, teamName, anchor, name string
	var execute, confirm, verifyOnly bool
	cmd := &cobra.Command{Use: "create", Short: "Preview or create a standard channel in a self-owned team", Args: cobra.NoArgs, Annotations: map[string]string{"mcp:read-only-unless-flags": "execute,confirm"}}
	cmd.Flags().StringVar(&teamID, "team", "", "Owning team ID")
	cmd.Flags().StringVar(&teamName, "if-team-name", "", "Require the exact current team name")
	cmd.Flags().StringVar(&anchor, "anchor-channel", "", "Existing standard channel in this team")
	cmd.Flags().StringVar(&name, "name", "", "New channel name")
	cmd.Flags().BoolVar(&execute, "execute", false, "Execute the previewed creation")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm the creation")
	cmd.Flags().BoolVar(&verifyOnly, "verify-only", false, "Read current ownership and anchor without creating")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if !teamsOwnedTeamID.MatchString(teamID) || !teamsOwnedTeamID.MatchString(anchor) || !validTeamsLifecycleName(teamName) || !teamsChannelName(name) {
			return usageErr(errors.New("valid --team, --if-team-name, --anchor-channel, and non-General --name are required"))
		}
		if execute && verifyOnly {
			return usageErr(errors.New("--verify-only cannot be combined with --execute"))
		}
		if flags.dryRun || (!execute && !verifyOnly) {
			return flags.printJSON(cmd, map[string]any{"executed": false, "action": "create standard channel", "team_id": teamID, "if_team_name": teamName, "anchor_channel": anchor, "name": name, "preflight": "not_checked", "invitations_in_request": false})
		}
		if execute && !confirm {
			return usageErr(errors.New("--execute requires --confirm"))
		}
		if execute && cliutil.IsAnyHarness() {
			return errors.New("channel writes are disabled in verification and dogfood harnesses")
		}
		if execute && localDataDisabled() {
			return errors.New("channel creation requires local receipt storage")
		}
		c, err := flags.newClient()
		if err != nil {
			return err
		}
		if c == nil || c.HTTPClient == nil || c.Config == nil || c.Config.AuthSource != "web-session" || !teamsOrigin(c.BaseURL) {
			return authErr(errors.New("a verified-origin Teams web session is required"))
		}
		guard, ok := c.HTTPClient.Transport.(teamsOriginTransport)
		if !ok || len(guard.scopes["mt"]) == 0 {
			return errors.New("an observed Teams management audience is required")
		}
		principal, ok := teamsPrincipal(c.Config.AuthHeader())
		if !ok {
			return authErr(errors.New("Teams account identity is unavailable"))
		}
		if err = teamsOwnedTeamPreflight(cmd, flags, c, teamID, teamName); err != nil {
			return err
		}
		var group string
		if anchor == teamID {
			group, err = readTeamsGeneralAnchor(cmd, flags, c, teamID, teamName)
		} else {
			group, err = readTeamsChannelThread(cmd, flags, c, teamID, anchor, "", "")
		}
		if err != nil {
			return err
		}
		if verifyOnly {
			return flags.printJSON(cmd, map[string]any{"executed": false, "action": "verify standard channel creation", "preflight": "verified", "owner_and_single_human_roster": true, "anchor_matches_team": true})
		}
		dir, err := teamsChannelReceiptDir(principal, true)
		if err != nil {
			return err
		}
		ctx, cancel := boundCtx(cmd.Context(), flags)
		defer cancel()
		id, err := performTeamsChannelWrite(ctx, c, "create", teamID, "", name, group)
		if err != nil {
			return err
		}
		receipt := teamsChannelReceipt{Version: 1, State: "active", TeamID: teamID, ChannelID: id, Name: name, GroupID: group}
		if err = writeTeamsChannelReceipt(dir, receipt); err != nil {
			return flags.printJSON(cmd, map[string]any{"executed": true, "status": "accepted", "channel_id": id, "receipt_saved": false, "verification": "not_checked", "note": "inspect Teams and remove the channel manually if needed"})
		}
		return flags.printJSON(cmd, map[string]any{"executed": true, "status": "accepted", "channel_id": id, "receipt_saved": true, "verification": "not_checked"})
	}
	return cmd
}
func newTeamsChannelDeleteCmd(flags *rootFlags) *cobra.Command {
	var teamID, teamName, name string
	var execute, confirm, verifyOnly bool
	cmd := &cobra.Command{Use: "delete [channel-id]", Short: "Preview or delete a locally receipted standard channel", Annotations: map[string]string{"mcp:read-only-unless-flags": "execute,confirm"}}
	cmd.Flags().StringVar(&teamID, "team", "", "Owning team ID")
	cmd.Flags().StringVar(&teamName, "if-team-name", "", "Require the exact current team name")
	cmd.Flags().StringVar(&name, "if-name", "", "Require the exact current channel name")
	cmd.Flags().BoolVar(&execute, "execute", false, "Execute the previewed deletion")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm the deletion")
	cmd.Flags().BoolVar(&verifyOnly, "verify-only", false, "Read receipt, ownership and channel without deleting")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 || !teamsOwnedTeamID.MatchString(args[0]) || !teamsOwnedTeamID.MatchString(teamID) || args[0] == teamID || !validTeamsLifecycleName(teamName) || !teamsChannelName(name) {
			return usageErr(errors.New("a valid channel ID, --team, --if-team-name, and non-General --if-name are required"))
		}
		if execute && verifyOnly {
			return usageErr(errors.New("--verify-only cannot be combined with --execute"))
		}
		if flags.dryRun || (!execute && !verifyOnly) {
			return flags.printJSON(cmd, map[string]any{"executed": false, "action": "delete receipted standard channel", "team_id": teamID, "channel_id": args[0], "if_team_name": teamName, "if_name": name, "preflight": "not_checked"})
		}
		if execute && !confirm {
			return usageErr(errors.New("--execute requires --confirm"))
		}
		if execute && cliutil.IsAnyHarness() {
			return errors.New("channel writes are disabled in verification and dogfood harnesses")
		}
		c, err := flags.newClient()
		if err != nil {
			return err
		}
		if c == nil || c.Config == nil || c.Config.AuthSource != "web-session" || !teamsOrigin(c.BaseURL) {
			return authErr(errors.New("a verified-origin Teams web session is required"))
		}
		principal, ok := teamsPrincipal(c.Config.AuthHeader())
		if !ok {
			return authErr(errors.New("Teams account identity is unavailable"))
		}
		dir, err := teamsChannelReceiptDir(principal, false)
		if err != nil {
			return err
		}
		if verifyOnly {
			receipt, err := readTeamsChannelReceipt(dir, teamID, args[0], name)
			if err != nil {
				return err
			}
			if err = teamsOwnedTeamPreflight(cmd, flags, c, teamID, teamName); err != nil {
				return err
			}
			if _, err = readTeamsChannelThread(cmd, flags, c, teamID, args[0], name, receipt.GroupID); err != nil {
				return err
			}
			return flags.printJSON(cmd, map[string]any{"executed": false, "action": "verify receipted standard channel", "preflight": "verified", "receipt_matches": true, "owner_and_single_human_roster": true, "channel_matches": true})
		}
		id, updated, err := claimTeamsChannelDelete(dir, teamID, args[0], name, func(receipt teamsChannelReceipt) error {
			if err := teamsOwnedTeamPreflight(cmd, flags, c, teamID, teamName); err != nil {
				return err
			}
			_, err := readTeamsChannelThread(cmd, flags, c, teamID, args[0], name, receipt.GroupID)
			return err
		}, func(receipt teamsChannelReceipt) (string, error) {
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			return performTeamsChannelWrite(ctx, c, "delete", teamID, args[0], name, receipt.GroupID)
		})
		if err != nil {
			return err
		}
		return flags.printJSON(cmd, map[string]any{"executed": true, "status": "accepted", "channel_id": id, "receipt_updated": updated, "verification": "not_checked"})
	}
	return cmd
}
func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		group := &cobra.Command{Use: "channels", Annotations: map[string]string{"pp:parent-group": "true"}, RunE: parentNoSubcommandRunE(flags)}
		group.AddCommand(newTeamsChannelCreateCmd(flags), newTeamsChannelDeleteCmd(flags))
		addTeamsCommandGroup(root, group)
	})
}
