// Calendar reads use the observed Outlook-backed Teams calendar service.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
)

const teamsCalendarOrigin = "https://outlook.office.com"
const teamsCalendarPath = "/owa/service.svc"

var teamsCalendarNumber = regexp.MustCompile(`^[0-9]{1,8}$`)
var teamsCalendarLimiter = cliutil.NewAdaptiveLimiterAuto(1)

func loadTeamsCalendarScope(dir, action string, primary teamsTokenPrincipal) (teamsAudienceScope, error) {
	var catalog teamsAudienceCatalog
	if err := readPrivateSessionFile(filepath.Join(dir, "audiences-teams.json"), &catalog); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return teamsAudienceScope{}, errors.New("no captured Teams calendar audience; import the observed operation")
		}
		return teamsAudienceScope{}, err
	}
	if catalog.Version != 1 || catalog.Service != "teams" {
		return teamsAudienceScope{}, errors.New("invalid Teams audience catalog")
	}
	for _, entry := range catalog.Scopes {
		if entry.Origin != teamsCalendarOrigin || entry.Operation != action {
			continue
		}
		name := filepath.Base(entry.ExportPath)
		if !teamsScopeID.MatchString(entry.ScopeID) || entry.ExportPath != filepath.Join(dir, name) || !strings.HasPrefix(name, "authscope-teams-"+entry.ScopeID+"-") || !strings.HasSuffix(name, ".json") {
			return teamsAudienceScope{}, errors.New("invalid calendar audience path")
		}
		var scope teamsAudienceScope
		if err := readPrivateSessionFile(entry.ExportPath, &scope); err != nil {
			return teamsAudienceScope{}, err
		}
		principal, ok := teamsPrincipal(scope.Headers["authorization"])
		if scope.Version != 1 || scope.Service != "teams" || scope.ScopeID != entry.ScopeID || scope.Origin != teamsCalendarOrigin || scope.ObservedPath != teamsCalendarPath || scope.Operation != action || scope.ObservedQuery["action"] != action || scope.ObservedQuery["app"] != "Calendar" || !teamsCalendarNumber.MatchString(scope.ObservedQuery["n"]) || len(scope.ObservedQuery) != 3 || !strings.HasPrefix(strings.ToLower(scope.Headers["authorization"]), "bearer ") || !ok || principal.Tenant != primary.Tenant || principal.Object != primary.Object {
			return teamsAudienceScope{}, errors.New("calendar audience does not match the current Teams account")
		}
		if exp, known := accessTokenExpiry(scope.Headers["authorization"]); !known || time.Now().After(exp) {
			return teamsAudienceScope{}, errors.New("Teams calendar audience expired; capture and import a fresh observation")
		}
		return scope, nil
	}
	return teamsAudienceScope{}, fmt.Errorf("no captured %s calendar operation; import its observation", action)
}

func calendarLocalTime(raw string) (string, time.Time, error) {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return "", time.Time{}, usageErr(errors.New("calendar times must be RFC3339 timestamps with an offset"))
	}
	zone, err := time.LoadLocation("America/Denver")
	if err != nil {
		return "", time.Time{}, err
	}
	const layout = "2006-01-02T15:04:05.000"
	local := t.In(zone).Format(layout)
	if t.Add(time.Hour).In(zone).Format(layout) == local || t.Add(-time.Hour).In(zone).Format(layout) == local {
		return "", time.Time{}, usageErr(errors.New("calendar time is ambiguous at a daylight-saving transition"))
	}
	return local, t, nil
}

func teamsCalendarRequest(scope teamsAudienceScope, action, calendarID, start, end string) (map[string]string, []byte, error) {
	return teamsCalendarRequestWithState(scope, action, calendarID, start, end, "")
}

