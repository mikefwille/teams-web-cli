// Teams message pages and their checkpoints are committed together.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const teamsMessageResource = "teams_message_mirror"

type TeamsMessageRecord struct {
	ID              string
	Data            json.RawMessage
	Deleted         bool
	RefreshExisting bool // The record came from a current head page, not a backward page.
}

type TeamsMirrorFreshness struct {
	Conversations int    `json:"conversations"`
	OldestPageAt  string `json:"oldest_page_at,omitempty"`
	NewestPageAt  string `json:"newest_page_at,omitempty"`
	Partial       bool   `json:"partial"`
}

func teamsMirrorHash(value string) string {
	s := sha256.Sum256([]byte(value))
	return hex.EncodeToString(s[:])
}

func teamsMirrorCheckpoint(conversation string) string {
	return "teams_message_page:" + teamsMirrorHash(conversation)
}

func teamsMirrorBackfillCheckpoint(conversation string) string {
	return "teams_message_backfill:" + teamsMirrorHash(conversation)
}

func storedTeamsMessageDeleted(raw string) (bool, error) {
	var envelope struct {
		Message json.RawMessage `json:"message"`
		Deleted bool            `json:"deleted"`
	}
	if json.Unmarshal([]byte(raw), &envelope) != nil || len(envelope.Message) == 0 {
		return false, errors.New("invalid stored Teams message")
	}
	if envelope.Deleted {
		return true, nil
	}
	var message map[string]any
	if json.Unmarshal(envelope.Message, &message) != nil || message == nil {
		return false, errors.New("invalid stored Teams message")
	}
	properties, ok := message["properties"].(map[string]any)
	if message["properties"] != nil && !ok {
		return false, errors.New("invalid stored Teams message properties")
	}
	for key, value := range properties {
		if !strings.EqualFold(key, "deletetime") {
			continue
		}
		switch v := value.(type) {
		case nil:
			return false, nil
		case string:
			return v != "" && v != "0", nil
		case float64:
			return v != 0, nil
		default:
			return true, nil
		}
	}
	return false, nil
}

// CommitTeamsMessagePage atomically applies one observed page and its opaque
// state. It deliberately records the walk as incomplete: a syncState response
// is not, by itself, proof that all history has been traversed.
func (s *Store) CommitTeamsMessagePage(ctx context.Context, conversation, cursor string, records []TeamsMessageRecord, now time.Time) error {
	return s.commitTeamsMessageRecords(ctx, conversation, cursor, nil, records, now, false)
}

// CommitTeamsMessageBatch atomically stores a bounded history batch, its
// observed page state and a private backward-link checkpoint. Existing live
// records are refreshed only by records explicitly marked as head observations;
// historical overlaps leave them unchanged. Tombstones take precedence until
// an explicit restore protocol is observed.
func (s *Store) CommitTeamsMessageBatch(ctx context.Context, conversation, cursor, backfill string, records []TeamsMessageRecord, now time.Time) error {
	return s.commitTeamsMessageRecords(ctx, conversation, cursor, &backfill, records, now, true)
}

