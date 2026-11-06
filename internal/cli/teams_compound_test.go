package cli

import (
	"testing"
	"time"
)

func TestTeamsStructuredMentionsAndMessageTimes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		message      map[string]any
		match, known bool
	}{
		{"nested mention", map[string]any{"properties": map[string]any{"mentions": `[{"mentioned":{"id":"8:orgid:mine"}}]`}}, true, true},
		{"different account", map[string]any{"properties": map[string]any{"mentions": []any{map[string]any{"id": "8:orgid:other"}}}}, false, true},
		{"missing metadata", map[string]any{}, false, false},
		{"malformed metadata", map[string]any{"properties": map[string]any{"mentions": "not-json"}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, known := teamsStructuredMention(tc.message, "8:orgid:mine")
			if got != tc.match || known != tc.known {
				t.Fatalf("match=%v known=%v", got, known)
			}
		})
	}
	expected := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	for _, value := range []any{expected.Format(time.RFC3339), float64(expected.UnixMilli()), "1793966400000"} {
		got, ok := teamsMessageTime(map[string]any{"originalarrivaltime": value})
		if !ok || !got.Equal(expected) {
			t.Fatalf("time %v: %v %v", value, got, ok)
		}
	}
	if _, ok := teamsMessageTime(map[string]any{}); ok {
		t.Fatal("missing time accepted")
	}
}
