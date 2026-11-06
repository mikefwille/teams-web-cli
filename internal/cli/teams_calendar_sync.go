// Durable, bounded synchronization of observed Teams calendar changes.
package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
	"github.com/mikefwille/teams-web-cli/internal/store"
)

var teamsCalendarDiagnosticName = regexp.MustCompile(`^teams-calendar-diag-[a-f0-9]{16}\.json$`)

// Opt-in diagnostics contain only the response body, never request headers.
func saveTeamsCalendarDiagnostic(data []byte) error {
	name := os.Getenv("TEAMS_WEB_CALENDAR_DIAGNOSTIC")
	if name == "" {
		return nil
	}
	dir, err := teamsSessionDir()
	if err != nil || !filepath.IsAbs(name) || filepath.Dir(name) != filepath.Clean(dir) || !teamsCalendarDiagnosticName.MatchString(filepath.Base(name)) {
		return errors.New("invalid private calendar diagnostic path")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("unsafe private calendar diagnostic directory")
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("could not create calendar diagnostic")
	}
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return errors.New("could not write calendar diagnostic")
	}
	return f.Sync()
}

func calendarScopeID(scope teamsAudienceScope, override string) (string, error) {
	if override != "" {
		if len(override) > 4096 || strings.ContainsAny(override, "\r\n\x00") {
			return "", usageErr(errors.New("invalid calendar ID"))
		}
		return override, nil
	}
	raw := scope.RequestBody
	if encoded := scope.Headers["x-owa-urlpostdata"]; encoded != "" {
		value, err := url.QueryUnescape(encoded)
		if err != nil {
			return "", errors.New("invalid captured calendar request")
		}
		raw = []byte(value)
	}
	var root struct {
		Body struct {
			CalendarID struct {
				BaseFolderID struct {
					ID string `json:"Id"`
				} `json:"BaseFolderId"`
			} `json:"CalendarId"`
		} `json:"Body"`
	}
	if json.Unmarshal(raw, &root) != nil || root.Body.CalendarID.BaseFolderID.ID == "" {
		return "", errors.New("captured calendar ID is unavailable")
	}
	return root.Body.CalendarID.BaseFolderID.ID, nil
}

func calendarObjectID(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case map[string]any:
		if id, ok := v["Id"].(string); ok && id != "" {
			return id
		}
		if item, ok := v["ItemId"].(map[string]any); ok {
			if id, ok := item["Id"].(string); ok {
				return id
			}
		}
	}
	return ""
}
func calendarPageRecords(result map[string]any) ([]store.TeamsCalendarRecord, []string, string, bool, error) {
	updated, ok1 := result["results"].([]any)
	deleted, ok2 := result["deleted_ids"].([]any)
	state, ok3 := result["sync_state"].(string)
	partial, ok4 := result["partial"].(bool)
	if !ok1 || !ok2 || !ok3 || !ok4 || state == "" || len(state) > 65536 {
		return nil, nil, "", false, errors.New("calendar changes page lacks a usable checkpoint")
	}
	records := make([]store.TeamsCalendarRecord, 0, len(updated))
	ids := make([]string, 0, len(deleted))
	for i, event := range updated {
		// SyncCalendarEvents wraps a changed item as {"Item": {...}}.
		// Return and store the actual item, preserving its service fields.
		if wrapper, ok := event.(map[string]any); ok {
			if item, ok := wrapper["Item"].(map[string]any); ok {
				event = item
				updated[i] = item
			}
		}
		id := calendarObjectID(event)
		if id == "" || len(id) > 8192 {
			return nil, nil, "", false, errors.New("calendar change lacks an item ID")
		}
		raw, err := json.Marshal(event)
		if err != nil {
			return nil, nil, "", false, errors.New("invalid calendar item")
		}
		records = append(records, store.TeamsCalendarRecord{ID: id, Data: raw})
	}
	for _, event := range deleted {
		id := calendarObjectID(event)
		if id == "" || len(id) > 8192 {
			return nil, nil, "", false, errors.New("calendar deletion lacks an item ID")
		}
		ids = append(ids, id)
	}
	return records, ids, state, !partial, nil
}

