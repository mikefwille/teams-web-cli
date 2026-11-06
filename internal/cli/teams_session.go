// Session-backed Teams web authentication. This file is retained across reprints.
package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
	"github.com/mikefwille/teams-web-cli/internal/config"
)

var teamsExportID = regexp.MustCompile(`^[a-z0-9]+-[a-f0-9]{8}$`)
var teamsScopeID = regexp.MustCompile(`^[a-f0-9]{16}$`)
var teamsRequestID = regexp.MustCompile(`^[0-9]+$`)

type teamsSessionPointer struct {
	Version  int    `json:"version"`
	Service  string `json:"service"`
	ExportID string `json:"export_id"`
}

type teamsSessionExport struct {
	Service      string            `json:"service"`
	SourceOrigin string            `json:"source_origin"`
	URL          string            `json:"url"`
	Status       string            `json:"observed_status"`
	Headers      map[string]string `json:"headers"`
}

type teamsAudienceCatalog struct {
	Version int    `json:"version"`
	Service string `json:"service"`
	Scopes  []struct {
		ScopeID    string `json:"scope_id"`
		Origin     string `json:"origin"`
		ExportPath string `json:"export_path"`
		Operation  string `json:"operation"`
	} `json:"scopes"`
}

type teamsAudienceScope struct {
	Version       int               `json:"version"`
	Service       string            `json:"service"`
	ScopeID       string            `json:"scope_id"`
	Origin        string            `json:"origin"`
	ObservedPath  string            `json:"observed_path"`
	Operation     string            `json:"operation"`
	ObservedQuery map[string]string `json:"observed_query"`
	RequestBody   json.RawMessage   `json:"request_body"`
	Headers       map[string]string `json:"headers"`
	OwnerScope    string            `json:"owner_scope"`
	TokenAudience string            `json:"token_audience"`
}

type teamsTokenPrincipal struct {
	Tenant string `json:"tid"`
	Object string `json:"oid"`
	Name   string `json:"name"`
}

func teamsPrincipal(auth string) (teamsTokenPrincipal, bool) {
	_, token, ok := strings.Cut(strings.TrimSpace(auth), " ")
	if !ok {
		return teamsTokenPrincipal{}, false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return teamsTokenPrincipal{}, false
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return teamsTokenPrincipal{}, false
	}
	var p teamsTokenPrincipal
	if json.Unmarshal(data, &p) != nil || p.Tenant == "" || p.Object == "" {
		return teamsTokenPrincipal{}, false
	}
	return p, true
}

func teamsSessionDir() (string, error) {
	if dir := os.Getenv("TEAMS_WEB_SESSION_DIR"); dir != "" {
		return dir, nil
	}
	if base := os.Getenv("M365_WEB_HOME"); base != "" {
		if !filepath.IsAbs(base) {
			return "", errors.New("M365_WEB_HOME must be an absolute path")
		}
		return filepath.Join(base, "private"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "outlook-web-dev", "private"), nil
}

// readPrivateSessionFile rejects symlinks and verifies ownership, permissions,
// and macOS ACLs on the same descriptor used to read the data.
func readPrivateSessionFile(path string, out any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("session file must be a regular file, not a symlink")
	}
	opened, err := cliutil.ReadOpenedConfig(path)
	if err != nil {
		return err
	}
	if opened.PermErr != nil {
		return fmt.Errorf("session file permissions: %w", opened.PermErr)
	}
	if len(opened.Data) > 2_000_000 {
		return errors.New("session file exceeds size limit")
	}
	if err := json.Unmarshal(opened.Data, out); err != nil {
		return errors.New("session file is not valid JSON")
	}
	return nil
}

func teamsOrigin(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "teams.cloud.microsoft") &&
		(u.Port() == "" || u.Port() == "443") && u.User == nil
}

func loadTeamsWebSession(dir string) (*teamsSessionExport, error) {
	session, err := loadTeamsWebSessionForLocalAccount(dir)
	if err != nil {
		return nil, err
	}
	if exp, ok := accessTokenExpiry(session.Headers["authorization"]); ok && time.Now().After(exp) {
		return nil, errors.New("Teams session expired; run the session helper refresh for teams")
	}
	return session, nil
}

