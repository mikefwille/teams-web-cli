// Native self-chat Hide/Unhide uses the observed ordered property pair.
package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
)

// One timestamp is shared by the two Hide writes, as in the captured pair.
// An error after the first accepted write is explicitly partial and is never
// compensated or retried without an independent state inspection.
func performTeamsNativeVisibilityWrite(ctx context.Context, c *client.Client, hide bool, now time.Time) error {
	var unpinned any
	if hide {
		stamp := now.UnixMilli()
		if stamp <= 0 {
			return errors.New("invalid current time")
		}
		unpinned = stamp
	}
	if err := performTeamsVisibilityProperty(ctx, c, "unpinnedTime", unpinned); err != nil {
		return err
	}
	if err := performTeamsVisibilityWrite(ctx, c, hide, now); err != nil {
		return fmt.Errorf("partial_write: unpinnedTime was accepted but the history property did not complete; inspect the self chat before another write: %w", err)
	}
	return nil
}
func configureTeamsNativeChatVisibility(cmd *cobra.Command, flags *rootFlags, hide bool) {
	var chat string
	var execute, confirm bool
	action := "unhide"
	if hide {
		action = "hide"
	}
	cmd.Short = "Preview or " + action + " the current account's self chat"
	cmd.Args = cobra.NoArgs
	cmd.Annotations = map[string]string{"mcp:read-only-unless-flags": "execute,confirm"}
	cmd.Flags().StringVar(&chat, "chat", "", "Self-chat ID (48:notes)")
	cmd.Flags().BoolVar(&execute, "execute", false, "Execute the previewed change")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm the change")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if chat != teamsSelfChat {
			return usageErr(errors.New("chat visibility currently supports only --chat 48:notes"))
		}
		effect := "request to unhide the self chat; rendered section requires independent verification"
		if hide {
			effect = "request to hide the self chat; list visibility and rendered section require independent verification"
		}
		if flags.dryRun || !execute {
			return flags.printJSON(cmd, map[string]any{"executed": false, "action": action, "chat": chat, "current_state": "not_checked", "effect": effect, "property_writes": []string{"unpinnedTime", "historyHiddenTime"}, "explicit_read_state_write": false, "note": "the two-write candidate needs independent state verification; inspect before retrying an uncertain or partial result"})
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
			return performTeamsNativeVisibilityWrite(ctx, c, hide, time.Now())
		})
		if err != nil {
			return err
		}
		if !locked {
			return errors.New("another visibility change for this self chat is in progress")
		}
		return flags.printJSON(cmd, map[string]any{"executed": true, "status": "accepted", "verification": "not_checked", "action": action, "chat": chat, "effect": effect, "property_writes": []string{"unpinnedTime", "historyHiddenTime"}, "explicit_read_state_write": false})
	}
}
func newTeamsChatHideCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "hide"}
	configureTeamsNativeChatVisibility(cmd, flags, true)
	return cmd
}
func newTeamsChatUnhideCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "unhide"}
	configureTeamsNativeChatVisibility(cmd, flags, false)
	return cmd
}
func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		group := &cobra.Command{Use: "chats", Annotations: map[string]string{"pp:parent-group": "true"}, RunE: parentNoSubcommandRunE(flags)}
		group.AddCommand(newTeamsChatHideCmd(flags), newTeamsChatUnhideCmd(flags))
		addTeamsCommandGroup(root, group)
	})
}
