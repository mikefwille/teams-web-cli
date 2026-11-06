package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTeamsFilesDeleteUsesFreshSameAudienceToken(t *testing.T) {
	dir := t.TempDir()
	p := teamsTokenPrincipal{Tenant: "tenant", Object: "owner"}
	sum := sha256.Sum256([]byte(p.Tenant + "\x00" + p.Object))
	owner := hex.EncodeToString(sum[:])
	token := func(exp any) string {
		b, _ := json.Marshal(map[string]any{"tid": "tenant", "oid": "owner", "aud": "audience", "scp": "Files.ReadWrite.All", "exp": exp})
		return "Bearer aa." + base64.RawURLEncoding.EncodeToString(b) + ".bb"
	}
	oldID, freshID := "1111111111111111", "2222222222222222"
	oldPath := filepath.Join(dir, "authscope-teams-"+oldID+"-123456789abc.json")
	freshPath := filepath.Join(dir, "authscope-teams-"+freshID+"-123456789abc.json")
	old := teamsAudienceScope{Version: 1, Service: "teams", ScopeID: oldID, Origin: teamsFilesOrigin, Operation: "DeleteFile", ObservedPath: "/_api/v2.1/drives/drive-id/items/old-id", OwnerScope: owner, TokenAudience: "audience", Headers: map[string]string{"authorization": token(time.Now().Add(-time.Hour).Unix())}}
	fresh := teamsAudienceScope{Version: 1, Service: "teams", ScopeID: freshID, Origin: teamsFilesOrigin, Operation: "FilesTokenRead", ObservedPath: "/_api/v2.0/sites/fixture-my.sharepoint.com:/personal/user_fixture_invalid", OwnerScope: owner, TokenAudience: "audience", Headers: map[string]string{"authorization": token(time.Now().Add(time.Hour).Unix()), "cookie": "fresh-cookie"}}
	write := func(name string, value any) {
		t.Helper()
		b, _ := json.Marshal(value)
		if err := os.WriteFile(name, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(oldPath, old)
	write(freshPath, fresh)
	write(filepath.Join(dir, "audiences-teams.json"), map[string]any{"version": 1, "service": "teams", "scopes": []any{map[string]any{"scope_id": oldID, "origin": teamsFilesOrigin, "operation": "DeleteFile", "export_path": oldPath}, map[string]any{"scope_id": freshID, "origin": teamsFilesOrigin, "operation": "FilesTokenRead", "export_path": freshPath}}})
	got, err := loadTeamsFilesWriteScope(dir, p, "DeleteFile")
	if err != nil || got.Headers["authorization"] != fresh.Headers["authorization"] || got.Headers["cookie"] != "fresh-cookie" {
		t.Fatalf("fresh token not used: %v", err)
	}
	fresh.ObservedPath = "/personal/user_fixture_invalid/_api/web/lists/GetByTitle('Synthetic%20Fixtures')/items"
	write(freshPath, fresh)
	if _, err := loadTeamsFilesWriteScope(dir, p, "DeleteFile"); err != nil {
		t.Fatalf("observed list read rejected: %v", err)
	}
	fresh.ObservedPath = "/personal/user_fixture_invalid/_api/web/lists/GetByTitle('Bad%2FPath')/items"
	write(freshPath, fresh)
	if _, err := loadTeamsFilesWriteScope(dir, p, "DeleteFile"); err == nil {
		t.Fatal("unobserved list path accepted")
	}
	fresh.ObservedPath = "/_api/v2.0/sites/fixture-my.sharepoint.com:/personal/user_fixture_invalid"
	fresh.TokenAudience = "other"
	write(freshPath, fresh)
	if _, err := loadTeamsFilesWriteScope(dir, p, "DeleteFile"); err == nil {
		t.Fatal("other audience accepted")
	}
}

func TestTeamsFilesDigestUsesContextInfoWithoutStaleDigest(t *testing.T) {
	previous := teamsFilesLimiter
	teamsFilesLimiter = nil
	defer func() { teamsFilesLimiter = previous }()
	base := &messageWriteTransport{response: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"d":{"GetContextWebInformation":{"FormDigestValue":"fresh","FormDigestTimeoutSeconds":1800}}}`))}}
	scope := teamsAudienceScope{ObservedPath: `/personal/user_fixture_invalid/_api/web/GetFolderByServerRelativePath(DecodedUrl=@a1)/Files/AddUsingPath(DecodedUrl=@a2,AutoCheckoutOnInvalidData=@a3,XorHash=@a4)`, Headers: map[string]string{"cookie": "cookie", "x-requestdigest": "expired", "x-http-method": "MERGE", "if-match": "*"}}
	digest, seconds, err := teamsFreshFilesDigest(context.Background(), base, scope)
	if err != nil || digest != "fresh" || seconds != 1800 || base.headers.Get("X-Requestdigest") != "" || base.headers.Get("X-Http-Method") != "" {
		t.Fatalf("digest response: %q %d %v", digest, seconds, err)
	}
}

func TestTeamsQuickXorKnownVectors(t *testing.T) {
	many := make([]byte, 256)
	for i := range many {
		many[i] = byte(i)
	}
	for _, tc := range []struct {
		data []byte
		want string
	}{{[]byte{}, "AAAAAAAAAAAAAAAAAAAAAAAAAAA="}, {[]byte("abc"), "YRDDGAAAAAAAAAAAAwAAAAAAAAA="}, {many, "QkGEfSisZcA7k+FCh71r2dbCayY="}} {
		if got := teamsQuickXor(tc.data); got != tc.want {
			t.Fatalf("hash mismatch: %q", got)
		}
	}
}
func TestTeamsFilesWritesStayOnObservedOrigin(t *testing.T) {
	path := `/personal/user_fixture_invalid/_api/web/GetListUsingPath(DecodedUrl=@a1)/items(@a2)`
	q := url.Values{"@a1": []string{"'/personal/user_fixture_invalid/Documents'"}, "@a2": []string{"'3'"}}
	base := &messageWriteTransport{response: &http.Response{StatusCode: 204, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}}
	scope := teamsAudienceScope{Headers: map[string]string{"cookie": "cookie", "x-requestdigest": "digest", "authorization": "Bearer"}}
	tr := teamsFilesWriteTransport{next: base, scope: scope, operation: "RenameFile", expectedPath: path, expectedQuery: q}
	bad, _ := http.NewRequest(http.MethodPost, "https://evil.invalid"+path+"?"+q.Encode(), nil)
	if _, err := tr.RoundTrip(bad); err == nil || base.calls != 0 {
		t.Fatal("off-origin write accepted")
	}
	bad, _ = http.NewRequest(http.MethodPost, teamsFilesOrigin+path+"?"+q.Encode()+"&extra=1", nil)
	if _, err := tr.RoundTrip(bad); err == nil || base.calls != 0 {
		t.Fatal("extra query accepted")
	}
	good, _ := http.NewRequest(http.MethodPost, teamsFilesOrigin+path+"?"+q.Encode(), nil)
	if _, err := tr.RoundTrip(good); err != nil || base.headers.Get("Cookie") != "cookie" || base.headers.Get("X-Requestdigest") != "digest" || base.headers.Get("Authorization") != "" {
		t.Fatal("observed write blocked or bare bearer forwarded")
	}
}
func TestTeamsFilesDecodeItemIdentifiers(t *testing.T) {
	item := map[string]any{"UniqueId": "{11111111-1111-1111-1111-111111111111}", "FileLeafRef": "fixture.txt", "FSObjType": "0", "ID": "3", ".spItemUrl": "https://fixture-my.sharepoint.com:443/_api/v2.0/drives/drive-id/items/item-id?version=1"}
	data, _ := json.Marshal(map[string]any{"ListData": map[string]any{"Row": []any{item}, "RowLimit": 30}})
	items, _, _, err := decodeTeamsFiles(data, 10)
	if err != nil || items[0]["list_item_id"] != "3" || items[0]["drive_id"] != "drive-id" || items[0]["item_id"] != "item-id" {
		t.Fatalf("item references unavailable: %#v %v", items, err)
	}
}
