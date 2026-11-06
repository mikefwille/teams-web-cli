// Calendar changes and their opaque server checkpoint commit in one transaction.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const teamsCalendarResource = "teams_calendar_mirror"

type TeamsCalendarRecord struct {
	ID   string
	Data json.RawMessage
}

func teamsCalendarCheckpoint(scope string) string {
	return "teams_calendar_page:" + teamsMirrorHash(scope)
}

func (s *Store) TeamsCalendarCheckpoint(ctx context.Context, scope string) (string, error) {
	var cursor string
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(last_cursor,'') FROM sync_state WHERE resource_type=?`, teamsCalendarCheckpoint(scope)).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return cursor, err
}

func (s *Store) CommitTeamsCalendarPage(ctx context.Context, scope, cursor string, updated []TeamsCalendarRecord, deleted []string, complete bool, now time.Time) error {
	if scope == "" || cursor == "" || len(cursor) > 65536 || now.IsZero() {
		return errors.New("invalid calendar checkpoint")
	}
	seen := map[string]bool{}
	for _, item := range updated {
		if item.ID == "" || seen[item.ID] || !json.Valid(item.Data) {
			return errors.New("invalid or duplicate calendar item")
		}
		seen[item.ID] = true
	}
	for _, id := range deleted {
		if id == "" || seen[id] {
			return errors.New("invalid or conflicting calendar deletion")
		}
		seen[id] = true
	}
	s.lockForWrite()
	defer s.unlockAfterWrite()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	for _, item := range updated {
		id := teamsMirrorHash(scope + "\x00" + item.ID)
		data, err := json.Marshal(struct {
			Scope string          `json:"scope"`
			Event json.RawMessage `json:"event"`
		}{teamsMirrorHash(scope), item.Data})
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO resources (id,resource_type,data,synced_at,updated_at) VALUES (?,?,?,?,?) ON CONFLICT(resource_type,id) DO UPDATE SET data=excluded.data,synced_at=excluded.synced_at,updated_at=excluded.updated_at`, id, teamsCalendarResource, string(data), stamp, stamp); err != nil {
			return err
		}
		rowID := ftsRowID(teamsCalendarResource, id)
		if _, err = tx.ExecContext(ctx, `DELETE FROM resources_fts WHERE rowid=?`, rowID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO resources_fts(rowid,id,resource_type,content) VALUES(?,?,?,?)`, rowID, id, teamsCalendarResource, searchableResourceContent(item.Data)); err != nil {
			return err
		}
	}
	for _, itemID := range deleted {
		id := teamsMirrorHash(scope + "\x00" + itemID)
		if _, err = tx.ExecContext(ctx, `DELETE FROM resources_fts WHERE rowid=?`, ftsRowID(teamsCalendarResource, id)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM resources WHERE resource_type=? AND id=?`, teamsCalendarResource, id); err != nil {
			return err
		}
	}
	completeInt := 0
	if complete {
		completeInt = 1
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sync_state(resource_type,last_cursor,last_synced_at,total_count,last_attempt_complete,scope_key) VALUES (?,?,?,?,?,?) ON CONFLICT(resource_type) DO UPDATE SET last_cursor=excluded.last_cursor,last_synced_at=excluded.last_synced_at,total_count=excluded.total_count,last_attempt_complete=excluded.last_attempt_complete,scope_key=excluded.scope_key`, teamsCalendarCheckpoint(scope), cursor, stamp, len(updated), completeInt, teamsMirrorHash(scope)); err != nil {
		return err
	}
	return tx.Commit()
}
