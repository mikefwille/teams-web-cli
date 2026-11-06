package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestBackfillDoesNotReplaceNewerEditedMessage(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "mirror.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	record := func(text string) TeamsMessageRecord {
		data, _ := json.Marshal(map[string]any{"id": "same-id", "content": text})
		return TeamsMessageRecord{ID: "same-id", Data: data}
	}
	now := time.Now().UTC()
	if err := db.CommitTeamsMessageBatch(ctx, "chat", "newer-state", "next-page", []TeamsMessageRecord{record("newereditword")}, now); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitTeamsMessageBatch(ctx, "chat", "older-state", "next-page-2", []TeamsMessageRecord{record("oldversionword")}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	hits, _, _, err := db.SearchTeamsMessages(ctx, "newereditword", "chat", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatal("an older overlapping batch replaced the previously observed edited content")
	}
}
