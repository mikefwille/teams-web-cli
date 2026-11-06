// OneDrive mutations are limited to the observed Documents folder and are never retried.
package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

	"github.com/mikefwille/teams-web-cli/internal/cliutil"
	"github.com/spf13/cobra"
)

var teamsUploadPath = regexp.MustCompile(`(?i)^/personal/([a-z0-9_]+)/_api/web/GetFolderByServerRelativePath\(DecodedUrl=@a1\)/Files/AddUsingPath\(DecodedUrl=@a2,AutoCheckoutOnInvalidData=@a3,XorHash=@a4\)$`)
var teamsRenamePath = regexp.MustCompile(`(?i)^/personal/([a-z0-9_]+)/_api/web/GetListUsingPath\(DecodedUrl=@a1\)/items\(@a2\)$`)
var teamsDeletePath = regexp.MustCompile(`^/_api/v2\.1/drives/([A-Za-z0-9_!.-]{1,512})/items/([A-Za-z0-9_!.-]{1,512})$`)
var teamsFilesTokenListPath = regexp.MustCompile(`(?i)^/personal/[a-z0-9_]+/_api/web/lists/GetByTitle\('[a-z0-9 _.-]{1,80}'\)/items$`)

func validTeamsFilesTokenPath(path string) bool {
	decoded, err := url.PathUnescape(path)
	if err != nil || teamsFilesOrigin == "" {
		return false
	}
	host := strings.TrimPrefix(teamsFilesOrigin, "https://")
	readPath := regexp.MustCompile(`(?i)^/_api/v2\.0/sites/` + regexp.QuoteMeta(host) + `:/personal/[a-z0-9_]+$`)
	return readPath.MatchString(decoded) || teamsFilesTokenListPath.MatchString(decoded)
}

// Claims are checked only for consistency with the locally observed session;
// server acceptance, not this decoding, establishes token validity.
func teamsFilesTokenMatches(auth, audience string) bool {
	scheme, token, ok := strings.Cut(strings.TrimSpace(auth), " ")
	parts := strings.Split(strings.TrimSpace(token), ".")
	if !ok || !strings.EqualFold(scheme, "bearer") || len(parts) != 3 || audience == "" {
		return false
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Audience string `json:"aud"`
		Scope    string `json:"scp"`
	}
	if json.Unmarshal(data, &claims) != nil || claims.Audience != audience {
		return false
	}
	for _, scope := range strings.Fields(claims.Scope) {
		if strings.EqualFold(scope, "Files.ReadWrite.All") {
			return true
		}
	}
	return false
}

