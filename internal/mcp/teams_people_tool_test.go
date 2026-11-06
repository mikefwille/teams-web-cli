package mcp

import (
	"testing"

	"github.com/mark3labs/mcp-go/server"
)

func TestTeamsPeopleSearchIsAvailableInReadOnlyMCP(t *testing.T) {
	t.Setenv("TEAMS_WEB_MCP_READ_ONLY", "1")
	s := server.NewMCPServer("people-test", "test")
	RegisterTools(s)
	entry, ok := s.ListTools()["people_search"]
	if !ok || entry == nil {
		t.Fatal("read-only people search was not registered")
	}
	if entry.Tool.Annotations.ReadOnlyHint == nil || !*entry.Tool.Annotations.ReadOnlyHint {
		t.Fatal("people search is not marked read-only")
	}
	if _, ok := entry.Tool.InputSchema.Properties["query"]; !ok {
		t.Fatal("people search query input is unavailable")
	}
	required := false
	for _, name := range entry.Tool.InputSchema.Required {
		required = required || name == "query"
	}
	if !required {
		t.Fatal("people search query is not required")
	}
}
