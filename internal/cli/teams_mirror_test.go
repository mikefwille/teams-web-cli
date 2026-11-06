package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mikefwille/teams-web-cli/internal/store"
)

func TestTeamsMirrorRecordsRejectUnusablePages(t *testing.T) {
	page := teamsMessagePage{Metadata: map[string]any{"syncState": "opaque"}, Messages: []map[string]any{{"id": "1", "content": "hello"}, {"id": "2", "properties": map[string]any{"deletetime": "2026-11-06"}}}}
	records, state, err := teamsMirrorRecords(page)
	if err != nil || state != "opaque" || len(records) != 2 || records[0].Deleted || !records[1].Deleted {
		t.Fatalf("invalid parsed page: %#v %q %v", records, state, err)
	}
	page.Metadata = nil
	if _, _, err := teamsMirrorRecords(page); err == nil {
		t.Fatal("page without a state was accepted")
	}
	page.Metadata = map[string]any{"syncState": "opaque"}
	page.Messages = []map[string]any{{"content": "missing ID"}}
	if _, _, err := teamsMirrorRecords(page); err == nil {
		t.Fatal("message without an ID was accepted")
	}
}

func TestTeamsMirrorBackwardCheckpointValidatesScopeAndResume(t *testing.T) {
	metadata := map[string]any{"backwardLink": syntheticBackwardLink("synthetic-next")}
	raw, err := teamsMirrorNextCheckpoint(metadata, "48:notes", 2)
	if err != nil || raw == "" {
		t.Fatalf("checkpoint: %v", err)
	}
	point, size, err := teamsMirrorResumePoint(raw, "48:notes", 200, false)
	if err != nil || size != 2 || point.SyncState != "synthetic-next" || !point.HasStartTime || point.StartTime != 0 || point.View != "msnp24Equivalent" {
		t.Fatalf("resume point: %#v %d %v", point, size, err)
	}
	if _, _, err := teamsMirrorResumePoint(raw, "48:notes", 3, true); err == nil {
		t.Fatal("accepted a changed page size")
	}
	if _, _, err := teamsMirrorResumePoint(raw, "other", 2, false); err == nil {
		t.Fatal("accepted another conversation's checkpoint")
	}
	metadata["backwardLink"] = strings.Replace(syntheticBackwardLink("synthetic-next"), "teams.cloud.microsoft", "evil.invalid", 1)
	if _, err := teamsMirrorNextCheckpoint(metadata, "48:notes", 2); err == nil {
		t.Fatal("accepted an off-origin checkpoint")
	}
	if got, err := teamsMirrorNextCheckpoint(map[string]any{}, "48:notes", 2); err != nil || got != "" {
		t.Fatalf("missing link: %q %v", got, err)
	}
}

func TestTeamsMirrorSyncResumeValidationBeforeIO(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	t.Setenv("TEAMS_WEB_SESSION_DIR", t.TempDir())
	for _, args := range [][]string{
		{"sync", "messages", "--conversation", "48:notes", "--resume", "--sync-state", "synthetic", "--dry-run", "--json", "--no-learn"},
		{"sync", "messages", "--conversation", "48:notes", "--max-pages", "11", "--dry-run", "--json", "--no-learn"},
	} {
		root := newRootCmd(&rootFlags{})
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		if err := root.Execute(); err == nil {
			t.Fatalf("accepted invalid sync arguments: %v", args)
		}
	}
}

func TestTeamsMirrorHeadRefreshRequiresDefaultSelectors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"default", nil, true},
		{"multiple pages", []string{"--max-pages=2"}, true},
		{"resume", []string{"--resume"}, false},
		{"explicit cursor", []string{"--sync-state=older"}, false},
		{"explicit empty cursor", []string{"--sync-state="}, false},
		{"explicit timestamp", []string{"--start-time=100"}, false},
		{"explicit zero timestamp", []string{"--start-time=0"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newTeamsSyncMessagesCmd(&rootFlags{})
			if err := cmd.Flags().Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			resume, err := cmd.Flags().GetBool("resume")
			if err != nil {
				t.Fatal(err)
			}
			cursor, err := cmd.Flags().GetString("sync-state")
			if err != nil {
				t.Fatal(err)
			}
			start, err := cmd.Flags().GetInt64("start-time")
			if err != nil {
				t.Fatal(err)
			}
			if got := teamsMirrorHeadRefresh(cmd, resume, cursor, start); got != tc.want {
				t.Fatalf("head refresh = %t, want %t", got, tc.want)
			}
		})
	}
	cmd := newTeamsSyncMessagesCmd(&rootFlags{})
	if teamsMirrorHeadRefresh(cmd, false, "indirect-cursor", 0) || teamsMirrorHeadRefresh(cmd, false, "", 100) {
		t.Fatal("nondefault selector was treated as head without an explicitly changed flag")
	}
}

