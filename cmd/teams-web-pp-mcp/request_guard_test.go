package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestRequestGuardBlocksAmbiguousToolDispatch(t *testing.T) {
	hooks := &server.Hooks{}
	hooks.AddOnRequestInitialization(guardRequest)
	s := server.NewMCPServer("isolated-test", "0", server.WithToolCapabilities(false), server.WithHooks(hooks))
	calls := 0
	s.AddTool(mcp.NewTool("fixture_probe"), func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls++
		return mcp.NewToolResultText("fixture"), nil
	})
	cases := []struct {
		name  string
		raw   string
		valid bool
	}{
		{"ordinary tool call", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fixture_probe","arguments":{}}}`, true},
		{"case folded method", `{"jsonrpc":"2.0","id":2,"Method":"tools/call","params":{"name":"fixture_probe","arguments":{}}}`, false},
		{"duplicate method", `{"jsonrpc":"2.0","id":3,"method":"other","method":"tools/call","params":{"name":"fixture_probe","arguments":{}}}`, false},
		{"case folded name", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"Name":"fixture_probe","arguments":{}}}`, false},
		{"duplicate name", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"other","name":"fixture_probe","arguments":{}}}`, false},
		{"unicode folded params", `{"jsonrpc":"2.0","id":6,"method":"tools/call","paramſ":{"name":"fixture_probe","arguments":{}}}`, false},
		{"case folded arguments", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"fixture_probe","Arguments":{}}}`, false},
		{"duplicate nested arguments", `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"fixture_probe","arguments":{"value":1,"value":2}}}`, false},
		{"distinct nested case is preserved", `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"fixture_probe","arguments":{"payload":{"Name":"value","name":"other"}}}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := calls
			response := s.HandleMessage(context.Background(), json.RawMessage(tc.raw))
			encoded, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Error *struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if tc.valid {
				if decoded.Error != nil || calls != before+1 {
					t.Fatalf("valid call rejected or not dispatched: %s", encoded)
				}
			} else if decoded.Error == nil || decoded.Error.Code != -32600 || calls != before {
				t.Fatalf("ambiguous call was not rejected before dispatch: %s", encoded)
			}
		})
	}
}
