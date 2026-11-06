package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/config"
)

type teamsTestTransport struct{ called bool }

func (t *teamsTestTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.called = true
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
}

type expectedTeamsAuthTransport struct{ token string }

func TestTeamsSessionDirectoryHonorsSharedM365Home(t *testing.T) {
	t.Setenv("TEAMS_WEB_SESSION_DIR", "")
	base := t.TempDir()
	t.Setenv("M365_WEB_HOME", base)
	if got, err := teamsSessionDir(); err != nil || got != filepath.Join(base, "private") {
		t.Fatalf("shared session directory = %q, %v", got, err)
	}
	t.Setenv("M365_WEB_HOME", "relative")
	if _, err := teamsSessionDir(); err == nil {
		t.Fatal("relative M365_WEB_HOME was accepted")
	}
}

func (t expectedTeamsAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	status := 200
	if req.Header.Get("Authorization") != t.token || req.Header.Get("Cookie") != "test-cookie" {
		status = 401
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(`{"teams":[]}`)), Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
}

func TestTeamsGeneratedClientSendsSessionCredential(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TEAMS_WEB_SESSION_DIR", dir)
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	token := writeTeamsFixture(t, dir, time.Now().Add(time.Hour))
	flags := &rootFlags{timeout: time.Second}
	c, err := flags.newClient()
	if err != nil {
		t.Fatal(err)
	}
	guard := c.HTTPClient.Transport.(teamsOriginTransport)
	guard.next = expectedTeamsAuthTransport{token: token}
	c.HTTPClient.Transport = guard
	_, err = c.GetNoCache(context.Background(), "/api/csa/amer/api/v3/teams/users/me", map[string]string{"enableMembershipSummary": "true"})
	if err != nil {
		t.Fatalf("generated request did not carry captured credentials: %v", err)
	}
}

