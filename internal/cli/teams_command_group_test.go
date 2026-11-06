package cli

import (
	"strings"
	"testing"
)

// The generated novel parent groups are installed before preserved Teams
// hooks. A generic parent must not hide a real leaf or its sibling commands.
func TestTeamsGeneratedGroupsExposeImplementedCommands(t *testing.T) {
	root := RootCmd()
	cases := []struct {
		path  []string
		flags []string
	}{
		{[]string{"sync", "messages"}, []string{"conversation", "resume", "max-pages"}},
		{[]string{"search", "messages"}, []string{"local", "from", "limit"}},
		{[]string{"messages", "context"}, []string{"conversation", "message-id", "max-pages"}},
		{[]string{"messages", "list"}, []string{"conversation", "max-pages"}},
		{[]string{"messages", "send"}, []string{"conversation", "execute", "confirm"}},
		{[]string{"messages", "send-rich"}, []string{"bold-text", "execute", "confirm"}},
		{[]string{"messages", "reply"}, []string{"conversation", "execute", "confirm"}},
		{[]string{"catch-up"}, []string{"conversation", "since"}},
		{[]string{"mentions"}, []string{"conversation", "limit"}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.path, "/"), func(t *testing.T) {
			cmd, remaining, err := root.Find(tc.path)
			if err != nil || len(remaining) != 0 || cmd == nil || cmd.Name() != tc.path[len(tc.path)-1] {
				t.Fatalf("command %v did not resolve: remaining=%v err=%v", tc.path, remaining, err)
			}
			if isNovelScaffoldCommand(cmd) || cmd.RunE == nil {
				t.Fatalf("command %v resolved to a scaffold or non-runnable group", tc.path)
			}
			for _, name := range tc.flags {
				if cmd.Flags().Lookup(name) == nil {
					t.Errorf("command %v is missing --%s", tc.path, name)
				}
			}
		})
	}
}