func teamsCalendarRequestWithState(scope teamsAudienceScope, action, calendarID, start, end, state string) (map[string]string, []byte, error) {
	headers := make(map[string]string, len(scope.Headers))
	for key, value := range scope.Headers {
		headers[key] = value
	}
	if calendarID != "" && (len(calendarID) > 4096 || strings.ContainsAny(calendarID, "\r\n\x00")) {
		return nil, nil, usageErr(errors.New("invalid calendar ID"))
	}
	if action == "GetCalendarFoldersV2" {
		return headers, nil, nil
	}
	var raw []byte
	if encoded := headers["x-owa-urlpostdata"]; encoded != "" {
		decoded, err := url.QueryUnescape(encoded)
		if err != nil {
			return nil, nil, errors.New("captured calendar request is invalid")
		}
		raw = []byte(decoded)
	} else {
		raw = scope.RequestBody
	}
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return nil, nil, errors.New("captured calendar request is invalid")
	}
	head, ok := root["Header"].(map[string]any)
	if !ok {
		return nil, nil, errors.New("captured calendar time zone is unavailable")
	}
	zone, ok := head["TimeZoneContext"].(map[string]any)
	if !ok {
		return nil, nil, errors.New("captured calendar time zone is unavailable")
	}
	definition, ok := zone["TimeZoneDefinition"].(map[string]any)
	if !ok || definition["Id"] != "Mountain Standard Time" {
		return nil, nil, errors.New("captured calendar time zone is unsupported")
	}
	body, ok := root["Body"].(map[string]any)
	if !ok {
		return nil, nil, errors.New("captured calendar request is invalid")
	}
	if calendarID != "" {
		calendar, ok := body["CalendarId"].(map[string]any)
		if !ok {
			return nil, nil, errors.New("captured calendar ID is unavailable")
		}
		folder, ok := calendar["BaseFolderId"].(map[string]any)
		if !ok {
			return nil, nil, errors.New("captured calendar ID is unavailable")
		}
		folder["Id"] = calendarID
	}
	from, startTime, err := calendarLocalTime(start)
	if err != nil {
		return nil, nil, err
	}
	if action == "GetCalendarView" {
		to, endTime, err := calendarLocalTime(end)
		if err != nil {
			return nil, nil, err
		}
		if !startTime.Before(endTime) || endTime.Sub(startTime) > 31*24*time.Hour {
			return nil, nil, usageErr(errors.New("calendar range must be positive and no longer than 31 days"))
		}
		body["RangeStart"], body["RangeEnd"] = from, to
	} else if action == "SyncCalendarEvents" {
		if end != "" {
			return nil, nil, usageErr(errors.New("calendar changes accepts only --start"))
		}
		body["StartDate"] = from
		if state != "" {
			if len(state) > 65536 {
				return nil, nil, usageErr(errors.New("calendar sync state is too long"))
			}
			body["SyncState"] = state
		}
	} else {
		return nil, nil, errors.New("unsupported calendar read")
	}
	updated, err := json.Marshal(root)
	if err != nil {
		return nil, nil, err
	}
	if headers["x-owa-urlpostdata"] != "" {
		headers["x-owa-urlpostdata"] = strings.ReplaceAll(url.QueryEscape(string(updated)), "+", "%20")
		return headers, nil, nil
	}
	return headers, updated, nil
}

type teamsCalendarTransport struct {
	next    http.RoundTripper
	headers map[string]string
	action  string
}

func (t teamsCalendarTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, "outlook.office.com") || req.URL.User != nil || req.URL.Path != teamsCalendarPath || req.Method != http.MethodPost {
		return nil, errors.New("refusing to send calendar credentials to an unobserved endpoint")
	}
	q := req.URL.Query()
	if len(q) != 3 || len(q["action"]) != 1 || q.Get("action") != t.action || len(q["app"]) != 1 || q.Get("app") != "Calendar" || len(q["n"]) != 1 || !teamsCalendarNumber.MatchString(q.Get("n")) {
		return nil, errors.New("refusing unobserved calendar operation")
	}
	omit := map[string]bool{"host": true, "connection": true, "content-length": true, "accept-encoding": true, "transfer-encoding": true, "proxy-authorization": true, "proxy-connection": true, "upgrade": true, "te": true, "trailer": true}
	for key, value := range t.headers {
		if !omit[strings.ToLower(key)] && !strings.HasPrefix(key, ":") && !strings.ContainsAny(value, "\r\n") {
			req.Header.Set(key, value)
		}
	}
	return t.next.RoundTrip(req)
}

func runTeamsCalendarRead(ctx context.Context, hc *http.Client, action string, query map[string]string, body []byte) ([]byte, error) {
	q := url.Values{}
	for key, value := range query {
		q.Set(key, value)
	}
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, teamsCalendarOrigin+teamsCalendarPath+"?"+q.Encode(), reader)
	if err != nil {
		return nil, err
	}
	if err := teamsCalendarLimiter.Wait(ctx); err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, errors.New("Teams calendar request failed")
	}
	defer resp.Body.Close()
	if rem, reset, ok := cliutil.ParseRateLimitHeaders(resp.Header); ok {
		teamsCalendarLimiter.ObserveHeaders(rem, reset)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2_000_001))
	if err != nil || len(data) > 2_000_000 {
		return nil, errors.New("Teams calendar response could not be read within the limit")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		teamsCalendarLimiter.OnRateLimit()
		return nil, rateLimitErr(&cliutil.RateLimitError{URL: "Teams calendar service", RetryAfter: cliutil.RetryAfter(resp)})
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifyAPIErrorOnly(fmt.Errorf("Teams calendar returned HTTP %d", resp.StatusCode))
	}
	teamsCalendarLimiter.OnSuccess()
	return data, nil
}