func TestTeamsClientSelectsAudienceForObservedService(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TEAMS_WEB_SESSION_DIR", dir)
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	writeTeamsFixture(t, dir, time.Now().Add(time.Hour))
	primaryPath := filepath.Join(dir, "session-teams-abc-1234abcd.json")
	var primary teamsSessionExport
	b, err := os.ReadFile(primaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &primary); err != nil {
		t.Fatal(err)
	}
	primary.URL = "https://teams.cloud.microsoft/api/mt/part/amer-02/beta/users/tenantsv2"
	b, _ = json.Marshal(primary)
	if err := os.WriteFile(primaryPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "aud": "bootstrap", "tid": "tenant-test", "oid": "account-test"})
	bootstrapToken := "Bearer aa." + base64.RawURLEncoding.EncodeToString(claims) + ".bb"
	scopeID := "0123456789abcdef"
	scopeName := "authscope-teams-" + scopeID + "-123456789abc.json"
	scope := map[string]any{"version": 1, "service": "teams", "scope_id": scopeID, "origin": "https://teams.cloud.microsoft", "observed_path": "/api/csa/amer/api/v3/teams/users/me", "headers": map[string]string{"authorization": bootstrapToken, "cookie": "test-cookie"}}
	b, _ = json.Marshal(scope)
	if err := os.WriteFile(filepath.Join(dir, scopeName), b, 0600); err != nil {
		t.Fatal(err)
	}
	catalog := map[string]any{"version": 1, "service": "teams", "scopes": []any{map[string]any{"scope_id": scopeID, "origin": "https://teams.cloud.microsoft", "export_path": filepath.Join(dir, scopeName)}}}
	b, _ = json.Marshal(catalog)
	if err := os.WriteFile(filepath.Join(dir, "audiences-teams.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	flags := &rootFlags{timeout: time.Second}
	c, err := flags.newClient()
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := c.HTTPClient.Transport.(teamsOriginTransport)
	if !ok {
		t.Fatal("session origin guard missing")
	}
	transport.next = expectedTeamsAuthTransport{token: bootstrapToken}
	c.HTTPClient.Transport = transport
	_, err = c.GetNoCache(context.Background(), "/api/csa/amer/api/v3/teams/users/me", nil)
	if err != nil {
		t.Fatalf("bootstrap request used the wrong audience: %v", err)
	}
}

func writeTeamsFixture(t *testing.T, dir string, expires time.Time) string {
	t.Helper()
	id := "abc-1234abcd"
	claims, _ := json.Marshal(map[string]any{"exp": expires.Unix(), "tid": "tenant-test", "oid": "account-test"})
	token := "Bearer aa." + base64.RawURLEncoding.EncodeToString(claims) + ".bb"
	pointer, _ := json.Marshal(teamsSessionPointer{Version: 1, Service: "teams", ExportID: id})
	session, _ := json.Marshal(teamsSessionExport{Service: "teams", SourceOrigin: "https://teams.cloud.microsoft", URL: "https://teams.cloud.microsoft/api/csa/amer/api/v3/teams/users/me", Status: "200", Headers: map[string]string{"authorization": token, "cookie": "test-cookie", "x-ms-client-capabilities": "test"}})
	if err := os.WriteFile(filepath.Join(dir, "current-teams.json"), pointer, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session-teams-"+id+".json"), session, 0600); err != nil {
		t.Fatal(err)
	}
	return token
}

func TestTeamsSessionAttachesAndBlocksOtherOrigins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TEAMS_WEB_SESSION_DIR", dir)
	want := writeTeamsFixture(t, dir, time.Now().Add(time.Hour))
	transport := &teamsTestTransport{}
	c := &client.Client{BaseURL: "https://teams.cloud.microsoft", Config: &config.Config{BaseURL: "https://teams.cloud.microsoft"}, HTTPClient: &http.Client{Transport: transport}}
	if err := attachTeamsWebSession(c); err != nil {
		t.Fatal(err)
	}
	if c.Config.AuthHeader() != want || c.Config.Headers["cookie"] != "test-cookie" {
		t.Fatal("session credentials not attached")
	}
	bad, _ := http.NewRequest(http.MethodGet, "https://attacker.invalid/collect", nil)
	if _, err := c.HTTPClient.Transport.RoundTrip(bad); err == nil || transport.called {
		t.Fatal("off-origin request was not refused")
	}
	good, _ := http.NewRequest(http.MethodGet, "https://teams.cloud.microsoft/api/csa/amer/api/v3/teams/users/me", nil)
	if _, err := c.HTTPClient.Transport.RoundTrip(good); err != nil || !transport.called {
		t.Fatal("same-origin request did not reach transport")
	}
}

func TestTeamsSessionRejectsExpiredAndUntrustedFiles(t *testing.T) {
	dir := t.TempDir()
	writeTeamsFixture(t, dir, time.Now().Add(-time.Minute))
	if _, err := loadTeamsWebSession(dir); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatal("expired session accepted")
	}
	writeTeamsFixture(t, dir, time.Now().Add(time.Hour))
	pointer := filepath.Join(dir, "current-teams.json")
	if err := os.Chmod(pointer, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTeamsWebSession(dir); err == nil {
		t.Fatal("group-readable pointer accepted")
	}
}

func TestTeamsTokenExpiryAcceptsNumericAndStringClaims(t *testing.T) {
	for _, value := range []any{float64(1793963152), "1793963152"} {
		claims, _ := json.Marshal(map[string]any{"exp": value})
		token := "Bearer aa." + base64.RawURLEncoding.EncodeToString(claims) + ".bb"
		got, ok := accessTokenExpiry(token)
		if !ok || got.Unix() != 1793963152 {
			t.Fatalf("expiry %v: %v %v", value, got, ok)
		}
	}
}

func TestTeamsSessionDoesNotOverrideConfiguredCredentialsOrOtherOrigins(t *testing.T) {
	t.Setenv("TEAMS_WEB_SESSION_DIR", t.TempDir())
	c := &client.Client{BaseURL: "https://other.invalid", Config: &config.Config{AuthHeaderVal: "explicit"}}
	if err := attachTeamsWebSession(c); err != nil || c.Config.AuthHeaderVal != "explicit" {
		t.Fatal("explicit credential changed")
	}
}