func (s *Store) commitTeamsMessageRecords(ctx context.Context, conversation, cursor string, backfill *string, records []TeamsMessageRecord, now time.Time, preserveTombstones bool) error {
	if conversation == "" || len(cursor) > 65536 || now.IsZero() {
		return errors.New("invalid Teams mirror checkpoint")
	}
	if backfill != nil && len(*backfill) > 65536 {
		return errors.New("invalid Teams backfill checkpoint")
	}
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		if record.ID == "" || seen[record.ID] || !json.Valid(record.Data) {
			return errors.New("invalid or duplicate Teams message in page")
		}
		seen[record.ID] = true
	}
	s.lockForWrite()
	defer s.unlockAfterWrite()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	for _, record := range records {
		storageID := teamsMirrorHash(conversation + "\x00" + record.ID)
		if preserveTombstones && !record.Deleted {
			var previous string
			err := tx.QueryRowContext(ctx, `SELECT data FROM resources WHERE id=? AND resource_type=?`, storageID, teamsMessageResource).Scan(&previous)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				deleted, err := storedTeamsMessageDeleted(previous)
				if err != nil {
					return err
				}
				if deleted || !record.RefreshExisting {
					continue
				}
			}
		}
		data, err := json.Marshal(struct {
			Conversation string          `json:"conversation"`
			Message      json.RawMessage `json:"message"`
			Deleted      bool            `json:"deleted,omitempty"`
		}{conversation, record.Data, record.Deleted})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO resources (id, resource_type, data, synced_at, updated_at)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT(resource_type, id) DO UPDATE SET
			data=excluded.data, synced_at=excluded.synced_at, updated_at=excluded.updated_at`,
			storageID, teamsMessageResource, string(data), stamp, stamp); err != nil {
			return err
		}
		rowID := ftsRowID(teamsMessageResource, storageID)
		if _, err := tx.ExecContext(ctx, `DELETE FROM resources_fts WHERE rowid = ?`, rowID); err != nil {
			return err
		}
		// A tombstone supersedes the previously indexed text. Keep its raw
		// service record for reconciliation, but do not index deleted content.
		if !record.Deleted {
			if _, err := tx.ExecContext(ctx, `INSERT INTO resources_fts(rowid,id,resource_type,content) VALUES (?,?,?,?)`,
				rowID, storageID, teamsMessageResource, searchableResourceContent(record.Data)); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sync_state(resource_type,last_cursor,last_synced_at,total_count,last_attempt_complete,scope_key)
		VALUES (?,?,?,?,0,?) ON CONFLICT(resource_type) DO UPDATE SET last_cursor=excluded.last_cursor,
		last_synced_at=excluded.last_synced_at,total_count=excluded.total_count,last_attempt_complete=0,scope_key=excluded.scope_key`,
		teamsMirrorCheckpoint(conversation), cursor, stamp, len(records), teamsMirrorHash(conversation)); err != nil {
		return err
	}
	if backfill != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sync_state(resource_type,last_cursor,last_synced_at,total_count,last_attempt_complete,scope_key)
			VALUES (?,?,?,?,0,?) ON CONFLICT(resource_type) DO UPDATE SET last_cursor=excluded.last_cursor,
			last_synced_at=excluded.last_synced_at,total_count=excluded.total_count,last_attempt_complete=0,scope_key=excluded.scope_key`,
			teamsMirrorBackfillCheckpoint(conversation), *backfill, stamp, len(records), teamsMirrorHash(conversation)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SearchTeamsMessages searches only collected, nondeleted message records.
func (s *Store) SearchTeamsMessages(ctx context.Context, query, conversation string, limit int) ([]json.RawMessage, TeamsMirrorFreshness, bool, error) {
	fresh := TeamsMirrorFreshness{Partial: true}
	if limit < 1 || limit > 100 {
		return nil, fresh, false, errors.New("local search limit must be between 1 and 100")
	}
	tokens := ftsQueryTokenRE.FindAllString(query, -1)
	if len(tokens) == 0 {
		return nil, fresh, false, errors.New("local search needs at least one word")
	}
	checkpointQuery := `SELECT COUNT(*), COALESCE(MIN(last_synced_at),''), COALESCE(MAX(last_synced_at),'') FROM sync_state WHERE substr(resource_type,1,19)=?`
	checkpoint := "teams_message_page:"
	if conversation != "" {
		checkpointQuery = `SELECT COUNT(*), COALESCE(MIN(last_synced_at),''), COALESCE(MAX(last_synced_at),'') FROM sync_state WHERE resource_type=?`
		checkpoint = teamsMirrorCheckpoint(conversation)
	}
	if err := s.db.QueryRowContext(ctx, checkpointQuery, checkpoint).Scan(&fresh.Conversations, &fresh.OldestPageAt, &fresh.NewestPageAt); err != nil {
		return nil, fresh, false, err
	}
	// Backfilling old pages must not make retained, older observations look
	// newly fetched just because the conversation checkpoint advanced.
	freshnessQuery := `SELECT synced_at FROM resources WHERE resource_type=?`
	freshnessArgs := []any{teamsMessageResource}
	if conversation != "" {
		freshnessQuery += ` AND json_extract(data,'$.conversation') = ?`
		freshnessArgs = append(freshnessArgs, conversation)
	}
	// RFC3339Nano omits trailing fractional zeros, so raw string MIN/MAX can
	// reverse two observations within a second. Stored times are always UTC.
	const fractional = `CASE WHEN substr(synced_at,20,1)='.' THEN substr(substr(synced_at,21,length(synced_at)-21)||'000000000',1,9) ELSE '000000000' END`
	var oldest, newest string
	err := s.db.QueryRowContext(ctx, freshnessQuery+` ORDER BY substr(synced_at,1,19) ASC, `+fractional+` ASC LIMIT 1`, freshnessArgs...).Scan(&oldest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fresh, false, err
	}
	if err == nil {
		if err := s.db.QueryRowContext(ctx, freshnessQuery+` ORDER BY substr(synced_at,1,19) DESC, `+fractional+` DESC LIMIT 1`, freshnessArgs...).Scan(&newest); err != nil {
			return nil, fresh, false, err
		}
		fresh.OldestPageAt, fresh.NewestPageAt = oldest, newest
	}
	q := `SELECT r.data FROM resources r JOIN resources_fts f ON r.id=f.id AND r.resource_type=f.resource_type WHERE r.resource_type=? AND `
	args := []any{teamsMessageResource}
	if ftsNeedsLikeFallback(tokens) {
		clause, values := ftsLikeClause("f", tokens)
		q += clause
		args = append(args, values...)
	} else {
		q += `resources_fts MATCH ?`
		args = append(args, FTSMatchQuery(query))
	}
	if conversation != "" {
		q += ` AND json_extract(r.data,'$.conversation') = ?`
		args = append(args, conversation)
	}
	q += ` ORDER BY r.synced_at DESC, r.id LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fresh, false, fmt.Errorf("searching Teams mirror: %w", err)
	}
	defer rows.Close()
	results, err := scanSearchData(rows)
	if err != nil {
		return nil, fresh, false, err
	}
	truncated := len(results) > limit
	if truncated {
		results = results[:limit]
	}
	if results == nil {
		results = []json.RawMessage{}
	}
	return results, fresh, truncated, nil
}

func (s *Store) TeamsMirrorCheckpoint(ctx context.Context, conversation string) (string, string, error) {
	var cursor, at string
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(last_cursor,''),COALESCE(last_synced_at,'') FROM sync_state WHERE resource_type=?`, teamsMirrorCheckpoint(strings.TrimSpace(conversation))).Scan(&cursor, &at)
	return cursor, at, err
}

func (s *Store) TeamsMirrorBackfillCheckpoint(ctx context.Context, conversation string) (string, string, error) {
	var cursor, at string
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(last_cursor,''),COALESCE(last_synced_at,'') FROM sync_state WHERE resource_type=?`, teamsMirrorBackfillCheckpoint(strings.TrimSpace(conversation))).Scan(&cursor, &at)
	return cursor, at, err
}
