package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestTeamsMirrorAtomicPageAndSearch(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	record := func(id, body string, deleted bool) TeamsMessageRecord {
		b, _ := json.Marshal(map[string]any{"id": id, "content": body})
		return TeamsMessageRecord{ID: id, Data: b, Deleted: deleted}
	}
	if err := db.CommitTeamsMessagePage(ctx, "chat-a", "state-a", []TeamsMessageRecord{record("1", "red fox", false), record("2", "red bird", false)}, at); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitTeamsMessagePage(ctx, "chat-b", "state-b", []TeamsMessageRecord{record("1", "red fox", false)}, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	hits, fresh, truncated, err := db.SearchTeamsMessages(ctx, "red fox", "chat-a", 10)
	if err != nil || len(hits) != 1 || truncated || fresh.Conversations != 1 || !fresh.Partial {
		t.Fatalf("scoped search: %d %#v %v %v", len(hits), fresh, truncated, err)
	}
	hits, fresh, truncated, err = db.SearchTeamsMessages(ctx, "red", "", 1)
	if err != nil || len(hits) != 1 || !truncated || fresh.Conversations != 2 || fresh.OldestPageAt != at.Format(time.RFC3339Nano) {
		t.Fatalf("global search: %d %#v %v %v", len(hits), fresh, truncated, err)
	}
	hits, _, _, err = db.SearchTeamsMessages(ctx, "fo", "chat-a", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("short search: %d %v", len(hits), err)
	}
	if err := db.CommitTeamsMessagePage(ctx, "chat-a", "state-c", []TeamsMessageRecord{record("1", "red fox", true)}, at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	hits, _, _, err = db.SearchTeamsMessages(ctx, "fox", "chat-a", 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("tombstone retained in FTS: %d %v", len(hits), err)
	}
	cursor, _, err := db.TeamsMirrorCheckpoint(ctx, "chat-a")
	if err != nil || cursor != "state-c" {
		t.Fatalf("checkpoint %q: %v", cursor, err)
	}

	// Force an indexing failure after the resource write and prove the entire
	// page, including its checkpoint, rolls back.
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_teams_mirror BEFORE INSERT ON sync_state WHEN NEW.last_cursor='bad-state' BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitTeamsMessagePage(ctx, "chat-c", "bad-state", []TeamsMessageRecord{record("4", "rollbackword", false)}, at); err == nil {
		t.Fatal("expected transaction failure")
	}
	hits, _, _, err = db.SearchTeamsMessages(ctx, "rollbackword", "", 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("partial transaction visible: %d %v", len(hits), err)
	}
	if _, _, err := db.TeamsMirrorCheckpoint(ctx, "chat-c"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("checkpoint survived rollback: %v", err)
	}
	if err := db.CommitTeamsMessagePage(ctx, "chat-a", "new", []TeamsMessageRecord{record("1", "x", false), record("1", "y", false)}, at); err == nil {
		t.Fatal("duplicate message accepted")
	}
	cursor, _, err = db.TeamsMirrorCheckpoint(ctx, "chat-a")
	if err != nil || cursor != "state-c" {
		t.Fatalf("invalid page changed checkpoint %q: %v", cursor, err)
	}
}

func TestTeamsMirrorBackfillPreservesTombstonesAndRollsBackCheckpoint(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	record := func(id, text string, deleted bool) TeamsMessageRecord {
		data, _ := json.Marshal(map[string]any{"id": id, "content": text})
		return TeamsMessageRecord{ID: id, Data: data, Deleted: deleted}
	}
	if err := db.CommitTeamsMessageBatch(ctx, "chat-a", "sync-1", "next-1", []TeamsMessageRecord{record("1", "deletedword", true), record("2", "firstword", false)}, at); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitTeamsMessageBatch(ctx, "chat-a", "sync-2", "next-2", []TeamsMessageRecord{record("1", "deletedword", false), record("3", "olderword", false)}, at.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	hits, fresh, _, err := db.SearchTeamsMessages(ctx, "deletedword", "chat-a", 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("older backfill resurrected a tombstone: %d %v", len(hits), err)
	}
	if fresh.OldestPageAt != at.Format(time.RFC3339Nano) || fresh.NewestPageAt != at.Add(100*time.Millisecond).Format(time.RFC3339Nano) || !fresh.Partial {
		t.Fatalf("freshness hid older retained observations: %#v", fresh)
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_teams_backfill BEFORE UPDATE ON sync_state WHEN NEW.last_cursor='bad-backfill' BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitTeamsMessageBatch(ctx, "chat-a", "sync-3", "bad-backfill", []TeamsMessageRecord{record("4", "rollbackword", false)}, at.Add(2*time.Minute)); err == nil {
		t.Fatal("expected backfill checkpoint failure")
	}
	hits, _, _, err = db.SearchTeamsMessages(ctx, "rollbackword", "chat-a", 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("failed batch left message data: %d %v", len(hits), err)
	}
	syncCursor, _, err := db.TeamsMirrorCheckpoint(ctx, "chat-a")
	if err != nil || syncCursor != "sync-2" {
		t.Fatalf("incremental checkpoint changed on rollback: %q %v", syncCursor, err)
	}
	backfill, _, err := db.TeamsMirrorBackfillCheckpoint(ctx, "chat-a")
	if err != nil || backfill != "next-2" {
		t.Fatalf("backfill checkpoint changed on rollback: %q %v", backfill, err)
	}
}

func TestTeamsMirrorHistoricalOverlapKeepsExistingContentAndObservation(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	record := func(text string) TeamsMessageRecord {
		data, _ := json.Marshal(map[string]any{"id": "1", "content": text})
		return TeamsMessageRecord{ID: "1", Data: data}
	}
	if err := db.CommitTeamsMessagePage(ctx, "chat-a", "head", []TeamsMessageRecord{record("newerword")}, at); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitTeamsMessageBatch(ctx, "chat-a", "history", "next", []TeamsMessageRecord{record("olderword")}, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	newer, fresh, _, err := db.SearchTeamsMessages(ctx, "newerword", "chat-a", 10)
	if err != nil || len(newer) != 1 || fresh.OldestPageAt != at.Format(time.RFC3339Nano) || fresh.NewestPageAt != at.Format(time.RFC3339Nano) {
		t.Fatalf("historical overlap changed existing content or observation: hits=%d freshness=%#v err=%v", len(newer), fresh, err)
	}
	older, _, _, err := db.SearchTeamsMessages(ctx, "olderword", "chat-a", 10)
	if err != nil || len(older) != 0 {
		t.Fatalf("historical overlap indexed stale text: hits=%d err=%v", len(older), err)
	}
	checkpoint, _, err := db.TeamsMirrorBackfillCheckpoint(ctx, "chat-a")
	if err != nil || checkpoint != "next" {
		t.Fatalf("backfill checkpoint did not advance: %q %v", checkpoint, err)
	}
}

func TestTeamsMirrorHeadRefreshAndHistoricalBatchAreAtomic(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	record := func(id, text string, deleted, refresh bool) TeamsMessageRecord {
		data, _ := json.Marshal(map[string]any{"id": id, "content": text})
		return TeamsMessageRecord{ID: id, Data: data, Deleted: deleted, RefreshExisting: refresh}
	}
	checkHits := func(word string, want int) {
		t.Helper()
		hits, _, _, err := db.SearchTeamsMessages(ctx, word, "chat-a", 10)
		if err != nil || len(hits) != want {
			t.Fatalf("search %s: hits=%d want=%d err=%v", word, len(hits), want, err)
		}
	}
	initial := []TeamsMessageRecord{record("1", "originalword", false, true), record("2", "deletedword", true, true), record("3", "removedword", false, true)}
	if err := db.CommitTeamsMessageBatch(ctx, "chat-a", "sync-1", "next-1", initial, at); err != nil {
		t.Fatal(err)
	}
	head := []TeamsMessageRecord{record("1", "editedword", false, true), record("2", "resurrectword", false, true)}
	if err := db.CommitTeamsMessageBatch(ctx, "chat-a", "sync-2", "next-2", head, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	checkHits("originalword", 0)
	checkHits("editedword", 1)
	checkHits("resurrectword", 0)
	var observation string
	if err := db.DB().QueryRowContext(ctx, `SELECT synced_at FROM resources WHERE resource_type=? AND id=?`, teamsMessageResource, teamsMirrorHash("chat-a\x001")).Scan(&observation); err != nil {
		t.Fatal(err)
	}
	if observation != at.Add(time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("head refresh did not update observation time: %q", observation)
	}
	history := []TeamsMessageRecord{record("1", "olderword", false, false), record("3", "removedword", true, false), record("4", "historyword", false, false)}
	if err := db.CommitTeamsMessageBatch(ctx, "chat-a", "sync-3", "next-3", history, at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	checkHits("editedword", 1)
	checkHits("olderword", 0)
	checkHits("removedword", 0)
	checkHits("historyword", 1)
	if err := db.DB().QueryRowContext(ctx, `SELECT synced_at FROM resources WHERE resource_type=? AND id=?`, teamsMessageResource, teamsMirrorHash("chat-a\x001")).Scan(&observation); err != nil {
		t.Fatal(err)
	}
	if observation != at.Add(time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("historical overlap changed the retained observation time: %q", observation)
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_head_batch BEFORE UPDATE ON sync_state WHEN NEW.last_cursor='bad-next' BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	failed := []TeamsMessageRecord{record("1", "uncommittedword", false, true), record("5", "newuncommitted", false, true)}
	if err := db.CommitTeamsMessageBatch(ctx, "chat-a", "bad-sync", "bad-next", failed, at.Add(3*time.Minute)); err == nil {
		t.Fatal("expected checkpoint failure")
	}
	checkHits("editedword", 1)
	checkHits("uncommittedword", 0)
	checkHits("newuncommitted", 0)
	if err := db.DB().QueryRowContext(ctx, `SELECT synced_at FROM resources WHERE resource_type=? AND id=?`, teamsMessageResource, teamsMirrorHash("chat-a\x001")).Scan(&observation); err != nil {
		t.Fatal(err)
	}
	if observation != at.Add(time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("failed batch changed observation time: %q", observation)
	}
	sync, _, err := db.TeamsMirrorCheckpoint(ctx, "chat-a")
	if err != nil || sync != "sync-3" {
		t.Fatalf("failed batch changed sync state: %q %v", sync, err)
	}
	backfill, _, err := db.TeamsMirrorBackfillCheckpoint(ctx, "chat-a")
	if err != nil || backfill != "next-3" {
		t.Fatalf("failed batch changed backfill checkpoint: %q %v", backfill, err)
	}
}
