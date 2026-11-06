package mcp

import (
	"github.com/mark3labs/mcp-go/server"
	"testing"
)

func TestTeamsChannelCommandsAreGuardedInReadOnlyMCP(t *testing.T) {
	t.Setenv("TEAMS_WEB_MCP_READ_ONLY", "1")
	s := server.NewMCPServer("channel-test", "test")
	RegisterTools(s)
	for _, name := range []string{"channels_create", "channels_delete"} {
		entry, ok := s.ListTools()[name]
		if !ok || entry == nil {
			t.Fatalf("%s missing", name)
		}
		if entry.Tool.Annotations.ReadOnlyHint == nil || !*entry.Tool.Annotations.ReadOnlyHint {
			t.Fatalf("%s missing read-only guard", name)
		}
		for _, flag := range []string{"execute", "confirm"} {
			if _, ok := entry.Tool.InputSchema.Properties[flag]; ok {
				t.Fatalf("%s exposes write flag %s", name, flag)
			}
		}
	}
}
