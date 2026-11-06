package cli

import "github.com/spf13/cobra"

func newTeamsFileDeleteCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "delete", Short: "Preview or recycle an owned file"}
	configureTeamsFileChange(cmd, flags, "DeleteFile")
	return cmd
}
