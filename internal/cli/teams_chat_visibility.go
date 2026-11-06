// Self-chat history visibility based on observed Teams web property updates.
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
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
)

const teamsSelfVisibilityBase = "/api/chatsvc/amer/v1/users/ME/conversations/48:notes/properties"

var teamsVisibilityLimiter = cliutil.NewAdaptiveLimiterAuto(1)

func teamsVisibilityPayload(hide bool, now time.Time) (map[string]any, error) {
	if !hide {
		return map[string]any{"historyHiddenTime": nil}, nil
	}
	stamp := now.UnixMilli()
	if stamp <= 0 {
		return nil, errors.New("invalid current time")
	}
	return map[string]any{"historyHiddenTime": strconv.FormatInt(stamp, 10)}, nil
}
func teamsVisibilityLockPath(principal teamsTokenPrincipal) (string, error) {
	if principal.Tenant == "" || principal.Object == "" {
		return "", errors.New("Teams account identity is unavailable")
	}
	base, err := cliutil.StateDir()
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(base, 0700); err != nil {
		return "", err
	}
	checkDir := func(p string) error {
		st, err := os.Lstat(p)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0 {
			return errors.New("visibility lock directory is not private")
		}
		return nil
	}
	if err = checkDir(base); err != nil {
		return "", err
	}
	dir := filepath.Join(base, "teams-chat-visibility")
	if err = os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if err = checkDir(dir); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(principal.Tenant + "\x00" + principal.Object + "\x00" + teamsSelfChat))
	lock := filepath.Join(dir, hex.EncodeToString(sum[:]))
	if st, err := os.Lstat(lock + ".lock"); err == nil {
		if !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0 {
			return "", errors.New("visibility lock file is not private")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return lock, nil
}

// The observed property update returns HTTP 200 and an empty body. A write is
// sent once; ambiguous responses never trigger an automatic retry.
func performTeamsVisibilityWrite(ctx context.Context, c *client.Client, hide bool, now time.Time) error {
	payload, err := teamsVisibilityPayload(hide, now)
	if err != nil {
		return err
	}
	return performTeamsVisibilityProperty(ctx, c, "historyHiddenTime", payload["historyHiddenTime"])
}

// Only the two captured self-chat properties are accepted. All callers hold
// the same account-scoped lock across their full visibility operation.
func performTeamsVisibilityProperty(ctx context.Context, c *client.Client, name string, value any) error {
	if name != "historyHiddenTime" && name != "unpinnedTime" {
		return errors.New("unsupported visibility property")
	}
	if value != nil {
		if name == "historyHiddenTime" {
			text, ok := value.(string)
			if !ok || len(text) != 13 {
				return errors.New("invalid history visibility timestamp")
			}
			n, err := strconv.ParseInt(text, 10, 64)
			if err != nil || n <= 0 {
				return errors.New("invalid history visibility timestamp")
			}
		} else {
			n, ok := value.(int64)
			if !ok || n <= 0 || len(strconv.FormatInt(n, 10)) != 13 {
				return errors.New("invalid unpinned timestamp")
			}
		}
	}
	if c == nil || c.HTTPClient == nil || c.Config == nil || c.Config.AuthSource != "web-session" || !teamsOrigin(c.BaseURL) {
		return authErr(errors.New("a verified-origin Teams web session is required"))
	}
	if _, ok := c.HTTPClient.Transport.(teamsOriginTransport); !ok {
		return errors.New("Teams session origin guard is unavailable")
	}
	data, err := json.Marshal(map[string]any{name: value})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.BaseURL+teamsSelfVisibilityBase+"?name="+name, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://teams.cloud.microsoft")
	if err = teamsVisibilityLimiter.Wait(ctx); err != nil {
		return err
	}
	hc := *c.HTTPClient
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return apiErr(errors.New("outcome_unknown: visibility request did not complete; inspect the self chat before another write"))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1_000_001))
	if err != nil || len(body) > 1_000_000 {
		return apiErr(errors.New("outcome_unknown: visibility response could not be read; inspect the self chat before another write"))
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		teamsVisibilityLimiter.OnRateLimit()
		return rateLimitErr(&cliutil.RateLimitError{URL: "Teams chat service", RetryAfter: cliutil.RetryAfter(resp)})
	}
	if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500 || (resp.StatusCode >= 300 && resp.StatusCode < 400) {
		return apiErr(fmt.Errorf("outcome_unknown: Teams returned HTTP %d; inspect the self chat before another write", resp.StatusCode))
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return apiErr(errors.New("outcome_unknown: unexpected visibility status; inspect the self chat"))
		}
		return classifyAPIErrorOnly(fmt.Errorf("Teams visibility request returned HTTP %d", resp.StatusCode))
	}
	if len(bytes.TrimSpace(body)) != 0 {
		return apiErr(errors.New("outcome_unknown: unexpected visibility response; inspect the self chat"))
	}
	teamsVisibilityLimiter.OnSuccess()
	return nil
}

