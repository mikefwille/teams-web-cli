package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mikefwille/teams-web-cli/internal/cliutil"
)

func TestTeamsSearchPayloadUsesScopedLiteralQuery(t *testing.T) {
	body, err := teamsSearchPayload("48:notes", "syntheticterm", "UTC", 0, 25)
	if err != nil {
		t.Fatal(err)
	}
	queries := body["entityRequests"].([]any)
	query := queries[0].(map[string]any)["query"].(map[string]string)
	if !strings.HasPrefix(query["queryString"], "clientthreadid:48:notes ") || !strings.HasSuffix(query["queryString"], " syntheticterm") {
		t.Fatalf("unexpected scoped query: %#v", query)
	}
	for _, tc := range []struct{ conversation, term string }{{"48:notes", "one OR two"}, {"48:notes OR other", "term"}, {"48:notes", "*"}} {
		if _, err := teamsSearchPayload(tc.conversation, tc.term, "UTC", 0, 25); err == nil {
			t.Fatal("unsafe search syntax accepted")
		}
	}
}

func TestTeamsGlobalSearchUsesObservedMessagesRequest(t *testing.T) {
	body, err := teamsSearchPayload("", "syntheticterm", "UTC", 0, 25)
	if err != nil {
		t.Fatal(err)
	}
	request := body["entityRequests"].([]any)[0].(map[string]any)
	query := request["query"].(map[string]string)
	if query["queryString"] != "NOT (isClientSoftDeleted:TRUE) syntheticterm" || request["topResultsCount"] != 5 {
		t.Fatalf("unexpected global query: %#v", request)
	}
	if _, ok := request["filter"]; ok {
		t.Fatal("global search incorrectly included a conversation filter")
	}
	result := []byte(`{"EntitySets":[{"EntityType":"Message","ResultSets":[{"Results":[{"Source":{"ClientThreadId":"other"}}],"Total":1}]}]}`)
	if hits, _, _, err := decodeTeamsSearch(result, ""); err != nil || len(hits) != 1 {
		t.Fatalf("global result rejected: %v", err)
	}
}

func TestTeamsSearchResponseRejectsOutOfScopeResults(t *testing.T) {
	data := []byte(`{"EntitySets":[{"EntityType":"Message","IsPartial":false,"ResultSets":[{"Results":[{"Source":{"ClientThreadId":"48:notes","Preview":"hello"}}],"Total":1,"MoreResultsAvailable":false}]}]}`)
	results, total, partial, err := decodeTeamsSearch(data, "48:notes")
	if err != nil || len(results) != 1 || total != 1 || partial {
		t.Fatalf("valid search response rejected: %v", err)
	}
	if _, _, _, err := decodeTeamsSearch(data, "other"); err == nil {
		t.Fatal("out-of-scope result accepted")
	}
	if _, _, _, err := decodeTeamsSearch([]byte(`{"error":{"code":"denied"}}`), "48:notes"); err == nil {
		t.Fatal("error was treated as no results")
	}
}

func TestTeamsSearchTransportRestrictsDestinationAndAudience(t *testing.T) {
	underlying := &messageWriteTransport{response: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"EntitySets":[]}`))}}
	transport := teamsSearchTransport{next: underlying, headers: map[string]string{"authorization": "Bearer search-only", "content-type": "bad"}}
	bad, _ := http.NewRequest(http.MethodPost, "https://other.invalid/searchservice/api/v2/query", nil)
	if _, err := transport.RoundTrip(bad); err == nil || underlying.calls != 0 {
		t.Fatal("off-origin search request was accepted")
	}
	good, _ := http.NewRequest(http.MethodPost, teamsSearchOrigin+teamsSearchPath, nil)
	good.Header.Set("Content-Type", "application/json")
	if _, err := transport.RoundTrip(good); err != nil || underlying.headers.Get("Authorization") != "Bearer search-only" || underlying.headers.Get("Content-Type") != "application/json" {
		t.Fatal("search audience or content type was not applied")
	}
}

func TestTeamsSearchAudienceRequiresSameAccount(t *testing.T) {
	dir := t.TempDir()
	scopeID := "0123456789abcdef"
	scopeName := "authscope-teams-" + scopeID + "-123456789abc.json"
	scopePath := filepath.Join(dir, scopeName)
	claims, _ := json.Marshal(map[string]any{"tid": "tenant-a", "oid": "user-a", "exp": time.Now().Add(time.Hour).Unix()})
	token := "Bearer aa." + base64.RawURLEncoding.EncodeToString(claims) + ".bb"
	scope := teamsAudienceScope{Version: 1, Service: "teams", ScopeID: scopeID, Origin: teamsSearchOrigin, ObservedPath: teamsSearchPath, Headers: map[string]string{"authorization": token}}
	data, _ := json.Marshal(scope)
	if err := os.WriteFile(scopePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	catalog := map[string]any{"version": 1, "service": "teams", "scopes": []any{map[string]any{"scope_id": scopeID, "origin": teamsSearchOrigin, "export_path": scopePath}}}
	data, _ = json.Marshal(catalog)
	if err := os.WriteFile(filepath.Join(dir, "audiences-teams.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTeamsSearchHeaders(dir, teamsTokenPrincipal{Tenant: "tenant-a", Object: "user-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTeamsSearchHeaders(dir, teamsTokenPrincipal{Tenant: "tenant-a", Object: "other"}); err == nil {
		t.Fatal("other account search credential accepted")
	}
}

func TestTeamsSearchRateLimitIsTyped(t *testing.T) {
	old := teamsSearchLimiter
	teamsSearchLimiter = nil
	defer func() { teamsSearchLimiter = old }()
	transport := &messageWriteTransport{response: &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"5"}}, Body: io.NopCloser(strings.NewReader(`{}`))}}
	_, err := runTeamsSearch(context.Background(), &http.Client{Transport: transport}, map[string]any{})
	var limited *cliutil.RateLimitError
	if !errors.As(err, &limited) || limited.RetryAfter != 5*time.Second || transport.calls != 1 {
		t.Fatalf("unexpected rate limit: %v calls=%d", err, transport.calls)
	}
}
