package cli

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func syntheticBackwardLink(state string) string {
	u := url.URL{Scheme: "https", Host: "teams.cloud.microsoft", Path: "/api/chatsvc/amer/v1/users/ME/conversations/48:notes/messages"}
	u.RawQuery = url.Values{"startTime": {"0"}, "syncState": {state}, "pageSize": {"2"}, "view": {"msnp24Equivalent"}}.Encode()
	return u.String()
}

func TestTeamsBackwardLinkKeepsObservedZeroStartAndView(t *testing.T) {
	point, err := teamsBackwardPoint(syntheticBackwardLink("synthetic-state"), "48:notes", 2)
	if err != nil {
		t.Fatal(err)
	}
	params, err := teamsHistoryPointParams(point, 2)
	if err != nil || params["startTime"] != "0" || params["syncState"] != "synthetic-state" || params["pageSize"] != "2" || params["view"] != "msnp24Equivalent" {
		t.Fatalf("continuation parameters: %#v %v", params, err)
	}
	good := syntheticBackwardLink("synthetic-state")
	for _, bad := range []string{
		strings.Replace(good, "teams.cloud.microsoft", "evil.invalid", 1),
		strings.Replace(good, "teams.cloud.microsoft", "teams.cloud.microsoft:443", 1),
		strings.Replace(good, "teams.cloud.microsoft", "other@teams.cloud.microsoft", 1),
		strings.Replace(good, "48:notes", "other", 1),
		strings.Replace(good, "msnp24Equivalent", "unobserved", 1),
		good + "&extra=1", good + "&pageSize=2", good + "#fragment",
		strings.Replace(good, "pageSize=2", "pageSize=200", 1),
		strings.Replace(good, "syncState=synthetic-state", "syncState=", 1),
	} {
		if _, err := teamsBackwardPoint(bad, "48:notes", 2); err == nil {
			t.Fatalf("accepted untrusted continuation %q", bad)
		}
	}
}

func TestTeamsBackwardWalkAdvancesAndKeepsOverlapTombstone(t *testing.T) {
	var calls []teamsHistoryPoint
	result, err := followTeamsHistoryBackward(teamsHistoryPoint{}, "48:notes", 2, 3, func(point teamsHistoryPoint) (teamsMessagePage, error) {
		calls = append(calls, point)
		switch len(calls) {
		case 1:
			return teamsMessagePage{Messages: []map[string]any{{"id": "new"}, {"id": "shared", "content": "old"}}, Metadata: map[string]any{"backwardLink": syntheticBackwardLink("s1")}}, nil
		case 2:
			return teamsMessagePage{Messages: []map[string]any{{"id": "shared", "properties": map[string]any{"deletetime": "123"}}, {"id": "older"}}, Metadata: map[string]any{"backwardLink": syntheticBackwardLink("s2")}}, nil
		default:
			return teamsMessagePage{Messages: []map[string]any{{"id": "oldest"}}, Metadata: map[string]any{"backwardLink": syntheticBackwardLink("s3")}}, nil
		}
	})
	if err != nil || len(calls) != 3 || calls[1].SyncState != "s1" || calls[2].SyncState != "s2" || !calls[1].HasStartTime || calls[1].View != "msnp24Equivalent" {
		t.Fatalf("walk: %#v calls=%#v err=%v", result, calls, err)
	}
	if result.PagesScanned != 3 || result.StopReason != "page_limit" || result.OverlapCount != 1 || len(result.Messages) != 4 || !teamsMessageDeleted(result.Messages[1]) || result.Messages[3]["id"] != "oldest" {
		t.Fatalf("walk results: %#v", result)
	}
}

