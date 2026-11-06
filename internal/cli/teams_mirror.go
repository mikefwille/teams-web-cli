// Bounded Teams message pages can be mirrored privately for offline search.
package cli

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
	"github.com/mikefwille/teams-web-cli/internal/store"
)

func teamsMirrorPath() (string, error) {
	disabled, err := teamsSessionDisabled()
	if err != nil {
		return "", err
	}
	if disabled {
		return "", errors.New("Teams web session is locally disabled")
	}
	dir, err := teamsSessionDir()
	if err != nil {
		return "", err
	}
	session, err := loadTeamsWebSessionForLocalAccount(dir)
	if err != nil {
		return "", err
	}
	principal, ok := teamsPrincipal(session.Headers["authorization"])
	if !ok {
		return "", errors.New("Teams account identity is unavailable")
	}
	return teamsMirrorPathForPrincipal(principal)
}

func teamsMirrorPathForPrincipal(principal teamsTokenPrincipal) (string, error) {
	if principal.Tenant == "" || principal.Object == "" {
		return "", errors.New("Teams account identity is unavailable")
	}
	dataDir, err := cliutil.DataDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(principal.Tenant + "\x00" + principal.Object))
	return filepath.Join(dataDir, "data-"+hex.EncodeToString(sum[:])+".db"), nil
}

func teamsMirrorRecords(page teamsMessagePage) ([]store.TeamsMessageRecord, string, error) {
	state, ok := page.Metadata["syncState"].(string)
	if !ok || state == "" || len(state) > 65536 {
		return nil, "", errors.New("Teams message page has no usable sync state")
	}
	records := make([]store.TeamsMessageRecord, 0, len(page.Messages))
	for _, message := range page.Messages {
		id, ok := message["id"].(string)
		if !ok || !validTeamsMessagePathID(id) {
			return nil, "", errors.New("Teams message page contains a message without a usable ID")
		}
		data, err := json.Marshal(message)
		if err != nil {
			return nil, "", errors.New("Teams message page contains invalid data")
		}
		records = append(records, store.TeamsMessageRecord{ID: id, Data: data, Deleted: teamsMessageDeleted(message)})
	}
	return records, state, nil
}

// Only an unselected first page is a head refresh. An explicit selector may
// name historical data even when its value is empty or zero.
func teamsMirrorHeadRefresh(cmd *cobra.Command, resume bool, cursor string, start int64) bool {
	return !resume && cursor == "" && start == 0 && !cmd.Flags().Changed("sync-state") && !cmd.Flags().Changed("start-time")
}

func teamsMirrorMarkHeadRecords(records []store.TeamsMessageRecord, scan teamsHistoryScan, refreshHead bool) {
	if !refreshHead {
		return
	}
	for i := range records {
		records[i].RefreshExisting = scan.FirstPageIDs[records[i].ID]
	}
}

type teamsMirrorBackfill struct {
	Version  int    `json:"version"`
	PageSize int    `json:"page_size"`
	Link     string `json:"backward_link"`
}

func teamsMirrorNextCheckpoint(metadata map[string]any, conversation string, pageSize int) (string, error) {
	raw, exists := metadata["backwardLink"]
	if !exists || raw == nil || raw == "" {
		return "", nil
	}
	link, ok := raw.(string)
	if !ok {
		return "", errors.New("invalid Teams backfill continuation")
	}
	if _, err := teamsBackwardPoint(link, conversation, pageSize); err != nil {
		return "", err
	}
	data, err := json.Marshal(teamsMirrorBackfill{Version: 1, PageSize: pageSize, Link: link})
	if err != nil || len(data) > 65536 {
		return "", errors.New("Teams backfill continuation exceeds the checkpoint limit")
	}
	return string(data), nil
}