func decodeTeamsCalendar(data []byte, action string) (map[string]any, error) {
	var root map[string]any
	if json.Unmarshal(data, &root) != nil || root == nil {
		return nil, errors.New("Teams returned invalid calendar JSON")
	}
	if action == "GetCalendarFoldersV2" {
		folders, ok1 := root["CalendarFolders"].([]any)
		groups, ok2 := root["CalendarGroups"].([]any)
		if root["WasSuccessful"] != true || root["ErrorCode"] != float64(0) || !ok1 || !ok2 {
			return nil, errors.New("Teams reported a calendar folder error")
		}
		return map[string]any{"results": folders, "count": len(folders), "groups": groups, "source": "live_calendar"}, nil
	}
	body, ok := root["Body"].(map[string]any)
	if !ok || body["ResponseClass"] != "Success" || body["ResponseCode"] != "NoError" {
		return nil, errors.New("Teams reported a calendar read error")
	}
	if action == "GetCalendarView" {
		items, ok := body["Items"].([]any)
		if !ok {
			return nil, errors.New("Teams returned an unexpected calendar view")
		}
		return map[string]any{"results": items, "count": len(items), "source": "live_calendar"}, nil
	}
	if action == "SyncCalendarEvents" {
		updated, ok1 := body["UpdatedItems"].([]any)
		deleted, ok2 := body["DeletedItemIds"].([]any)
		cursor, ok3 := body["SyncState"].(string)
		complete, ok4 := body["IncludesLastItemInRange"].(bool)
		if !ok1 || !ok2 || !ok3 || !ok4 {
			return nil, errors.New("Teams returned an unexpected calendar changes page")
		}
		return map[string]any{"results": updated, "count": len(updated), "deleted_ids": deleted, "sync_state": cursor, "partial": !complete, "continuation": "subsequent sync requests are not yet verified", "source": "live_calendar"}, nil
	}
	return nil, errors.New("unsupported calendar response")
}

func configureTeamsCalendarRead(cmd *cobra.Command, flags *rootFlags, action string) {
	var calendarID, start, end string
	cmd.Annotations = map[string]string{"mcp:read-only": "true", "pp:data-source": "live"}
	cmd.Args = cobra.NoArgs
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if flags.dataSource == "local" {
			return usageErr(errors.New("this command reads the live Teams calendar service"))
		}
		if action != "GetCalendarFoldersV2" && start == "" {
			return usageErr(errors.New("--start is required"))
		}
		if action == "GetCalendarView" && end == "" {
			return usageErr(errors.New("--end is required"))
		}
		if dryRunOK(flags) {
			return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": action, "start": start, "end": end})
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
		scope, err := loadTeamsCalendarScope(dir, action, principal)
		if err != nil {
			return authErr(err)
		}
		headers, body, err := teamsCalendarRequest(scope, action, calendarID, start, end)
		if err != nil {
			return err
		}
		guard, ok := c.HTTPClient.Transport.(teamsOriginTransport)
		if !ok {
			return errors.New("Teams session origin guard is unavailable")
		}
		hc := *c.HTTPClient
		hc.Transport = teamsCalendarTransport{next: guard.next, headers: headers, action: action}
		hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		ctx, cancel := boundCtx(cmd.Context(), flags)
		defer cancel()
		data, err := runTeamsCalendarRead(ctx, &hc, action, scope.ObservedQuery, body)
		if err != nil {
			return err
		}
		result, err := decodeTeamsCalendar(data, action)
		if err != nil {
			return err
		}
		if start != "" {
			result["start"] = start
		}
		if end != "" {
			result["end"] = end
		}
		return flags.printJSON(cmd, result)
	}
	if action != "GetCalendarFoldersV2" {
		cmd.Flags().StringVar(&calendarID, "calendar-id", "", "Calendar ID; defaults to the captured primary calendar")
		cmd.Flags().StringVar(&start, "start", "", "Start time in RFC3339 with an offset")
	}
	if action == "GetCalendarView" {
		cmd.Flags().StringVar(&end, "end", "", "End time in RFC3339 with an offset")
	}
}

func newTeamsCalendarListCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "list", Short: "List calendars available in Teams"}
	configureTeamsCalendarRead(cmd, flags, "GetCalendarFoldersV2")
	return cmd
}
func newTeamsCalendarAgendaCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "agenda", Short: "Read calendar events in a time range"}
	configureTeamsCalendarRead(cmd, flags, "GetCalendarView")
	return cmd
}
func newTeamsCalendarChangesCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "changes", Short: "Read one initial page of calendar changes"}
	configureTeamsCalendarRead(cmd, flags, "SyncCalendarEvents")
	return cmd
}
func newTeamsCalendarCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "calendar", Short: "Read the Teams calendar", Annotations: map[string]string{"pp:parent-group": "true"}, RunE: parentNoSubcommandRunE(flags)}
	cmd.AddCommand(newTeamsCalendarListCmd(flags), newTeamsCalendarAgendaCmd(flags), newTeamsCalendarChangesCmd(flags), newTeamsCalendarSyncCmd(flags))
	return cmd
}
func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) { addNovelCommandIfAbsent(root, newTeamsCalendarCmd(flags)) })
}