// Local data is account-scoped, and remains readable when a previously
// accepted session has expired. This does not authorize a network request.
func loadTeamsWebSessionForLocalAccount(dir string) (*teamsSessionExport, error) {
	var pointer teamsSessionPointer
	if err := readPrivateSessionFile(filepath.Join(dir, "current-teams.json"), &pointer); err != nil {
		return nil, err
	}
	if pointer.Version != 1 || pointer.Service != "teams" || !teamsExportID.MatchString(pointer.ExportID) {
		return nil, errors.New("invalid Teams session pointer")
	}
	var session teamsSessionExport
	if err := readPrivateSessionFile(filepath.Join(dir, "session-teams-"+pointer.ExportID+".json"), &session); err != nil {
		return nil, err
	}
	if session.Service != "teams" || session.Status != "200" || !teamsOrigin(session.SourceOrigin) || !teamsOrigin(session.URL) {
		return nil, errors.New("Teams session export has an unexpected origin or status")
	}
	if !strings.HasPrefix(strings.ToLower(session.Headers["authorization"]), "bearer ") {
		return nil, errors.New("Teams session export has no bearer credential")
	}
	return &session, nil
}

func accessTokenExpiry(auth string) (time.Time, bool) {
	_, token, ok := strings.Cut(strings.TrimSpace(auth), " ")
	if !ok {
		return time.Time{}, false
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp json.RawMessage `json:"exp"`
	}
	if json.Unmarshal(data, &claims) != nil {
		return time.Time{}, false
	}
	var exp int64
	if json.Unmarshal(claims.Exp, &exp) != nil {
		var text string
		if json.Unmarshal(claims.Exp, &text) != nil {
			return time.Time{}, false
		}
		var err error
		exp, err = strconv.ParseInt(text, 10, 64)
		if err != nil {
			return time.Time{}, false
		}
	}
	if exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(exp, 0), true
}

func teamsStableStoreScope(c *config.Config) string {
	if c == nil || c.AuthSource != "web-session" {
		return ""
	}
	_, token, ok := strings.Cut(strings.TrimSpace(c.AuthHeader()), " ")
	if !ok {
		return ""
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Tenant string `json:"tid"`
		Object string `json:"oid"`
	}
	if json.Unmarshal(data, &claims) != nil || claims.Tenant == "" || claims.Object == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(claims.Tenant + "\x00" + claims.Object))
	return "teams-account:" + hex.EncodeToString(sum[:])
}

// A host can accept several unrelated token audiences. Route only between
// service families seen in captured requests; unknown paths stay exact.
func teamsServiceFamily(path string) string {
	lower := strings.ToLower(path)
	switch lower {
	case "/ups/noam/v1/presence/getpresence/", "/ups/noam/v1/me/forceavailability", "/ups/noam/v1/me/publishnote":
		return "presence"
	}
	for _, family := range []string{"csa", "chatsvc", "mt"} {
		if strings.HasPrefix(lower, "/api/"+family+"/") {
			return family
		}
	}
	return "path:" + path
}

func loadTeamsAudienceScopes(dir string, primary teamsTokenPrincipal) (map[string]map[string]string, error) {
	result := map[string]map[string]string{}
	var catalog teamsAudienceCatalog
	if err := readPrivateSessionFile(filepath.Join(dir, "audiences-teams.json"), &catalog); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, nil
		}
		return nil, err
	}
	if catalog.Version != 1 || catalog.Service != "teams" {
		return nil, errors.New("invalid Teams audience catalog")
	}
	for _, entry := range catalog.Scopes {
		if !teamsScopeID.MatchString(entry.ScopeID) || !teamsOrigin(entry.Origin) {
			continue
		}
		name := filepath.Base(entry.ExportPath)
		if entry.ExportPath != filepath.Join(dir, name) || !strings.HasPrefix(name, "authscope-teams-"+entry.ScopeID+"-") || !strings.HasSuffix(name, ".json") {
			return nil, errors.New("invalid Teams audience export path")
		}
		var scope teamsAudienceScope
		if err := readPrivateSessionFile(entry.ExportPath, &scope); err != nil {
			return nil, err
		}
		if scope.Version != 1 || scope.Service != "teams" || scope.ScopeID != entry.ScopeID || !teamsOrigin(scope.Origin) || !strings.HasPrefix(scope.ObservedPath, "/") ||
			!strings.HasPrefix(strings.ToLower(scope.Headers["authorization"]), "bearer ") {
			return nil, errors.New("invalid Teams audience export")
		}
		principal, ok := teamsPrincipal(scope.Headers["authorization"])
		if !ok || principal.Tenant != primary.Tenant || principal.Object != primary.Object {
			return nil, errors.New("Teams audience credential belongs to a different or unknown account")
		}
		if exp, ok := accessTokenExpiry(scope.Headers["authorization"]); ok && time.Now().After(exp) {
			continue
		}
		result[teamsServiceFamily(scope.ObservedPath)] = scope.Headers
	}
	return result, nil
}