func teamsMirrorResumePoint(raw, conversation string, requestedSize int, sizeChanged bool) (teamsHistoryPoint, int, error) {
	var checkpoint teamsMirrorBackfill
	if raw == "" || len(raw) > 65536 || json.Unmarshal([]byte(raw), &checkpoint) != nil || checkpoint.Version != 1 || checkpoint.PageSize < 1 || checkpoint.PageSize > 200 || checkpoint.Link == "" {
		return teamsHistoryPoint{}, 0, errors.New("no usable backward checkpoint; start a new bounded message sync")
	}
	if sizeChanged && requestedSize != checkpoint.PageSize {
		return teamsHistoryPoint{}, 0, usageErr(errors.New("--page-size must match the stored backward checkpoint"))
	}
	point, err := teamsBackwardPoint(checkpoint.Link, conversation, checkpoint.PageSize)
	if err != nil {
		return teamsHistoryPoint{}, 0, err
	}
	return point, checkpoint.PageSize, nil
}

func newTeamsSyncMessagesCmd(flags *rootFlags) *cobra.Command {
	var conversation, cursor string
	var start int64
	var pageSize, maxPages int
	var resume bool
	cmd := &cobra.Command{Use: "messages", Short: "Mirror bounded Teams message history locally", Args: cobra.NoArgs,
		Annotations: map[string]string{"mcp:local-write": "true", "pp:data-source": "live"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !validTeamsMessagePathID(conversation) {
				return usageErr(errors.New("--conversation must be a valid opaque conversation ID"))
			}
			if len(cursor) > 65536 {
				return usageErr(errors.New("sync state is too long"))
			}
			if _, err := teamsHistoryParams(cursor, start, pageSize); err != nil {
				return err
			}
			if maxPages < 1 || maxPages > 10 {
				return usageErr(errors.New("--max-pages must be 1–10"))
			}
			if resume && (cmd.Flags().Changed("sync-state") || cmd.Flags().Changed("start-time")) {
				return usageErr(errors.New("--resume cannot be combined with --sync-state or --start-time"))
			}
			if dryRunOK(flags) {
				return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "sync messages", "conversation": conversation, "max_pages": maxPages, "resume": resume})
			}
			if cliutil.IsAnyHarness() {
				return writeHarnessRefusal(cmd.OutOrStdout(), flags, "sync messages")
			}
			if localDataDisabled() {
				return errors.New("sync is disabled by TEAMS_WEB_NO_LOCAL_DATA")
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			if c.Config.AuthSource != "web-session" {
				return authErr(errors.New("a Teams web session is required for the mirror"))
			}
			principal, ok := teamsPrincipal(c.Config.AuthHeader())
			if !ok {
				return authErr(errors.New("Teams account identity is unavailable"))
			}
			path, err := teamsMirrorPathForPrincipal(principal)
			if err != nil {
				return authErr(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return err
			}
			var result map[string]any
			locked, err := cliutil.TryWithFileLock(path+".sync", func() error {
				ctx, cancel := boundCtx(cmd.Context(), flags)
				defer cancel()
				db, err := store.OpenWithContext(ctx, path)
				if err != nil {
					return err
				}
				defer db.Close()
				point := teamsHistoryPoint{StartTime: start, SyncState: cursor}
				effectiveSize := pageSize
				if resume {
					raw, _, err := db.TeamsMirrorBackfillCheckpoint(ctx, conversation)
					if errors.Is(err, sql.ErrNoRows) {
						return errors.New("no backward checkpoint for this conversation; start a new bounded message sync")
					}
					if err != nil {
						return err
					}
					point, effectiveSize, err = teamsMirrorResumePoint(raw, conversation, pageSize, cmd.Flags().Changed("page-size"))
					if err != nil {
						return err
					}
				}
				bounded := &cobra.Command{}
				bounded.SetContext(ctx)
				scan, err := fetchTeamsHistoryScanWithClient(bounded, flags, c, conversation, []teamsHistoryPoint{point}, effectiveSize, maxPages)
				if err != nil {
					return err
				}
				lastMetadata := scan.Metadata[len(scan.Metadata)-1]
				records, state, err := teamsMirrorRecords(teamsMessagePage{Messages: scan.Messages, Metadata: lastMetadata})
				if err != nil {
					return err
				}
				teamsMirrorMarkHeadRecords(records, scan, teamsMirrorHeadRefresh(cmd, resume, cursor, start))
				backfill, err := teamsMirrorNextCheckpoint(lastMetadata, conversation, effectiveSize)
				if err != nil {
					return err
				}
				at := time.Now().UTC()
				if err := db.CommitTeamsMessageBatch(ctx, conversation, state, backfill, records, at); err != nil {
					return err
				}
				result = map[string]any{"source": "live_pages", "storage": "local", "count": len(records), "pages_scanned": scan.PagesScanned, "stop_reason": scan.StopReason, "overlap_count": scan.OverlapCount, "conflicting_copies": scan.ConflictingCopies, "page_saved_at": at.Format(time.RFC3339Nano), "state_stored": true, "continuation_available": backfill != "", "resumed": resume, "partial": true}
				return nil
			})
			if err != nil {
				return err
			}
			if !locked {
				return errors.New("another Teams mirror operation is already running for this account")
			}
			return flags.printJSON(cmd, result)
		}}
	cmd.Flags().StringVar(&conversation, "conversation", "", "Opaque conversation or channel ID")
	cmd.Flags().IntVar(&pageSize, "page-size", 200, "Maximum messages in this page")
	cmd.Flags().IntVar(&maxPages, "max-pages", 1, "Maximum backward pages to fetch in this batch (1–10)")
	cmd.Flags().BoolVar(&resume, "resume", false, "Continue from the stored backward checkpoint for this conversation")
	cmd.Flags().Int64Var(&start, "start-time", 0, "Optional page start as Unix milliseconds")
	cmd.Flags().StringVar(&cursor, "sync-state", "", "Opaque sync state from a previous page")
	return cmd
}

