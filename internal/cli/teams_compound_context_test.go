package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func TestTeamsObservedQuoteContextResolvesOnlySelectedPages(t *testing.T) {
	data, err := os.ReadFile("testdata/teams_quote_sanitized.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct{ Source, Quote map[string]any }
	if json.Unmarshal(data, &f) != nil {
		t.Fatal("bad synthetic captured-shape fixture")
	}
	unrelated := map[string]any{"id": "unrelated", "properties": map[string]any{"qtdMsgs": "malformed"}}
	messages := []map[string]any{f.Source, unrelated, f.Quote}
	neighbors, target := teamsNeighborWindow(messages, 0, 0, 0)
	if len(neighbors) != 1 || target != 0 {
		t.Fatal("bounded neighbor window incorrect")
	}
	ctx := teamsObservedQuoteContext(messages, 0)
	related := ctx["related"].([]map[string]any)
	if len(related) != 1 || related[0]["relation"] != "quotes_target" || related[0]["message_id"] != "synthetic-quote" || related[0]["resolved"] != true || ctx["invalid_quote_metadata_count"] != 1 || ctx["relationship_partial"] != true {
		t.Fatalf("reverse relationship missing: %#v", ctx)
	}
	ctx = teamsObservedQuoteContext(messages, 2)
	related = ctx["related"].([]map[string]any)
	if len(related) != 1 || related[0]["relation"] != "target_quotes" || related[0]["message_id"] != "synthetic-source" || related[0]["resolved"] != true {
		t.Fatalf("source relationship missing: %#v", ctx)
	}
	missing := []map[string]any{f.Quote}
	ctx = teamsObservedQuoteContext(missing, 0)
	related = ctx["related"].([]map[string]any)
	if len(related) != 1 || related[0]["resolved"] != false || related[0]["message"] != nil {
		t.Fatal("outside-page reference was not explicitly unresolved")
	}
}
func TestTeamsObservedQuoteContextRejectsAmbiguousMetadataAndBoundsOutput(t *testing.T) {
	ids, ok := teamsQuoteReferences(map[string]any{"properties": map[string]any{"qtdMsgs": []any{map[string]any{"messageId": "valid"}}}})
	if !ok || len(ids) != 1 || ids[0] != "valid" {
		t.Fatal("structured quote array not accepted")
	}
	for _, m := range []map[string]any{
		{"properties": "invalid"},
		{"properties": map[string]any{"qtdMsgs": "not-json"}},
		{"properties": map[string]any{"qtdMsgs": `[{"messageId":"../unsafe"}]`}},
		{"properties": map[string]any{"qtdMsgs": "[]", "QTDMSGS": "[]"}},
	} {
		if _, ok := teamsQuoteReferences(m); ok {
			t.Fatalf("accepted malformed reference: %#v", m)
		}
	}
	messages := []map[string]any{{"id": "target", "properties": map[string]any{}}}
	for i := 0; i < 25; i++ {
		messages = append(messages, map[string]any{"id": fmt.Sprintf("reply-%d", i), "properties": map[string]any{"qtdMsgs": `[{"messageId":"target"}]`}})
	}
	ctx := teamsObservedQuoteContext(messages, 0)
	if ctx["related_count"] != 20 || ctx["related_truncated"] != true {
		t.Fatalf("unbounded related results: %#v", ctx)
	}
}
func TestTeamsMentionReviewReturnsBoundedContextAndUnknownCounts(t *testing.T) {
	messages := []map[string]any{
		{"id": "before", "properties": map[string]any{"mentions": "[]"}},
		{"id": "hit-a", "properties": map[string]any{"mentions": `[{"mentioned":{"id":"8:orgid:mine"}}]`}},
		{"id": "unknown"},
		{"id": "hit-b", "properties": map[string]any{"mentions": []any{map[string]any{"id": "8:orgid:mine"}}}},
		{"id": "other", "properties": map[string]any{"mentions": `[{"id":"8:orgid:other"}]`}},
	}
	hits, contexts, unknown, matched := teamsMentionReview(messages, "8:orgid:mine", 1, 1, 1)
	if len(hits) != 1 || len(contexts) != 1 || unknown != 1 || matched != 2 || hits[0]["id"] != "hit-a" {
		t.Fatal("mention count or limit incorrect")
	}
	c := contexts[0]
	neighbors := c["results"].([]map[string]any)
	if c["message_id"] != "hit-a" || c["target_index"] != 1 || c["partial"] != true || len(neighbors) != 3 || neighbors[0]["id"] != "before" || neighbors[2]["id"] != "unknown" {
		t.Fatalf("bad mention context: %#v", c)
	}
	hits, contexts, unknown, matched = teamsMentionReview(messages, "8:orgid:absent", 5, 0, 0)
	if hits == nil || contexts == nil || len(hits) != 0 || len(contexts) != 0 || unknown != 1 || matched != 0 {
		t.Fatal("empty review not represented correctly")
	}
}

func TestTeamsMentionContextBoundsAreValidated(t *testing.T) {
	for _, tc := range []struct {
		before, after int
		wantError     bool
	}{{0, 0, false}, {10, 10, false}, {-1, 0, true}, {0, 11, true}} {
		root := newRootCmd(&rootFlags{})
		root.SetArgs([]string{"mentions", "--conversation", "48:notes", "--before", fmt.Sprint(tc.before), "--after", fmt.Sprint(tc.after), "--dry-run", "--json", "--no-learn"})
		err := root.Execute()
		if (err != nil) != tc.wantError {
			t.Fatalf("before=%d after=%d err=%v", tc.before, tc.after, err)
		}
	}
}
