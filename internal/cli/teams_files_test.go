package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTeamsFilesScopeRequiresSelectedAccount(t *testing.T) {
	dir := t.TempDir()
	principal := teamsTokenPrincipal{Tenant: "tenant", Object: "owner"}
	sum := sha256.Sum256([]byte(principal.Tenant + "\x00" + principal.Object))
	id := "0123456789abcdef"
	name := "authscope-teams-" + id + "-123456789abc.json"
	scope := teamsAudienceScope{Version: 1, Service: "teams", ScopeID: id, Origin: teamsFilesOrigin, Operation: "ListFiles", OwnerScope: hex.EncodeToString(sum[:]), ObservedPath: `/personal/user_fixture_invalid/_api/web/GetListUsingPath(DecodedUrl=@a1)/RenderListDataAsStream`, ObservedQuery: map[string]string{"@a1": "'/personal/user_fixture_invalid/Documents'", "TryNewExperienceSingle": "TRUE"}, Headers: map[string]string{"cookie": "private-cookie"}, RequestBody: json.RawMessage(`{"parameters":{"__metadata":{"type":"SP.RenderListDataParameters"},"RenderOptions":1496839}}`)}
	b, _ := json.Marshal(scope)
	if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
		t.Fatal(err)
	}
	catalog := map[string]any{"version": 1, "service": "teams", "scopes": []any{map[string]any{"scope_id": id, "origin": teamsFilesOrigin, "operation": "ListFiles", "export_path": filepath.Join(dir, name)}}}
	b, _ = json.Marshal(catalog)
	if err := os.WriteFile(filepath.Join(dir, "audiences-teams.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTeamsFilesScope(dir, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTeamsFilesScope(dir, teamsTokenPrincipal{Tenant: "tenant", Object: "other"}); err == nil {
		t.Fatal("another account's cookie was accepted")
	}
}

func TestTeamsFilesListingExposesOnlySafeFields(t *testing.T) {
	data := []byte(`{"ListSchema":{".driveAccessToken":"secret-value"},"ListData":{"Row":[{"UniqueId":"{11111111-1111-1111-1111-111111111111}","FileLeafRef":"example.txt","FileRef":"/Documents/example.txt","File_x0020_Size":"66",".driveAccessToken":"hidden","FSObjType":"0"}],"RowLimit":30}}`)
	items, partial, truncated, err := decodeTeamsFiles(data, 100)
	if err != nil || len(items) != 1 || partial || truncated || items[0]["size_bytes"] != int64(66) {
		t.Fatalf("listing %v %#v", err, items)
	}
	b, _ := json.Marshal(items)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "hidden") {
		t.Fatal("internal token leaked in listing")
	}
	if _, _, _, err := decodeTeamsFiles([]byte(`{"error":{"code":"failure"},"ListData":{"Row":[]}}`), 1); err == nil {
		t.Fatal("service error was accepted")
	}
	if _, _, _, err := decodeTeamsFiles([]byte(`{"ListData":{"Row":[{"UniqueId":"invalid"}]}}`), 1); err == nil {
		t.Fatal("invalid item ID was accepted")
	}
}
func TestTeamsFilesTransportPinsOriginPathAndQuery(t *testing.T) {
	path := `/personal/user_fixture_invalid/_api/web/GetListUsingPath(DecodedUrl=@a1)/RenderListDataAsStream`
	q := map[string]string{"@a1": "'/personal/user_fixture_invalid/Documents'", "TryNewExperienceSingle": "TRUE"}
	scope := teamsAudienceScope{ObservedPath: path, ObservedQuery: q, Headers: map[string]string{"cookie": "private-cookie"}}
	base := &messageWriteTransport{response: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}}
	transport := teamsFilesTransport{next: base, scope: scope}
	values := url.Values{}
	for k, v := range q {
		values.Set(k, v)
	}
	bad, _ := http.NewRequest(http.MethodPost, "https://evil.invalid"+path+"?"+values.Encode(), nil)
	if _, err := transport.RoundTrip(bad); err == nil || base.calls != 0 {
		t.Fatal("off-origin request accepted")
	}
	bad, _ = http.NewRequest(http.MethodPost, teamsFilesOrigin+path+"?"+values.Encode()+"&x=1", nil)
	if _, err := transport.RoundTrip(bad); err == nil || base.calls != 0 {
		t.Fatal("unobserved listing query accepted")
	}
	good, _ := http.NewRequest(http.MethodPost, teamsFilesOrigin+path+"?"+values.Encode(), nil)
	if _, err := transport.RoundTrip(good); err != nil || base.headers.Get("Cookie") != "private-cookie" {
		t.Fatal("observed listing blocked")
	}
	transport.download = true
	bad, _ = http.NewRequest(http.MethodGet, teamsFilesOrigin+"/personal/user_fixture_invalid/_layouts/15/download.aspx?UniqueId=bad", nil)
	if _, err := transport.RoundTrip(bad); err == nil {
		t.Fatal("invalid download ID accepted")
	}
	good, _ = http.NewRequest(http.MethodGet, teamsFilesOrigin+"/personal/user_fixture_invalid/_layouts/15/download.aspx?UniqueId=11111111-1111-1111-1111-111111111111", nil)
	if _, err := transport.RoundTrip(good); err != nil {
		t.Fatal("observed download blocked")
	}
}