func loadTeamsFilesWriteScope(dir string, p teamsTokenPrincipal, operation string) (teamsAudienceScope, error) {
	if teamsFilesOrigin == "" {
		return teamsAudienceScope{}, errors.New("set M365_ONEDRIVE_ORIGIN to the account's HTTPS OneDrive origin")
	}
	var catalog teamsAudienceCatalog
	if err := readPrivateSessionFile(filepath.Join(dir, "audiences-teams.json"), &catalog); err != nil {
		return teamsAudienceScope{}, errors.New("OneDrive write evidence has not been imported")
	}
	if catalog.Version != 1 || catalog.Service != "teams" {
		return teamsAudienceScope{}, errors.New("invalid Teams audience catalog")
	}
	sum := sha256.Sum256([]byte(p.Tenant + "\x00" + p.Object))
	owner := hex.EncodeToString(sum[:])
	for _, entry := range catalog.Scopes {
		if entry.Origin != teamsFilesOrigin || entry.Operation != operation {
			continue
		}
		name := filepath.Base(entry.ExportPath)
		if !teamsScopeID.MatchString(entry.ScopeID) || entry.ExportPath != filepath.Join(dir, name) || !strings.HasPrefix(name, "authscope-teams-"+entry.ScopeID+"-") || !strings.HasSuffix(name, ".json") {
			return teamsAudienceScope{}, errors.New("invalid OneDrive write scope path")
		}
		var s teamsAudienceScope
		if err := readPrivateSessionFile(entry.ExportPath, &s); err != nil {
			return teamsAudienceScope{}, err
		}
		if s.Version != 1 || s.Service != "teams" || s.ScopeID != entry.ScopeID || s.Origin != teamsFilesOrigin || s.Operation != operation || s.OwnerScope != owner {
			return teamsAudienceScope{}, errors.New("OneDrive write scope does not match the current account")
		}
		switch operation {
		case "UploadFile", "RenameFile":
			pattern := teamsUploadPath
			if operation == "RenameFile" {
				pattern = teamsRenamePath
			}
			parts := pattern.FindStringSubmatch(s.ObservedPath)
			if len(parts) != 2 || s.ObservedQuery["@a1"] != "'/personal/"+parts[1]+"/Documents'" || s.Headers["cookie"] == "" || s.Headers["x-requestdigest"] == "" {
				return teamsAudienceScope{}, errors.New("invalid OneDrive write scope")
			}
			if operation == "UploadFile" && s.ObservedQuery["@a3"] != "true" {
				return teamsAudienceScope{}, errors.New("invalid OneDrive upload scope")
			}
			for key := range s.ObservedQuery {
				if operation == "UploadFile" && key != "@a1" && key != "@a2" && key != "@a3" && key != "@a4" && key != "$Select" && key != "$Expand" {
					return teamsAudienceScope{}, errors.New("invalid OneDrive upload query")
				}
				if operation == "RenameFile" && key != "@a1" && key != "@a2" {
					return teamsAudienceScope{}, errors.New("invalid OneDrive rename query")
				}
			}
			if operation == "RenameFile" && (s.Headers["x-http-method"] != "MERGE" || s.Headers["if-match"] != "*") {
				return teamsAudienceScope{}, errors.New("invalid OneDrive rename scope")
			}
		case "DeleteFile":
			if len(teamsDeletePath.FindStringSubmatch(s.ObservedPath)) != 3 {
				return teamsAudienceScope{}, errors.New("invalid OneDrive delete scope")
			}
			principal, ok := teamsPrincipal(s.Headers["authorization"])
			if !ok || principal.Tenant != p.Tenant || principal.Object != p.Object || !teamsFilesTokenMatches(s.Headers["authorization"], s.TokenAudience) {
				return teamsAudienceScope{}, errors.New("OneDrive delete credential does not match the account")
			}
			// A clean OneDrive page load makes an observed site read with the
			// same audience and Files.ReadWrite.All scope. Prefer its fresh
			// credential while retaining only the previously observed drive route.
			for _, freshEntry := range catalog.Scopes {
				if freshEntry.Origin != teamsFilesOrigin || freshEntry.Operation != "FilesTokenRead" {
					continue
				}
				freshName := filepath.Base(freshEntry.ExportPath)
				if !teamsScopeID.MatchString(freshEntry.ScopeID) || freshEntry.ExportPath != filepath.Join(dir, freshName) || !strings.HasPrefix(freshName, "authscope-teams-"+freshEntry.ScopeID+"-") || !strings.HasSuffix(freshName, ".json") {
					return teamsAudienceScope{}, errors.New("invalid OneDrive refresh scope path")
				}
				var fresh teamsAudienceScope
				if err := readPrivateSessionFile(freshEntry.ExportPath, &fresh); err != nil {
					return teamsAudienceScope{}, err
				}
				freshPrincipal, matched := teamsPrincipal(fresh.Headers["authorization"])
				expiry, known := accessTokenExpiry(fresh.Headers["authorization"])
				if fresh.Version != 1 || fresh.Service != "teams" || fresh.ScopeID != freshEntry.ScopeID || fresh.Origin != teamsFilesOrigin || fresh.Operation != "FilesTokenRead" || fresh.OwnerScope != owner || fresh.TokenAudience == "" || fresh.TokenAudience != s.TokenAudience || !teamsFilesTokenMatches(fresh.Headers["authorization"], fresh.TokenAudience) || !validTeamsFilesTokenPath(fresh.ObservedPath) || !matched || freshPrincipal.Tenant != p.Tenant || freshPrincipal.Object != p.Object || !known || time.Now().After(expiry) {
					return teamsAudienceScope{}, errors.New("OneDrive refreshed credential does not match the account and audience")
				}
				s.Headers["authorization"] = fresh.Headers["authorization"]
				if fresh.Headers["cookie"] != "" {
					s.Headers["cookie"] = fresh.Headers["cookie"]
				}
				break
			}
			if expiry, known := accessTokenExpiry(s.Headers["authorization"]); !known || time.Now().After(expiry) {
				return teamsAudienceScope{}, errors.New("OneDrive delete credential expired; refresh the file audience")
			}
		default:
			return teamsAudienceScope{}, errors.New("unsupported OneDrive write scope")
		}
		return s, nil
	}
	return teamsAudienceScope{}, fmt.Errorf("no observed %s OneDrive scope; import its evidence", operation)
}

