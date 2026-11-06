// Copyright 2026 mikefwille and contributors. Licensed under Apache-2.0. See LICENSE.
// Novel command scaffold. Implement the RunE body before shipping.
// generate --force preserves implemented bodies; untouched TODO scaffolds may refresh.
// pp:data-source auto
// Supported strategies: auto, local, live, or computed. Change this default deliberately.

package cli

import (
	"github.com/spf13/cobra"
)

func newNovelSyncCmd(flags *rootFlags) *cobra.Command {

	cmd := &cobra.Command{
		Use:         "sync",
		Short:       "Work with sync",
		Example:     "  teams-web-pp-cli sync messages --help",
		Annotations: map[string]string{"mcp:read-only": "true", "pp:data-source": "auto", "pp:typed-exit-codes": "0,2"},
		RunE:        parentNoSubcommandRunE(flags),
	}
	addNovelCommandIfAbsent(cmd, newNovelSyncMessagesCmd(flags))
	return cmd
}
