package cli

import (
	"bytes"
	"encoding/json"
	"testing"
)

func syntheticSearchHit(thread, id, from, sent string, attachments any) map[string]any {
	return map[string]any{"Source": map[string]any{
		"ClientThreadId": thread, "Id": id,
		"From":         map[string]any{"EmailAddress": map[string]any{"Address": from}},
		"DateTimeSent": sent, "HasAttachments": attachments,
	}}
}

func TestTeamsSearchTermsAreSeparateLiteralRequests(t *testing.T) {
	terms, err := teamsSearchTerms("  alpha  beta ALPHA ")
	if err != nil || len(terms) != 2 || terms[0] != "alpha" || terms[1] != "beta" {
		t.Fatalf("unexpected terms %#v: %v", terms, err)
	}
	for _, query := range []string{"", "alpha OR beta", "NOT", "alpha:beta", `"alpha beta"`, "one two three four five"} {
		if _, err := teamsSearchTerms(query); err == nil {
			t.Fatalf("accepted unsupported query %q", query)
		}
	}
	for _, term := range terms {
		body, err := teamsSearchPayload("48:notes", term, "UTC", 0, 25)
		if err != nil {
			t.Fatal(err)
		}
		request := body["entityRequests"].([]any)[0].(map[string]any)
		query := request["query"].(map[string]string)
		if query["displayQueryString"] != term {
			t.Fatalf("term was not isolated: %#v", query)
		}
	}
}

func TestTeamsSearchIntersectionUsesThreadAndMessageIdentity(t *testing.T) {
	a := syntheticSearchHit("chat-a", "1", "a@example.test", "2026-11-06T12:00:00Z", false)
	b := syntheticSearchHit("chat-a", "2", "b@example.test", "2026-11-06T12:00:00Z", true)
	c := syntheticSearchHit("chat-b", "1", "a@example.test", "2026-11-06T12:00:00Z", false)
	missing := map[string]any{"Source": map[string]any{"ClientThreadId": "chat-a"}}
	got, unknown := intersectTeamsSearchPages([][]map[string]any{{a, b, a, c, missing}, {b, c, missing}, {c, b}})
	if len(got) != 2 || got[0]["Source"].(map[string]any)["Id"] != "2" || got[1]["Source"].(map[string]any)["ClientThreadId"] != "chat-b" || unknown != 2 {
		t.Fatalf("intersection = %#v, unavailable = %d", got, unknown)
	}
}

func TestTeamsSearchFiltersUseReturnedMetadataAndReportUnknown(t *testing.T) {
	required := true
	f, err := parseTeamsSearchFilters("A@example.test", "2026-11-06T12:00:00Z", "2026-11-06T13:00:00Z", &required)
	if err != nil {
		t.Fatal(err)
	}
	hits := []map[string]any{
		syntheticSearchHit("chat", "1", "a@example.test", "2026-11-06T12:00:00Z", true),
		syntheticSearchHit("chat", "2", "a@example.test", "2026-11-06T13:00:00Z", true),
		syntheticSearchHit("chat", "3", "other@example.test", "2026-11-06T12:30:00Z", true),
		syntheticSearchHit("chat", "4", "a@example.test", "not-a-time", true),
		syntheticSearchHit("chat", "5", "a@example.test", "2026-11-06T12:30:00Z", nil),
	}
	got, unknown := filterTeamsSearchHits(hits, f)
	if len(got) != 1 || got[0]["Source"].(map[string]any)["Id"] != "1" || unknown != 2 {
		t.Fatalf("filtered = %#v, unavailable = %d", got, unknown)
	}
	if _, err := parseTeamsSearchFilters("", "2026-11-06T13:00:00Z", "2026-11-06T12:00:00Z", nil); err == nil {
		t.Fatal("accepted reversed time window")
	}
}

func TestTeamsSearchDryRunAndLocalFilterGuard(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	t.Setenv("TEAMS_WEB_SESSION_DIR", t.TempDir())
	for _, tc := range []struct {
		args    []string
		wantErr bool
	}{
		{[]string{"search", "messages", "--query", "alpha beta", "--dry-run", "--json", "--no-learn"}, false},
		{[]string{"search", "messages", "--query", "alpha beta", "--offset", "25", "--dry-run", "--json", "--no-learn"}, true},
		{[]string{"search", "messages", "--query", "alpha", "--local", "--from", "a@example.test", "--dry-run", "--json", "--no-learn"}, true},
	} {
		root := newRootCmd(&rootFlags{})
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(tc.args)
		err := root.Execute()
		if (err != nil) != tc.wantErr {
			t.Fatalf("args %v: %v", tc.args, err)
		}
		if !tc.wantErr {
			var result map[string]any
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result["term_requests"] != float64(2) {
				t.Fatalf("dry run = %s, %v", out.String(), err)
			}
		}
	}
}