type teamsFilesWriteTransport struct {
	next          http.RoundTripper
	scope         teamsAudienceScope
	operation     string
	expectedPath  string
	expectedQuery url.Values
}

// SharePoint documents contextinfo as the read-only way to obtain the
// short-lived request digest used by cookie-authenticated writes:
// https://learn.microsoft.com/en-us/sharepoint/dev/sp-add-ins/complete-basic-operations-using-sharepoint-rest-endpoints
func teamsFreshFilesDigest(ctx context.Context, next http.RoundTripper, scope teamsAudienceScope) (string, int, error) {
	parts := teamsUploadPath.FindStringSubmatch(scope.ObservedPath)
	if len(parts) != 2 {
		parts = teamsRenamePath.FindStringSubmatch(scope.ObservedPath)
	}
	if len(parts) != 2 || scope.Headers["cookie"] == "" {
		return "", 0, errors.New("invalid OneDrive digest scope")
	}
	path := "/personal/" + parts[1] + "/_api/contextinfo"
	headers := make(map[string]string, len(scope.Headers))
	for key, value := range scope.Headers {
		if key != "x-requestdigest" && key != "x-http-method" && key != "if-match" {
			headers[key] = value
		}
	}
	scope.Headers = headers
	transport := teamsFilesWriteTransport{next: next, scope: scope, operation: "DigestRead", expectedPath: path, expectedQuery: url.Values{}}
	hc := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, teamsFilesOrigin+path, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Accept", "application/json;odata=verbose")
	if err := teamsFilesLimiter.Wait(ctx); err != nil {
		return "", 0, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, errors.New("OneDrive digest request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1_000_001))
	if err != nil || len(data) > 1_000_000 {
		return "", 0, errors.New("OneDrive digest response was incomplete")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		teamsFilesLimiter.OnRateLimit()
		return "", 0, rateLimitErr(&cliutil.RateLimitError{URL: "OneDrive digest service", RetryAfter: cliutil.RetryAfter(resp)})
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, classifyAPIErrorOnly(fmt.Errorf("OneDrive digest returned HTTP %d", resp.StatusCode))
	}
	var result struct {
		D struct {
			Context struct {
				Value   string `json:"FormDigestValue"`
				Seconds int    `json:"FormDigestTimeoutSeconds"`
			} `json:"GetContextWebInformation"`
		} `json:"d"`
	}
	if json.Unmarshal(data, &result) != nil || result.D.Context.Value == "" || strings.ContainsAny(result.D.Context.Value, "\r\n") || result.D.Context.Seconds <= 0 || result.D.Context.Seconds > 86400 {
		return "", 0, errors.New("OneDrive returned an invalid request digest")
	}
	teamsFilesLimiter.OnSuccess()
	return result.D.Context.Value, result.D.Context.Seconds, nil
}

func (t teamsFilesWriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || teamsFilesOrigin == "" || req.URL.Scheme != "https" || req.URL.Host != strings.TrimPrefix(teamsFilesOrigin, "https://") || req.URL.User != nil || req.URL.Path != t.expectedPath || req.URL.RawQuery != t.expectedQuery.Encode() {
		return nil, errors.New("refusing an unobserved OneDrive write")
	}
	if t.operation == "DeleteFile" && req.Method != http.MethodDelete || t.operation != "DeleteFile" && req.Method != http.MethodPost {
		return nil, errors.New("refusing an unobserved OneDrive method")
	}
	for _, key := range []string{"cookie", "authorization", "x-requestdigest", "x-http-method", "if-match", "accept-language", "user-agent", "referer", "origin", "x-ms-cc", "application", "prefer"} {
		v := t.scope.Headers[key]
		if v != "" && v != "Bearer" && !strings.ContainsAny(v, "\r\n") {
			req.Header.Set(key, v)
		}
	}
	return t.next.RoundTrip(req)
}

func performTeamsFilesWrite(ctx context.Context, flags *rootFlags, operation, path string, q url.Values, body []byte) ([]byte, error) {
	c, err := flags.newClient()
	if err != nil {
		return nil, err
	}
	if c.Config.AuthSource != "web-session" {
		return nil, authErr(errors.New("a Teams web session is required"))
	}
	p, ok := teamsPrincipal(c.Config.AuthHeader())
	if !ok {
		return nil, authErr(errors.New("Teams identity is unavailable"))
	}
	dir, err := teamsSessionDir()
	if err != nil {
		return nil, err
	}
	scope, err := loadTeamsFilesWriteScope(dir, p, operation)
	if err != nil {
		return nil, authErr(err)
	}
	if operation == "UploadFile" || operation == "RenameFile" {
		if path != scope.ObservedPath {
			return nil, errors.New("OneDrive destination does not match captured scope")
		}
	} else {
		observed := teamsDeletePath.FindStringSubmatch(scope.ObservedPath)
		target := teamsDeletePath.FindStringSubmatch(path)
		if len(observed) != 3 || len(target) != 3 || observed[1] != target[1] {
			return nil, errors.New("OneDrive drive does not match captured scope")
		}
	}
	guard, ok := c.HTTPClient.Transport.(teamsOriginTransport)
	if !ok {
		return nil, errors.New("Teams session origin guard is unavailable")
	}
	if operation == "UploadFile" || operation == "RenameFile" {
		digest, _, err := teamsFreshFilesDigest(ctx, guard.next, scope)
		if err != nil {
			return nil, err
		}
		headers := make(map[string]string, len(scope.Headers))
		for key, value := range scope.Headers {
			headers[key] = value
		}
		headers["x-requestdigest"] = digest
		scope.Headers = headers
	}
	hc := *c.HTTPClient
	hc.Transport = teamsFilesWriteTransport{next: guard.next, scope: scope, operation: operation, expectedPath: path, expectedQuery: q}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	method := http.MethodPost
	if operation == "DeleteFile" {
		method = http.MethodDelete
	}
	req, err := http.NewRequestWithContext(ctx, method, teamsFilesOrigin+path+func() string {
		if len(q) > 0 {
			return "?" + q.Encode()
		}
		return ""
	}(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if operation == "UploadFile" {
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Accept", "application/json;odata=verbose")
	} else if operation == "RenameFile" {
		req.Header.Set("Content-Type", "application/json;odata=verbose")
		req.Header.Set("Accept", "application/json;odata=verbose")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if err := teamsFilesLimiter.Wait(ctx); err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, apiErr(errors.New("outcome_unknown: OneDrive write did not complete; inspect the folder before trying again"))
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1_000_001))
	if readErr != nil || len(data) > 1_000_000 {
		return nil, apiErr(errors.New("outcome_unknown: OneDrive write response was incomplete"))
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		teamsFilesLimiter.OnRateLimit()
		return nil, rateLimitErr(&cliutil.RateLimitError{URL: "OneDrive service", RetryAfter: cliutil.RetryAfter(resp)})
	}
	if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500 {
		return nil, apiErr(fmt.Errorf("outcome_unknown: OneDrive returned HTTP %d; inspect the folder before trying again", resp.StatusCode))
	}
	want := http.StatusNoContent
	if operation == "UploadFile" {
		want = http.StatusOK
	}
	if resp.StatusCode != want {
		if err := saveTeamsFilesDiagnostic(resp.StatusCode, data); err != nil {
			return nil, err
		}
		return nil, classifyAPIErrorOnly(fmt.Errorf("OneDrive write returned HTTP %d", resp.StatusCode))
	}
	teamsFilesLimiter.OnSuccess()
	return data, nil
}

// QuickXorHash follows Microsoft's published OneDrive hash algorithm.
func teamsQuickXor(data []byte) string {
	var out [20]byte
	for i, b := range data {
		for bit := 0; bit < 8; bit++ {
			if b&(1<<bit) != 0 {
				p := (i*11 + bit) % 160
				out[p/8] ^= 1 << uint(p%8)
			}
		}
	}
	n := uint64(len(data))
	for i := 0; i < 8; i++ {
		out[12+i] ^= byte(n >> uint(8*i))
	}
	return base64.StdEncoding.EncodeToString(out[:])
}
func teamsFileName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 && !strings.ContainsAny(name, "/\\\x00\r\n")
}
func teamsSameGUID(a, b string) bool {
	return strings.EqualFold(strings.Trim(a, "{}"), strings.Trim(b, "{}"))
}
func findTeamsFile(items []map[string]any, id string) map[string]any {
	for _, item := range items {
		if value, ok := item["id"].(string); ok && teamsSameGUID(value, id) {
			return item
		}
	}
	return nil
}
func quotedOData(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func newTeamsFileUploadCmd(flags *rootFlags) *cobra.Command {
	var file, encoded, name string
	var execute, confirm bool
	cmd := &cobra.Command{Use: "upload", Short: "Preview or upload a small file to Documents", Example: "teams-web-pp-cli files upload --content-base64 aGVsbG8= --name example.txt --json --no-learn", Args: cobra.NoArgs, Annotations: map[string]string{"mcp:read-only-unless-flags": "execute,confirm", "mcp:read-flags": "file"}, RunE: func(cmd *cobra.Command, _ []string) error {
		hasFile, hasContent := cmd.Flags().Changed("file"), cmd.Flags().Changed("content-base64")
		if hasFile == hasContent || hasFile && file == "" {
			return usageErr(errors.New("provide exactly one of --file or --content-base64"))
		}
		if name == "" && file != "" {
			name = filepath.Base(file)
		}
		if !teamsFileName(name) {
			return usageErr(errors.New("--name must be a simple file name"))
		}
		preview := map[string]any{"executed": false, "action": "upload", "name": name, "destination": "Documents"}
		if flags.dryRun || !execute {
			return flags.printJSON(cmd, preview)
		}
		if !confirm {
			return usageErr(errors.New("--execute requires --confirm"))
		}
		if cliutil.IsAnyHarness() {
			return errors.New("OneDrive writes are disabled in verification harnesses")
		}
		items, partial, truncated, err := fetchTeamsFilesList(cmd, flags, 1000)
		if err != nil {
			return err
		}
		if partial || truncated {
			return errors.New("cannot check for a duplicate name in a partial Documents listing")
		}
		for _, item := range items {
			if strings.EqualFold(fmt.Sprint(item["name"]), name) {
				return errors.New("an item with that name already exists")
			}
		}
		var data []byte
		if file != "" {
			f, err := os.Open(file)
			if err != nil {
				return err
			}
			data, err = io.ReadAll(io.LimitReader(f, 10_000_001))
			f.Close()
			if err != nil {
				return err
			}
		} else {
			data, err = base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return usageErr(errors.New("--content-base64 is invalid"))
			}
		}
		if len(data) > 10_000_000 {
			return errors.New("upload exceeds the 10 MB limit")
		}
		c, err := flags.newClient()
		if err != nil {
			return err
		}
		p, ok := teamsPrincipal(c.Config.AuthHeader())
		if c.Config.AuthSource != "web-session" || !ok {
			return authErr(errors.New("a Teams web session is required"))
		}
		dir, err := teamsSessionDir()
		if err != nil {
			return err
		}
		scope, err := loadTeamsFilesWriteScope(dir, p, "UploadFile")
		if err != nil {
			return authErr(err)
		}
		q := url.Values{}
		for k, v := range scope.ObservedQuery {
			q.Set(k, v)
		}
		q.Set("@a2", quotedOData(name))
		q.Set("@a4", quotedOData(teamsQuickXor(data)))
		ctx, cancel := boundCtx(cmd.Context(), flags)
		defer cancel()
		response, err := performTeamsFilesWrite(ctx, flags, "UploadFile", scope.ObservedPath, q, data)
		if err != nil {
			return err
		}
		var result struct {
			D struct {
				Name string `json:"Name"`
				ID   string `json:"UniqueId"`
			} `json:"d"`
		}
		if json.Unmarshal(response, &result) != nil || result.D.Name != name || !teamsFileGUID.MatchString(result.D.ID) {
			return apiErr(errors.New("outcome_unknown: OneDrive upload response could not be verified"))
		}
		return flags.printJSON(cmd, map[string]any{"executed": true, "status": "accepted", "id": result.D.ID, "name": name, "size_bytes": len(data), "verification": "read the folder to confirm the final state"})
	}}
	cmd.Flags().StringVar(&file, "file", "", "Local file path")
	cmd.Flags().StringVar(&encoded, "content-base64", "", "Base64 file content")
	cmd.Flags().StringVar(&name, "name", "", "Destination file name")
	cmd.Flags().BoolVar(&execute, "execute", false, "Perform upload")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm upload")
	return cmd
}

func configureTeamsFileChange(cmd *cobra.Command, flags *rootFlags, operation string) {
	var id, name string
	var execute, confirm bool
	cmd.Args = cobra.NoArgs
	cmd.Annotations = map[string]string{"mcp:read-only-unless-flags": "execute,confirm"}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if !teamsFileGUID.MatchString(id) {
			return usageErr(errors.New("--id must be an item GUID from files list"))
		}
		if operation == "RenameFile" && !teamsFileName(name) {
			return usageErr(errors.New("--name must be a simple file name"))
		}
		if flags.dryRun {
			return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": operation, "id": id})
		}
		items, partial, truncated, err := fetchTeamsFilesList(cmd, flags, 1000)
		if err != nil {
			return err
		}
		item := findTeamsFile(items, id)
		if item == nil {
			return errors.New("item is not in the current Documents page")
		}
		if item["item_type_code"] != "0" {
			return errors.New("this operation supports files only")
		}
		if operation == "RenameFile" {
			if partial || truncated {
				return errors.New("cannot check for a duplicate name in a partial Documents listing")
			}
			for _, other := range items {
				if !teamsSameGUID(fmt.Sprint(other["id"]), id) && strings.EqualFold(fmt.Sprint(other["name"]), name) {
					return errors.New("an item with that name already exists")
				}
			}
		}
		preview := map[string]any{"executed": false, "action": operation, "current": item}
		if operation == "RenameFile" {
			preview["name"] = name
		}
		if !execute {
			return flags.printJSON(cmd, preview)
		}
		if !confirm {
			return usageErr(errors.New("--execute requires --confirm"))
		}
		if cliutil.IsAnyHarness() {
			return errors.New("OneDrive writes are disabled in verification harnesses")
		}
		c, err := flags.newClient()
		if err != nil {
			return err
		}
		p, ok := teamsPrincipal(c.Config.AuthHeader())
		if c.Config.AuthSource != "web-session" || !ok {
			return authErr(errors.New("a Teams web session is required"))
		}
		dir, err := teamsSessionDir()
		if err != nil {
			return err
		}
		scope, err := loadTeamsFilesWriteScope(dir, p, operation)
		if err != nil {
			return authErr(err)
		}
		var target string
		q := url.Values{}
		var body []byte
		if operation == "RenameFile" {
			num, ok := item["list_item_id"].(string)
			if !ok || !teamsFileNumericID.MatchString(num) {
				return errors.New("listing did not include a usable list item ID")
			}
			target = scope.ObservedPath
			for k, v := range scope.ObservedQuery {
				q.Set(k, v)
			}
			q.Set("@a2", quotedOData(num))
			body, err = json.Marshal(map[string]any{"__metadata": map[string]string{"type": "SP.Data.DocumentsItem"}, "FileLeafRef": name})
			if err != nil {
				return err
			}
		} else {
			drive, ok1 := item["drive_id"].(string)
			itemID, ok2 := item["item_id"].(string)
			observed := teamsDeletePath.FindStringSubmatch(scope.ObservedPath)
			if !ok1 || !ok2 || len(observed) != 3 || drive != observed[1] {
				return errors.New("listing item is not in the observed OneDrive")
			}
			target = "/_api/v2.1/drives/" + drive + "/items/" + itemID
		}
		ctx, cancel := boundCtx(cmd.Context(), flags)
		defer cancel()
		if _, err := performTeamsFilesWrite(ctx, flags, operation, target, q, body); err != nil {
			return err
		}
		return flags.printJSON(cmd, map[string]any{"executed": true, "status": "accepted", "action": operation, "id": id, "verification": "read the folder to confirm the final state"})
	}
	cmd.Flags().StringVar(&id, "id", "", "OneDrive item GUID")
	if operation == "RenameFile" {
		cmd.Flags().StringVar(&name, "name", "", "New file name")
	}
	cmd.Flags().BoolVar(&execute, "execute", false, "Perform the change")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm the change")
}
func newTeamsFileRenameCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "rename", Short: "Preview or rename an owned file"}
	configureTeamsFileChange(cmd, flags, "RenameFile")
	return cmd
}
func newTeamsFilesDigestCheckCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{Use: "digest-check", Short: "Check whether a fresh OneDrive request digest can be obtained", Example: "teams-web-pp-cli files digest-check --json --no-learn", Args: cobra.NoArgs, Annotations: map[string]string{"mcp:read-only": "true", "pp:data-source": "live"}, RunE: func(cmd *cobra.Command, _ []string) error {
		if dryRunOK(flags) {
			return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "files digest-check"})
		}
		c, err := flags.newClient()
		if err != nil {
			return err
		}
		if c.Config.AuthSource != "web-session" {
			return authErr(errors.New("a Teams web session is required"))
		}
		p, ok := teamsPrincipal(c.Config.AuthHeader())
		if !ok {
			return authErr(errors.New("Teams identity is unavailable"))
		}
		dir, err := teamsSessionDir()
		if err != nil {
			return err
		}
		scope, err := loadTeamsFilesWriteScope(dir, p, "UploadFile")
		if err != nil {
			return authErr(err)
		}
		guard, ok := c.HTTPClient.Transport.(teamsOriginTransport)
		if !ok {
			return errors.New("Teams session origin guard is unavailable")
		}
		ctx, cancel := boundCtx(cmd.Context(), flags)
		defer cancel()
		_, seconds, err := teamsFreshFilesDigest(ctx, guard.next, scope)
		if err != nil {
			return err
		}
		return flags.printJSON(cmd, map[string]any{"ok": true, "source": "onedrive_contextinfo", "valid_for_seconds": seconds})
	}}
}