func TestTeamsMirrorMixedHeadAndHistoricalPagesPreserveProvenance(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "mirror.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	initial := []store.TeamsMessageRecord{
		{ID: "a", Data: json.RawMessage(`{"id":"a","content":"originalhead"}`)},
		{ID: "b", Data: json.RawMessage(`{"id":"b","content":"retainededit"}`)},
	}
	if err := db.CommitTeamsMessagePage(ctx, "48:notes", "initial", initial, at); err != nil {
		t.Fatal(err)
	}
	pages := []teamsMessagePage{
		{Messages: []map[string]any{{"id": "a", "content": "refreshedhead"}, {"id": "c", "content": "newhead"}}, Metadata: map[string]any{"syncState": "sync-1", "backwardLink": syntheticBackwardLink("next")}},
		{Messages: []map[string]any{{"id": "a", "content": "oldhead"}, {"id": "b", "content": "stalehistory"}}, Metadata: map[string]any{"syncState": "sync-2"}},
	}
	index := 0
	scan, err := followTeamsHistoryBackward(teamsHistoryPoint{}, "48:notes", 2, 2, func(teamsHistoryPoint) (teamsMessagePage, error) {
		page := pages[index]
		index++
		return page, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	records, state, err := teamsMirrorRecords(teamsMessagePage{Messages: scan.Messages, Metadata: scan.Metadata[len(scan.Metadata)-1]})
	if err != nil {
		t.Fatal(err)
	}
	teamsMirrorMarkHeadRecords(records, scan, true)
	for _, record := range records {
		if record.RefreshExisting != (record.ID == "a" || record.ID == "c") {
			t.Fatalf("wrong head-page provenance for %q", record.ID)
		}
	}
	if err := db.CommitTeamsMessageBatch(ctx, "48:notes", state, "", records, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for word, want := range map[string]int{"originalhead": 0, "refreshedhead": 1, "newhead": 1, "retainededit": 1, "oldhead": 0, "stalehistory": 0} {
		hits, fresh, _, err := db.SearchTeamsMessages(ctx, word, "48:notes", 10)
		if err != nil || len(hits) != want || fresh.OldestPageAt != at.Format(time.RFC3339Nano) || fresh.NewestPageAt != at.Add(time.Minute).Format(time.RFC3339Nano) {
			t.Fatalf("search %s: hits=%d freshness=%#v err=%v", word, len(hits), fresh, err)
		}
	}
	history, _, err := teamsMirrorRecords(teamsMessagePage{Messages: scan.Messages, Metadata: scan.Metadata[len(scan.Metadata)-1]})
	if err != nil {
		t.Fatal(err)
	}
	teamsMirrorMarkHeadRecords(history, scan, false)
	for _, record := range history {
		if record.RefreshExisting {
			t.Fatal("historical selector marked a record as a head refresh")
		}
	}
}

func TestTeamsMirrorTwoBatchesResumeAndKeepEarlierDataOnFailure(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "mirror.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	first := teamsMessagePage{Messages: []map[string]any{{"id": "new", "content": "firstword"}, {"id": "removed", "properties": map[string]any{"deletetime": "1"}}}, Metadata: map[string]any{"syncState": "sync-1", "backwardLink": syntheticBackwardLink("next-1")}}
	scan, err := followTeamsHistoryBackward(teamsHistoryPoint{}, "48:notes", 2, 1, func(teamsHistoryPoint) (teamsMessagePage, error) { return first, nil })
	if err != nil {
		t.Fatal(err)
	}
	records, syncState, err := teamsMirrorRecords(teamsMessagePage{Messages: scan.Messages, Metadata: scan.Metadata[0]})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := teamsMirrorNextCheckpoint(scan.Metadata[0], "48:notes", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CommitTeamsMessageBatch(ctx, "48:notes", syncState, checkpoint, records, at); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	stored, _, err := db.TeamsMirrorBackfillCheckpoint(ctx, "48:notes")
	if err != nil {
		t.Fatal(err)
	}
	point, size, err := teamsMirrorResumePoint(stored, "48:notes", 200, false)
	if err != nil || point.SyncState != "next-1" || size != 2 {
		t.Fatalf("resume: %#v %d %v", point, size, err)
	}
	second := teamsMessagePage{Messages: []map[string]any{{"id": "old", "content": "olderword"}, {"id": "removed", "content": "shouldnotreturn"}}, Metadata: map[string]any{"syncState": "sync-2", "backwardLink": syntheticBackwardLink("next-2")}}
	scan, err = followTeamsHistoryBackward(point, "48:notes", size, 1, func(got teamsHistoryPoint) (teamsMessagePage, error) {
		if got != point {
			t.Fatal("resume did not use the stored point")
		}
		return second, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	records, syncState, err = teamsMirrorRecords(teamsMessagePage{Messages: scan.Messages, Metadata: scan.Metadata[0]})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err = teamsMirrorNextCheckpoint(scan.Metadata[0], "48:notes", size)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CommitTeamsMessageBatch(ctx, "48:notes", syncState, checkpoint, records, at.Add(time.Minute)); err != nil {
		t.Fatalf("second commit: %v", err)
	}
	for _, term := range []string{"firstword", "olderword"} {
		hits, fresh, _, err := db.SearchTeamsMessages(ctx, term, "48:notes", 10)
		if err != nil || len(hits) != 1 || !fresh.Partial || fresh.OldestPageAt != at.Format(time.RFC3339Nano) {
			t.Fatalf("search %s: %d %#v %v", term, len(hits), fresh, err)
		}
	}
	if hits, _, _, err := db.SearchTeamsMessages(ctx, "shouldnotreturn", "48:notes", 10); err != nil || len(hits) != 0 {
		t.Fatalf("resurrected tombstone: %d %v", len(hits), err)
	}
	calls := 0
	_, err = followTeamsHistoryBackward(teamsHistoryPoint{}, "48:notes", 2, 2, func(teamsHistoryPoint) (teamsMessagePage, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return teamsMessagePage{}, errors.New("synthetic cursor failure")
	})
	if err == nil || calls != 2 {
		t.Fatal("later failure was treated as a completed batch")
	}
	after, _, err := db.TeamsMirrorBackfillCheckpoint(ctx, "48:notes")
	if err != nil || after != checkpoint {
		t.Fatal("failed fetch advanced the stored checkpoint")
	}
}

func TestTeamsMirrorOfflineSearchAndAccountIsolation(t *testing.T) {
	home, sessionDir := t.TempDir(), t.TempDir()
	t.Setenv("TEAMS_WEB_HOME", home)
	t.Setenv("TEAMS_WEB_SESSION_DIR", sessionDir)
	writeTeamsFixture(t, sessionDir, time.Now().Add(-time.Hour))
	path, err := teamsMirrorPath()
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	record := store.TeamsMessageRecord{ID: "1", Data: json.RawMessage(`{"id":"1","content":"quiet synthetic fox"}`)}
	if err := db.CommitTeamsMessagePage(context.Background(), "chat-a", "state", []store.TeamsMessageRecord{record}, time.Now()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	run := func(args ...string) map[string]any {
		t.Helper()
		root := newRootCmd(&rootFlags{})
		var b bytes.Buffer
		root.SetOut(&b)
		root.SetErr(&b)
		root.SetArgs(append(args, "--json", "--no-learn"))
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v %s", args, err, b.String())
		}
		var out map[string]any
		if err := json.Unmarshal(b.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := run("search", "messages", "--local", "--query", "synthetic fox")
	if out["count"] != float64(1) || out["partial"] != true || out["source"] != "local_mirror" {
		t.Fatalf("offline local result: %#v", out)
	}
	file := filepath.Join(sessionDir, "session-teams-abc-1234abcd.json")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var session teamsSessionExport
	if err := json.Unmarshal(b, &session); err != nil {
		t.Fatal(err)
	}
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "tid": "tenant-test", "oid": "other-user"})
	session.Headers["authorization"] = "Bearer aa." + base64.RawURLEncoding.EncodeToString(claims) + ".bb"
	b, _ = json.Marshal(session)
	if err := os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	out = run("search", "messages", "--local", "--query", "synthetic")
	if out["count"] != float64(0) {
		t.Fatal("local results leaked across accounts")
	}
}