// pp:data-source live
func newTeamsCalendarSyncCmd(flags *rootFlags) *cobra.Command {
	var calendarID, start string
	var maxPages int
	cmd := &cobra.Command{Use: "sync", Short: "Synchronize calendar changes into a private checkpoint", Args: cobra.NoArgs, Annotations: map[string]string{"mcp:local-write": "true", "pp:data-source": "live"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if start == "" {
				return usageErr(errors.New("--start is required"))
			}
			if _, _, err := calendarLocalTime(start); err != nil {
				return err
			}
			if maxPages < 1 || maxPages > 10 {
				return usageErr(errors.New("--max-pages must be between 1 and 10"))
			}
			if dryRunOK(flags) {
				return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "calendar sync", "start": start, "max_pages": maxPages})
			}
			if cliutil.IsAnyHarness() {
				return writeHarnessRefusal(cmd.OutOrStdout(), flags, "calendar sync")
			}
			if localDataDisabled() {
				return errors.New("calendar sync is disabled by TEAMS_WEB_NO_LOCAL_DATA")
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			if c.Config.AuthSource != "web-session" {
				return authErr(errors.New("a Teams web session is required"))
			}
			principal, ok := teamsPrincipal(c.Config.AuthHeader())
			if !ok {
				return authErr(errors.New("Teams session identity is unavailable"))
			}
			dir, err := teamsSessionDir()
			if err != nil {
				return err
			}
			scope, err := loadTeamsCalendarScope(dir, "SyncCalendarEvents", principal)
			if err != nil {
				return authErr(err)
			}
			folder, err := calendarScopeID(scope, calendarID)
			if err != nil {
				return err
			}
			localStart, _, err := calendarLocalTime(start)
			if err != nil {
				return err
			}
			scopeKey := folder + "\x00" + localStart
			path, err := teamsMirrorPathForPrincipal(principal)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return err
			}
			guard, ok := c.HTTPClient.Transport.(teamsOriginTransport)
			if !ok {
				return errors.New("Teams session origin guard is unavailable")
			}
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			var output map[string]any
			locked, err := cliutil.TryWithFileLock(path+".sync", func() error {
				db, err := store.OpenWithContext(ctx, path)
				if err != nil {
					return err
				}
				defer db.Close()
				state, err := db.TeamsCalendarCheckpoint(ctx, scopeKey)
				if err != nil {
					return err
				}
				hadCheckpoint := state != ""
				allUpdated := []any{}
				allDeleted := []string{}
				complete := false
				pages := 0
				for pages < maxPages {
					headers, body, err := teamsCalendarRequestWithState(scope, "SyncCalendarEvents", calendarID, start, "", state)
					if err != nil {
						return err
					}
					hc := *c.HTTPClient
					hc.Transport = teamsCalendarTransport{next: guard.next, headers: headers, action: "SyncCalendarEvents"}
					hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
					data, err := runTeamsCalendarRead(ctx, &hc, "SyncCalendarEvents", scope.ObservedQuery, body)
					if err != nil {
						return err
					}
					if pages == 0 {
						if err := saveTeamsCalendarDiagnostic(data); err != nil {
							return err
						}
					}
					result, err := decodeTeamsCalendar(data, "SyncCalendarEvents")
					if err != nil {
						return err
					}
					records, deleted, next, done, err := calendarPageRecords(result)
					if err != nil {
						return err
					}
					if !done && next == state {
						return errors.New("calendar continuation made no progress")
					}
					if err := db.CommitTeamsCalendarPage(ctx, scopeKey, next, records, deleted, done, time.Now()); err != nil {
						return err
					}
					allUpdated = append(allUpdated, result["results"].([]any)...)
					allDeleted = append(allDeleted, deleted...)
					pages++
					state = next
					complete = done
					if done {
						break
					}
				}
				output = map[string]any{"source": "live_calendar", "storage": "local", "start": start, "pages": pages, "resumed": hadCheckpoint, "updated": allUpdated, "deleted_ids": allDeleted, "updated_count": len(allUpdated), "deleted_count": len(allDeleted), "partial": !complete, "checkpoint_saved": true}
				return nil
			})
			if err != nil {
				return err
			}
			if !locked {
				return errors.New("another Teams mirror operation is already running for this account")
			}
			return flags.printJSON(cmd, output)
		}}
	cmd.Flags().StringVar(&calendarID, "calendar-id", "", "Calendar ID; defaults to captured primary calendar")
	cmd.Flags().StringVar(&start, "start", "", "Start time in RFC3339 with an offset")
	cmd.Flags().IntVar(&maxPages, "max-pages", 5, "Maximum change pages per invocation (1–10)")
	return cmd
}
