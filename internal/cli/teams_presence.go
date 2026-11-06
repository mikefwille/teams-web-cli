// Own presence uses the observed read-only Teams presence query.
package cli

import (
	"encoding/json"
	"errors"

	"github.com/spf13/cobra"
)

const teamsPresencePath = "/ups/noam/v1/presence/getpresence/"

func decodeTeamsOwnPresence(data []byte, mri string) (map[string]any, error) {
	var items []map[string]any
	if json.Unmarshal(data, &items) != nil || len(items) != 1 {
		return nil, errors.New("Teams returned an unexpected presence response")
	}
	item := items[0]
	presence, ok := item["presence"].(map[string]any)
	if !ok || item["mri"] != mri || item["source"] != "ups" || item["status"] != float64(20000) {
		return nil, errors.New("Teams reported a presence read error")
	}
	return presence, nil
}

func newTeamsPresenceGetCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{Use: "get", Short: "Read the current account's presence", Example: "teams-web-pp-cli presence get --json --no-learn", Annotations: map[string]string{"mcp:read-only": "true", "pp:data-source": "live"}, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if flags.dataSource == "local" {
				return usageErr(errors.New("presence is read from the live Teams service"))
			}
			if dryRunOK(flags) {
				return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "presence get"})
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			if c.Config.AuthSource != "web-session" {
				return authErr(errors.New("a Teams web session is required"))
			}
			principal, ok := teamsPrincipal(c.Config.AuthHeader())
			if !ok {
				return authErr(errors.New("Teams session identity is unavailable"))
			}
			mri := "8:orgid:" + principal.Object
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			data, _, err := c.PostQueryWithParams(ctx, teamsPresencePath, nil, []any{map[string]string{"mri": mri, "source": "ups"}})
			if err != nil {
				return classifyAPIErrorOnly(err)
			}
			presence, err := decodeTeamsOwnPresence(data, mri)
			if err != nil {
				return err
			}
			return flags.printJSON(cmd, map[string]any{"result": presence, "source": "live_presence"})
		}}
}
func newTeamsPresenceCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "presence", Short: "Read or manage your Teams presence", Annotations: map[string]string{"pp:parent-group": "true"}, RunE: parentNoSubcommandRunE(flags)}
	cmd.AddCommand(newTeamsPresenceGetCmd(flags))
	cmd.AddCommand(newTeamsPresenceSetCmd(flags))
	cmd.AddCommand(newTeamsPresenceResetCmd(flags))
	cmd.AddCommand(newTeamsPresenceMessageCmd(flags))
	return cmd
}
func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) { addNovelCommandIfAbsent(root, newTeamsPresenceCmd(flags)) })
}
