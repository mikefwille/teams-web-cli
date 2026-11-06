// Wire Teams subcommands into a generated group when one already exists.
package cli

import "github.com/spf13/cobra"

func addTeamsCommandGroup(parent, candidate *cobra.Command) {
	if parent == nil || candidate == nil {
		return
	}
	for _, existing := range parent.Commands() {
		if existing.Name() != candidate.Name() {
			continue
		}
		for _, child := range candidate.Commands() {
			addNovelCommandIfAbsent(existing, child)
		}
		return
	}
	parent.AddCommand(candidate)
}