type teamsOriginTransport struct {
	next   http.RoundTripper
	scopes map[string]map[string]string
}

func (t teamsOriginTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || !teamsOrigin(req.URL.String()) {
		return nil, errors.New("refusing to send Teams session credentials to another origin")
	}
	if t.scopes != nil {
		headers, ok := t.scopes[teamsServiceFamily(req.URL.Path)]
		if !ok {
			return nil, errors.New("no observed Teams credential for this service; capture and import the matching audience")
		}
		if exp, known := accessTokenExpiry(headers["authorization"]); known && time.Now().After(exp) {
			return nil, errors.New("Teams service credential expired; refresh its audience")
		}
		for key := range req.Header {
			lower := strings.ToLower(key)
			if lower == "authorization" || lower == "cookie" || strings.HasPrefix(lower, "x-ms-") || lower == "x-ringoverride" {
				req.Header.Del(key)
			}
		}
		omit := map[string]bool{"host": true, "connection": true, "content-length": true, "accept-encoding": true, "transfer-encoding": true, "proxy-authorization": true, "proxy-connection": true, "upgrade": true, "te": true, "trailer": true}
		for key, value := range headers {
			// A captured read may have a different content type than a write.
			if strings.EqualFold(key, "content-type") && req.Header.Get("Content-Type") != "" {
				continue
			}
			if !omit[strings.ToLower(key)] && !strings.HasPrefix(key, ":") && !strings.ContainsAny(value, "\r\n") {
				req.Header.Set(key, value)
			}
		}
	}
	return t.next.RoundTrip(req)
}

func attachTeamsWebSession(c *client.Client) error {
	if c == nil || c.Config == nil || !teamsOrigin(c.BaseURL) || c.Config.AuthHeader() != "" {
		return nil
	}
	disabled, err := teamsSessionDisabled()
	if err != nil {
		return fmt.Errorf("checking Teams session state: %w", err)
	}
	if disabled {
		return nil
	}
	dir, err := teamsSessionDir()
	if err != nil {
		return nil
	}
	session, err := loadTeamsWebSession(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil // Keep ordinary generated config and offline verification usable.
	}
	if err != nil {
		return fmt.Errorf("loading private Teams session: %w", err)
	}
	primary, ok := teamsPrincipal(session.Headers["authorization"])
	if !ok {
		return errors.New("Teams session identity is unavailable")
	}
	scopes, err := loadTeamsAudienceScopes(dir, primary)
	if err != nil {
		return fmt.Errorf("loading Teams audience catalog: %w", err)
	}
	if u, err := url.Parse(session.URL); err == nil {
		family := teamsServiceFamily(u.Path)
		current, exists := scopes[family]
		primaryExpiry, primaryKnown := accessTokenExpiry(session.Headers["authorization"])
		currentExpiry, currentKnown := accessTokenExpiry(current["authorization"])
		if !exists || !currentKnown || (primaryKnown && primaryExpiry.After(currentExpiry)) {
			scopes[family] = session.Headers
		}
	}
	c.Config.AuthHeaderVal = session.Headers["authorization"]
	c.Config.AuthSource = "web-session"
	if c.Config.Headers == nil {
		c.Config.Headers = map[string]string{}
	}
	for _, key := range []string{"cookie", "accept", "accept-language", "user-agent", "x-ms-client-capabilities", "x-ms-migration", "x-ms-test-user"} {
		if value := session.Headers[key]; value != "" {
			c.Config.Headers[key] = value
		}
	}
	if c.HTTPClient == nil {
		return errors.New("Teams client has no HTTP transport")
	}
	next := c.HTTPClient.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	c.HTTPClient.Transport = teamsOriginTransport{next: next, scopes: scopes}
	c.NoCache = true // Audience-specific credentials must not share generated response-cache entries.
	return nil
}

func init() {
	registerClientHook(attachTeamsWebSession)
	prior := config.StoreScopeIdentity
	config.StoreScopeIdentity = func(c *config.Config) string {
		if id := teamsStableStoreScope(c); id != "" {
			return id
		}
		if prior != nil {
			return prior(c)
		}
		return ""
	}
}
