package mcp

import (
	"github.com/mark3labs/mcp-go/server"
	"testing"
)

func TestTeamsNativeSelfVisibilityReadOnlyMCP(t *testing.T) {
	t.Setenv("TEAMS_WEB_MCP_READ_ONLY", "1")
	s := server.NewMCPServer("native-visibility", "test")
	RegisterTools(s)
	for _, name := range []string{"chats_hide", "chats_unhide"} {
		entry, ok := s.ListTools()[name]
		if !ok || entry == nil || entry.Tool.Annotations.ReadOnlyHint == nil || !*entry.Tool.Annotations.ReadOnlyHint {
			t.Fatalf("%s not available as read-only preview", name)
		}
		for _, f := range []string{"execute", "confirm"} {
			if _, ok := entry.Tool.InputSchema.Properties[f]; ok {
				t.Fatalf("%s exposes %s", name, f)
			}
		}
	}
}
