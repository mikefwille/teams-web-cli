// Preserve the generated registration while using the implemented command.
package cli

import "github.com/spf13/cobra"

func newNovelCatchUpCmd(flags *rootFlags) *cobra.Command {
	return newTeamsCatchUpCmd(flags)
}