func TestTeamsBackwardWalkRefusesInvalidOrNonAdvancingPages(t *testing.T) {
	for name, next := range map[string]string{
		"off origin":  strings.Replace(syntheticBackwardLink("s1"), "teams.cloud.microsoft", "evil.invalid", 1),
		"no progress": syntheticBackwardLink("s1"),
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			result, err := followTeamsHistoryBackward(teamsHistoryPoint{}, "48:notes", 2, 3, func(point teamsHistoryPoint) (teamsMessagePage, error) {
				calls++
				return teamsMessagePage{Messages: []map[string]any{{"id": "same"}}, Metadata: map[string]any{"backwardLink": next}}, nil
			})
			if err == nil || len(result.Messages) != 0 || (name == "off origin" && calls != 1) || (name == "no progress" && calls != 2) {
				t.Fatalf("unsafe walk: %#v %v calls=%d", result, err, calls)
			}
		})
	}
}

func TestTeamsHistoryPlanIsBoundedAndRejectsRepeatedSelectors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pages.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"pages":[{"start_time":123,"sync_state":"synthetic-a"},{"start_time":100}]}`)
	points, err := readTeamsHistoryPlan(file, teamsHistoryPoint{}, 10)
	if err != nil || len(points) != 3 || points[1].StartTime != 123 || points[2].StartTime != 100 {
		t.Fatalf("plan: %#v %v", points, err)
	}
	for _, body := range []string{
		`{"pages":[{"start_time":1},{"start_time":1}]}`,
		`{"pages":[{}]}`,
		`{"pages":[{"start_time":-1}]}`,
		`{"pages":[{"start_time":1,"unexpected":true}]}`,
		`{"pages":[]} {}`,
	} {
		write(body)
		if _, err := readTeamsHistoryPlan(file, teamsHistoryPoint{}, 10); err == nil {
			t.Fatalf("accepted invalid plan %s", body)
		}
	}
	if _, err := teamsHistoryParams(strings.Repeat("x", 65537), 0, 10); err == nil {
		t.Fatal("accepted oversized cursor")
	}
}

func TestTeamsHistoryScanFindsOlderTargetAndPreservesTombstones(t *testing.T) {
	points := []teamsHistoryPoint{{}, {StartTime: 100, SyncState: "synthetic-next"}}
	calls := []teamsHistoryPoint{}
	result, err := scanTeamsHistory(points, func(p teamsHistoryPoint) (teamsMessagePage, error) {
		calls = append(calls, p)
		if len(calls) == 1 {
			return teamsMessagePage{Messages: []map[string]any{{"id": "new", "content": "before delete"}, {"id": "overlap", "content": "first"}}, Metadata: map[string]any{"syncState": "synthetic-next"}}, nil
		}
		return teamsMessagePage{Messages: []map[string]any{{"id": "overlap", "content": "different"}, {"id": "new", "properties": map[string]any{"deletetime": "123"}}, {"id": "older", "content": "target"}}, Metadata: map[string]any{}}, nil
	})
	if err != nil || len(calls) != 2 || calls[1] != points[1] || result.PagesScanned != 2 || len(result.Messages) != 3 || result.OverlapCount != 2 || result.ConflictingCopies != 2 {
		t.Fatalf("scan: %#v calls=%#v err=%v", result, calls, err)
	}
	if result.Messages[2]["id"] != "older" || !teamsMessageDeleted(result.Messages[0]) || result.Messages[1]["content"] != "first" {
		t.Fatalf("older target or tombstone lost: %#v", result.Messages)
	}
}

func TestTeamsHistoryScanFailsClosedOnLaterPageFailure(t *testing.T) {
	calls := 0
	result, err := scanTeamsHistory([]teamsHistoryPoint{{}, {SyncState: "synthetic-bad"}}, func(teamsHistoryPoint) (teamsMessagePage, error) {
		calls++
		if calls == 1 {
			return teamsMessagePage{Messages: []map[string]any{{"id": "visible"}}}, nil
		}
		return teamsMessagePage{}, errors.New("cursor rejected")
	})
	if err == nil || !strings.Contains(err.Error(), "page 2") || calls != 2 || len(result.Messages) != 0 {
		t.Fatalf("partial scan accepted: %#v %v calls=%d", result, err, calls)
	}
}
