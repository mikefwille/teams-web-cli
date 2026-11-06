package cli

import (
	"encoding/json"
	"net/url"
	"testing"
)

func TestTeamsCalendarContinuationAndChangesValidation(t *testing.T) {
	template := `{"__type":"SyncCalendarEventsJsonRequest:#Exchange","Header":{"TimeZoneContext":{"TimeZoneDefinition":{"Id":"Mountain Standard Time"}}},"Body":{"CalendarId":{"BaseFolderId":{"Id":"folder"}}}}`
	scope := teamsAudienceScope{Headers: map[string]string{"x-owa-urlpostdata": url.QueryEscape(template)}}
	headers, _, err := teamsCalendarRequestWithState(scope, "SyncCalendarEvents", "", "2026-11-02T00:00:00-07:00", "", "opaque")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := url.QueryUnescape(headers["x-owa-urlpostdata"])
	var root map[string]any
	if json.Unmarshal([]byte(raw), &root) != nil {
		t.Fatal("invalid request")
	}
	body := root["Body"].(map[string]any)
	if body["SyncState"] != "opaque" || body["StartDate"] != "2026-11-02T00:00:00.000" {
		t.Fatal("continuation not inserted")
	}
	result := map[string]any{"results": []any{map[string]any{"Item": map[string]any{"ItemId": map[string]any{"Id": "one"}, "Subject": "fixture"}}}, "deleted_ids": []any{map[string]any{"Id": "two"}}, "sync_state": "next", "partial": false}
	updated, deleted, state, complete, err := calendarPageRecords(result)
	if err != nil || len(updated) != 1 || updated[0].ID != "one" || len(deleted) != 1 || deleted[0] != "two" || state != "next" || !complete {
		t.Fatalf("unexpected calendar page: %v", err)
	}
	if result["results"].([]any)[0].(map[string]any)["Subject"] != "fixture" {
		t.Fatal("wrapped calendar event was not exposed")
	}
	result["deleted_ids"] = []any{map[string]any{"unexpected": "value"}}
	if _, _, _, _, err := calendarPageRecords(result); err == nil {
		t.Fatal("unknown deletion shape accepted")
	}
}
