package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mikefwille/teams-web-cli/internal/cliutil"
)

func TestTeamsCalendarParameterizesObservedView(t *testing.T) {
	template := `{"__type":"GetCalendarViewJsonRequest:#Exchange","Header":{"TimeZoneContext":{"TimeZoneDefinition":{"Id":"Mountain Standard Time"}}},"Body":{"CalendarId":{"BaseFolderId":{"Id":"old"}},"RangeStart":"","RangeEnd":""}}`
	scope := teamsAudienceScope{Headers: map[string]string{"x-owa-urlpostdata": url.QueryEscape(template)}}
	headers, body, err := teamsCalendarRequest(scope, "GetCalendarView", "synthetic-calendar", "2026-11-06T16:00:00Z", "2026-11-06T17:00:00Z")
	if err != nil || body != nil {
		t.Fatalf("calendar request: %v", err)
	}
	decoded, err := url.QueryUnescape(headers["x-owa-urlpostdata"])
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if json.Unmarshal([]byte(decoded), &root) != nil {
		t.Fatal("invalid envelope")
	}
	b := root["Body"].(map[string]any)
	if b["RangeStart"] != "2026-11-06T09:00:00.000" || b["RangeEnd"] != "2026-11-06T10:00:00.000" || b["CalendarId"].(map[string]any)["BaseFolderId"].(map[string]any)["Id"] != "synthetic-calendar" {
		t.Fatalf("unexpected calendar envelope: %#v", b)
	}
	if _, _, err := teamsCalendarRequest(scope, "GetCalendarView", "", "2026-11-01T07:30:00Z", "2026-11-01T10:30:00Z"); err == nil {
		t.Fatal("ambiguous DST time accepted")
	}
}

func TestTeamsCalendarResponseDistinguishesEmptyFromErrors(t *testing.T) {
	folders, err := decodeTeamsCalendar([]byte(`{"CalendarFolders":[],"CalendarGroups":[],"WasSuccessful":true,"ErrorCode":0}`), "GetCalendarFoldersV2")
	if err != nil || folders["count"] != 0 {
		t.Fatalf("empty folders: %v", err)
	}
	agenda, err := decodeTeamsCalendar([]byte(`{"Body":{"Items":[{"Subject":"synthetic"}],"ResponseClass":"Success","ResponseCode":"NoError"}}`), "GetCalendarView")
	if err != nil || agenda["count"] != 1 {
		t.Fatalf("populated agenda: %v", err)
	}
	if _, err := decodeTeamsCalendar([]byte(`{"Body":{"Items":[],"ResponseClass":"Error","ResponseCode":"Denied"}}`), "GetCalendarView"); err == nil {
		t.Fatal("calendar error accepted as empty")
	}
	changes, err := decodeTeamsCalendar([]byte(`{"Body":{"UpdatedItems":[],"DeletedItemIds":[],"SyncState":"opaque","IncludesLastItemInRange":false,"ResponseClass":"Success","ResponseCode":"NoError"}}`), "SyncCalendarEvents")
	if err != nil || changes["partial"] != true {
		t.Fatal("partial calendar page was not reported")
	}
}

func TestTeamsCalendarTransportAllowsOnlyObservedOperation(t *testing.T) {
	underlying := &messageWriteTransport{response: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}}
	tr := teamsCalendarTransport{next: underlying, headers: map[string]string{"authorization": "Bearer calendar-only"}, action: "GetCalendarView"}
	bad, _ := http.NewRequest(http.MethodPost, teamsCalendarOrigin+teamsCalendarPath+"?action=DeleteItem&app=Calendar&n=1", nil)
	if _, err := tr.RoundTrip(bad); err == nil || underlying.calls != 0 {
		t.Fatal("unobserved calendar operation accepted")
	}
	good, _ := http.NewRequest(http.MethodPost, teamsCalendarOrigin+teamsCalendarPath+"?action=GetCalendarView&app=Calendar&n=1", nil)
	if _, err := tr.RoundTrip(good); err != nil || underlying.headers.Get("Authorization") != "Bearer calendar-only" {
		t.Fatal("calendar audience not applied")
	}
}

func TestTeamsCalendarAudienceRequiresSameAccount(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef"
	name := "authscope-teams-" + id + "-123456789abc.json"
	file := filepath.Join(dir, name)
	claims, _ := json.Marshal(map[string]any{"tid": "tenant-a", "oid": "user-a", "exp": time.Now().Add(time.Hour).Unix()})
	token := "Bearer aa." + base64.RawURLEncoding.EncodeToString(claims) + ".bb"
	scope := teamsAudienceScope{Version: 1, Service: "teams", ScopeID: id, Origin: teamsCalendarOrigin, ObservedPath: teamsCalendarPath, Operation: "GetCalendarView", ObservedQuery: map[string]string{"action": "GetCalendarView", "app": "Calendar", "n": "1"}, Headers: map[string]string{"authorization": token}}
	data, _ := json.Marshal(scope)
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	catalog := map[string]any{"version": 1, "service": "teams", "scopes": []any{map[string]any{"scope_id": id, "origin": teamsCalendarOrigin, "operation": "GetCalendarView", "export_path": file}}}
	data, _ = json.Marshal(catalog)
	if err := os.WriteFile(filepath.Join(dir, "audiences-teams.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTeamsCalendarScope(dir, "GetCalendarView", teamsTokenPrincipal{Tenant: "tenant-a", Object: "user-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTeamsCalendarScope(dir, "GetCalendarView", teamsTokenPrincipal{Tenant: "tenant-a", Object: "other"}); err == nil {
		t.Fatal("calendar credential from another account accepted")
	}
}

func TestTeamsCalendarRateLimitIsTyped(t *testing.T) {
	old := teamsCalendarLimiter
	teamsCalendarLimiter = nil
	defer func() { teamsCalendarLimiter = old }()
	transport := &messageWriteTransport{response: &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"5"}}, Body: io.NopCloser(strings.NewReader(`{}`))}}
	_, err := runTeamsCalendarRead(context.Background(), &http.Client{Transport: transport}, "GetCalendarView", map[string]string{"action": "GetCalendarView", "app": "Calendar", "n": "1"}, nil)
	var limited *cliutil.RateLimitError
	if !errors.As(err, &limited) || limited.RetryAfter != 5*time.Second || transport.calls != 1 {
		t.Fatalf("unexpected rate limit: %v", err)
	}
}