func configureTeamsChatVisibility(cmd *cobra.Command, flags *rootFlags, hide bool) {
	var chat string
	var execute, confirm bool
	action := "show-history"
	if hide {
		action = "hide-history"
	}
	cmd.Short = "Preview or " + action + " in the current account's self chat"
	cmd.Args = cobra.NoArgs
	cmd.Annotations = map[string]string{"mcp:read-only-unless-flags": "execute,confirm"}
	cmd.Flags().StringVar(&chat, "chat", "", "Self-chat ID (48:notes)")
	cmd.Flags().BoolVar(&execute, "execute", false, "Execute the previewed change")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm the change")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if chat != teamsSelfChat {
			return usageErr(errors.New("history visibility changes currently support only --chat 48:notes"))
		}
		effect := "request to show prior self-chat history in the UI; rendered section is not determined"
		if hide {
			effect = "request to hide prior self-chat history in the UI; the observed API still returned history, and rendered section is not determined"
		}
		if flags.dryRun || !execute {
			return flags.printJSON(cmd, map[string]any{"executed": false, "action": action, "chat": chat, "current_state": "not_checked", "effect": effect, "explicit_read_state_write": false, "note": "pass --execute --confirm only after checking the self chat"})
		}
		if !confirm {
			return usageErr(errors.New("--execute requires --confirm"))
		}
		if cliutil.IsAnyHarness() {
			return errors.New("chat visibility writes are disabled in verification and dogfood harnesses")
		}
		c, err := flags.newClient()
		if err != nil {
			return err
		}
		if c == nil || c.Config == nil || c.Config.AuthSource != "web-session" {
			return authErr(errors.New("a Teams web session is required"))
		}
		principal, ok := teamsPrincipal(c.Config.AuthHeader())
		if !ok {
			return authErr(errors.New("Teams account identity is unavailable"))
		}
		lock, err := teamsVisibilityLockPath(principal)
		if err != nil {
			return err
		}
		locked, err := cliutil.TryWithFileLock(lock, func() error {
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			return performTeamsVisibilityWrite(ctx, c, hide, time.Now())
		})
		if err != nil {
			return err
		}
		if !locked {
			return errors.New("another visibility change for this self chat is in progress")
		}
		return flags.printJSON(cmd, map[string]any{"executed": true, "status": "accepted", "verification": "not_checked", "action": action, "chat": chat, "effect": effect, "explicit_read_state_write": false})
	}
}
func newTeamsChatHideHistoryCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "hide-history"}
	configureTeamsChatVisibility(cmd, flags, true)
	return cmd
}
func newTeamsChatShowHistoryCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "show-history"}
	configureTeamsChatVisibility(cmd, flags, false)
	return cmd
}
func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		group := &cobra.Command{Use: "chats", Annotations: map[string]string{"pp:parent-group": "true"}, RunE: parentNoSubcommandRunE(flags)}
		group.AddCommand(newTeamsChatHideHistoryCmd(flags), newTeamsChatShowHistoryCmd(flags))
		addTeamsCommandGroup(root, group)
	})
}
