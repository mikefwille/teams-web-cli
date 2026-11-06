package mcp

import (
	"encoding/json"
	"github.com/mark3labs/mcp-go/server"
	"testing"
)

func TestTeamsMentionsContextFlagsRemainReadOnly(t *testing.T) {
	t.Setenv("TEAMS_WEB_MCP_READ_ONLY", "1")
	s := server.NewMCPServer("mentions-context", "test")
	RegisterTools(s)
	entry, ok := s.ListTools()["mentions"]
	if !ok || entry == nil || entry.Tool.Annotations.ReadOnlyHint == nil || !*entry.Tool.Annotations.ReadOnlyHint {
		t.Fatal("mentions missing read-only hint")
	}
	for _, name := range []string{"before", "after"} {
		raw, ok := entry.Tool.InputSchema.Properties[name]
		encoded, err := json.Marshal(raw)
		var p map[string]any
		if !ok || err != nil || json.Unmarshal(encoded, &p) != nil || p["type"] != "number" {
			t.Fatalf("%s is not a numeric flag", name)
		}
	}
}