func searchTeamsMirror(cmd *cobra.Command, flags *rootFlags, query, conversation string, limit int) error {
	if strings.TrimSpace(query) == "" {
		return usageErr(errors.New("--query is required"))
	}
	if conversation != "" && !validTeamsMessagePathID(conversation) {
		return usageErr(errors.New("invalid conversation ID"))
	}
	if limit < 1 || limit > 100 {
		return usageErr(errors.New("--limit must be between 1 and 100 for local search"))
	}
	if dryRunOK(flags) {
		return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "search local messages", "query": query})
	}
	path, err := teamsMirrorPath()
	if err != nil {
		return authErr(err)
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return flags.printJSON(cmd, map[string]any{"results": []any{}, "count": 0, "source": "local_mirror", "partial": true, "note": "no pages mirrored for this account"})
		}
		return err
	}
	ctx, cancel := boundCtx(cmd.Context(), flags)
	defer cancel()
	db, err := store.OpenReadOnlyContext(ctx, path)
	if err != nil {
		return err
	}
	defer db.Close()
	hits, fresh, truncated, err := db.SearchTeamsMessages(ctx, query, conversation, limit)
	if err != nil {
		return err
	}
	stale := false
	if fresh.OldestPageAt != "" && flags.maxAge > 0 {
		at, parseErr := time.Parse(time.RFC3339Nano, fresh.OldestPageAt)
		if parseErr != nil {
			return fmt.Errorf("invalid mirror freshness timestamp")
		}
		stale = time.Since(at) > flags.maxAge
	}
	return flags.printJSON(cmd, map[string]any{"results": hits, "count": len(hits), "source": "local_mirror", "freshness": fresh, "stale": stale, "partial": true, "truncated": truncated})
}

func newTeamsSyncCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "sync", Short: "Save observed Teams data locally", Annotations: map[string]string{"pp:parent-group": "true"}, RunE: parentNoSubcommandRunE(flags)}
	cmd.AddCommand(newTeamsSyncMessagesCmd(flags))
	return cmd
}
func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) { addTeamsCommandGroup(root, newTeamsSyncCmd(flags)) })
}
