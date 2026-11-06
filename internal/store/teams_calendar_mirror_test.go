package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestTeamsCalendarCheckpointCommitsChangesAtomically(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Now()
	scope := "folder\x00start"
	event := func(text string) TeamsCalendarRecord {
		return TeamsCalendarRecord{ID: "event-1", Data: json.RawMessage(`{"ItemId":{"Id":"event-1"},"Subject":"` + text + `"}`)}
	}
	if err := db.CommitTeamsCalendarPage(ctx, scope, "cursor-one", []TeamsCalendarRecord{event("before")}, nil, true, at); err != nil {
		t.Fatal(err)
	}
	id := teamsMirrorHash(scope + "\x00event-1")
	before, err := db.Get(teamsCalendarResource, id)
	if err != nil || !json.Valid(before) {
		t.Fatalf("missing event: %v", err)
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_cal_checkpoint BEFORE UPDATE ON sync_state WHEN NEW.last_cursor='bad' BEGIN SELECT RAISE(ABORT,'fail'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitTeamsCalendarPage(ctx, scope, "bad", []TeamsCalendarRecord{event("after")}, nil, true, at); err == nil {
		t.Fatal("failed checkpoint was accepted")
	}
	after, err := db.Get(teamsCalendarResource, id)
	if err != nil || string(after) != string(before) {
		t.Fatal("partial calendar transaction escaped")
	}
	cursor, err := db.TeamsCalendarCheckpoint(ctx, scope)
	if err != nil || cursor != "cursor-one" {
		t.Fatalf("checkpoint changed: %q %v", cursor, err)
	}
	if err := db.CommitTeamsCalendarPage(ctx, scope, "cursor-two", []TeamsCalendarRecord{event("after")}, nil, true, at); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitTeamsCalendarPage(ctx, scope, "cursor-three", nil, []string{"event-1"}, true, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Get(teamsCalendarResource, id); err == nil {
		t.Fatal("deleted event remains")
	}
	cursor, err = db.TeamsCalendarCheckpoint(ctx, scope)
	if err != nil || cursor != "cursor-three" {
		t.Fatal("deletion checkpoint missing")
	}
	other, err := db.TeamsCalendarCheckpoint(ctx, "other")
	if err != nil || other != "" {
		t.Fatal("calendar scopes are not isolated")
	}
}
