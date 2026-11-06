// Owned OneDrive reads use the captured Microsoft 365 app-launcher session.
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
	"strconv"
	"strings"

	"github.com/mikefwille/teams-web-cli/internal/cliutil"
	"github.com/spf13/cobra"
)

var teamsFilesOrigin = configuredTeamsFilesOrigin(os.Getenv("M365_ONEDRIVE_ORIGIN"))

// OneDrive credentials are sent only to the explicitly configured personal
// SharePoint origin. An absent or invalid value leaves file operations disabled.
func configuredTeamsFilesOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Port() != "" && u.Port() != "443") {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,61}-my\.sharepoint\.com$`).MatchString(host) {
		return ""
	}
	return "https://" + host
}

var teamsFilesPath = regexp.MustCompile(`(?i)^/personal/([a-z0-9_]+)/_api/web/GetListUsingPath\(DecodedUrl=@a1\)/RenderListDataAsStream$`)
var teamsFileGUID = regexp.MustCompile(`(?i)^(?:\{[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
var teamsFileItemURL = regexp.MustCompile(`^/_api/v2\.0/drives/([A-Za-z0-9_!.-]{1,512})/items/([A-Za-z0-9_!.-]{1,512})$`)
var teamsFileNumericID = regexp.MustCompile(`^\d{1,12}$`)
var teamsFilesLimiter = cliutil.NewAdaptiveLimiterAuto(1)
var teamsFilesDiagnosticName = regexp.MustCompile(`^teams-files-diag-[a-f0-9]{16}\.json$`)

func saveTeamsFilesDiagnostic(status int, body []byte) error {
	name := os.Getenv("TEAMS_WEB_FILES_DIAGNOSTIC")
	if name == "" {
		return nil
	}
	dir, err := teamsSessionDir()
	if err != nil || !filepath.IsAbs(name) || filepath.Dir(name) != filepath.Clean(dir) || !teamsFilesDiagnosticName.MatchString(filepath.Base(name)) {
		return errors.New("invalid private files diagnostic path")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("unsafe private files diagnostic directory")
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("could not create files diagnostic")
	}
	defer f.Close()
	encoded, err := json.Marshal(map[string]any{"http_status": status, "response_body": string(body)})
	if err != nil {
		return err
	}
	if _, err = f.Write(encoded); err != nil {
		return err
	}
	return f.Sync()
}

func loadTeamsFilesScope(dir string, principal teamsTokenPrincipal) (teamsAudienceScope, error) {
	if teamsFilesOrigin == "" {
		return teamsAudienceScope{}, errors.New("set M365_ONEDRIVE_ORIGIN to the account's HTTPS OneDrive origin")
	}
	var catalog teamsAudienceCatalog
	if err := readPrivateSessionFile(filepath.Join(dir, "audiences-teams.json"), &catalog); err != nil {
		return teamsAudienceScope{}, errors.New("no captured OneDrive listing; import the observed listing")
	}
	if catalog.Version != 1 || catalog.Service != "teams" {
		return teamsAudienceScope{}, errors.New("invalid Teams audience catalog")
	}
	owner := sha256.Sum256([]byte(principal.Tenant + "\x00" + principal.Object))
	expectedOwner := hex.EncodeToString(owner[:])
	for _, entry := range catalog.Scopes {
		if entry.Origin != teamsFilesOrigin || entry.Operation != "ListFiles" {
			continue
		}
		name := filepath.Base(entry.ExportPath)
		if !teamsScopeID.MatchString(entry.ScopeID) || entry.ExportPath != filepath.Join(dir, name) || !strings.HasPrefix(name, "authscope-teams-"+entry.ScopeID+"-") || !strings.HasSuffix(name, ".json") {
			return teamsAudienceScope{}, errors.New("invalid OneDrive scope path")
		}
		var scope teamsAudienceScope
		if err := readPrivateSessionFile(entry.ExportPath, &scope); err != nil {
			return teamsAudienceScope{}, err
		}
		match := teamsFilesPath.FindStringSubmatch(scope.ObservedPath)
		q := scope.ObservedQuery
		if scope.Version != 1 || scope.Service != "teams" || scope.ScopeID != entry.ScopeID || scope.Origin != teamsFilesOrigin || scope.Operation != "ListFiles" || scope.OwnerScope != expectedOwner || len(match) != 2 || q["@a1"] != "'/personal/"+match[1]+"/Documents'" || q["TryNewExperienceSingle"] != "TRUE" || len(q) < 2 || len(q) > 3 || scope.Headers["cookie"] == "" {
			return teamsAudienceScope{}, errors.New("OneDrive scope does not match the current account")
		}
		for k := range q {
			if k != "@a1" && k != "TryNewExperienceSingle" && k != "View" {
				return teamsAudienceScope{}, errors.New("invalid OneDrive listing query")
			}
		}
		var body struct {
			Parameters struct {
				Metadata struct {
					Type string `json:"type"`
				} `json:"__metadata"`
				RenderOptions int64 `json:"RenderOptions"`
				ViewXML       any   `json:"ViewXml"`
			} `json:"parameters"`
		}
		if json.Unmarshal(scope.RequestBody, &body) != nil || body.Parameters.Metadata.Type != "SP.RenderListDataParameters" || body.Parameters.RenderOptions <= 0 || body.Parameters.ViewXML != nil {
			return teamsAudienceScope{}, errors.New("invalid OneDrive listing request")
		}
		return scope, nil
	}
	return teamsAudienceScope{}, errors.New("no captured OneDrive listing; import the observed listing")
}

type teamsFilesTransport struct {
	next     http.RoundTripper
	scope    teamsAudienceScope
	download bool
}

func (t teamsFilesTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || teamsFilesOrigin == "" || req.URL.Scheme != "https" || req.URL.Host != strings.TrimPrefix(teamsFilesOrigin, "https://") || req.URL.User != nil {
		return nil, errors.New("refusing to send OneDrive credentials to another origin")
	}
	match := teamsFilesPath.FindStringSubmatch(t.scope.ObservedPath)
	if len(match) != 2 {
		return nil, errors.New("invalid OneDrive scope")
	}
	if t.download {
		if req.Method != http.MethodGet || req.URL.Path != "/personal/"+match[1]+"/_layouts/15/download.aspx" || len(req.URL.Query()) != 1 || len(req.URL.Query()["UniqueId"]) != 1 || !teamsFileGUID.MatchString(req.URL.Query().Get("UniqueId")) {
			return nil, errors.New("refusing unobserved OneDrive download")
		}
	} else {
		if req.Method != http.MethodPost || req.URL.Path != t.scope.ObservedPath || len(req.URL.Query()) != len(t.scope.ObservedQuery) {
			return nil, errors.New("refusing unobserved OneDrive listing")
		}
		for key, value := range t.scope.ObservedQuery {
			if values := req.URL.Query()[key]; len(values) != 1 || values[0] != value {
				return nil, errors.New("refusing unobserved OneDrive listing query")
			}
		}
	}
	for _, key := range []string{"cookie", "accept-language", "user-agent", "referer", "origin", "x-ms-cc", "application", "prefer", "scenariotype", "scenario", "collectspperfmetrics", "x-sp-requestresources"} {
		if value := t.scope.Headers[key]; value != "" && !strings.ContainsAny(value, "\r\n") {
			req.Header.Set(key, value)
		}
	}
	return t.next.RoundTrip(req)
}

func teamsFilesClient(flags *rootFlags) (teamsAudienceScope, *http.Client, error) {
	c, err := flags.newClient()
	if err != nil {
		return teamsAudienceScope{}, nil, err
	}
	if c.Config.AuthSource != "web-session" {
		return teamsAudienceScope{}, nil, authErr(errors.New("a Teams web session is required"))
	}
	principal, ok := teamsPrincipal(c.Config.AuthHeader())
	if !ok {
		return teamsAudienceScope{}, nil, authErr(errors.New("Teams session identity is unavailable"))
	}
	dir, err := teamsSessionDir()
	if err != nil {
		return teamsAudienceScope{}, nil, err
	}
	scope, err := loadTeamsFilesScope(dir, principal)
	if err != nil {
		return teamsAudienceScope{}, nil, authErr(err)
	}
	guard, ok := c.HTTPClient.Transport.(teamsOriginTransport)
	if !ok {
		return teamsAudienceScope{}, nil, errors.New("Teams session origin guard is unavailable")
	}
	hc := *c.HTTPClient
	hc.Transport = teamsFilesTransport{next: guard.next, scope: scope}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return scope, &hc, nil
}

func runTeamsFilesRequest(ctx context.Context, hc *http.Client, req *http.Request) (*http.Response, error) {
	if err := teamsFilesLimiter.Wait(ctx); err != nil {
		return nil, err
	}
	resp, err := hc.Do(req.WithContext(ctx))
	if err != nil {
		return nil, errors.New("OneDrive request failed")
	}
	if remaining, reset, ok := cliutil.ParseRateLimitHeaders(resp.Header); ok {
		teamsFilesLimiter.ObserveHeaders(remaining, reset)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		resp.Body.Close()
		teamsFilesLimiter.OnRateLimit()
		return nil, rateLimitErr(&cliutil.RateLimitError{URL: "OneDrive service", RetryAfter: cliutil.RetryAfter(resp)})
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
		resp.Body.Close()
		if err := saveTeamsFilesDiagnostic(resp.StatusCode, data); err != nil {
			return nil, err
		}
		return nil, classifyAPIErrorOnly(fmt.Errorf("OneDrive returned HTTP %d", resp.StatusCode))
	}
	teamsFilesLimiter.OnSuccess()
	return resp, nil
}

func decodeTeamsFiles(data []byte, limit int) ([]map[string]any, bool, bool, error) {
	var body struct {
		ListData struct {
			Row      []map[string]any `json:"Row"`
			RowLimit int              `json:"RowLimit"`
			NextHref string           `json:"NextHref"`
		} `json:"ListData"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &body) != nil || body.ListData.Row == nil || len(bytes.TrimSpace(body.Error)) > 0 && !bytes.Equal(bytes.TrimSpace(body.Error), []byte("null")) {
		return nil, false, false, errors.New("OneDrive returned an unexpected listing")
	}
	rows := body.ListData.Row
	out := make([]map[string]any, 0, min(len(rows), limit))
	for _, row := range rows {
		if len(out) >= limit {
			break
		}
		item := map[string]any{}
		for source, target := range map[string]string{"UniqueId": "id", "FileLeafRef": "name", "FileRef": "path", "FSObjType": "item_type_code", "Modified": "modified_display", "File_x0020_Type": "extension"} {
			if v, ok := row[source].(string); ok && v != "" {
				item[target] = v
			}
		}
		if value, ok := row["ID"].(string); ok && teamsFileNumericID.MatchString(value) {
			item["list_item_id"] = value
		}
		if value, ok := row[".spItemUrl"].(string); ok {
			if u, err := url.Parse(value); err == nil && teamsFilesOrigin != "" && u.Scheme == "https" && strings.EqualFold(u.Hostname(), strings.TrimPrefix(teamsFilesOrigin, "https://")) && (u.Port() == "" || u.Port() == "443") && u.User == nil {
				if parts := teamsFileItemURL.FindStringSubmatch(u.Path); len(parts) == 3 {
					item["drive_id"], item["item_id"] = parts[1], parts[2]
				}
			}
		}
		if raw, ok := row["File_x0020_Size"].(string); ok && raw != "" {
			if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
				item["size_bytes"] = n
			}
		}
		if id, ok := item["id"].(string); !ok || !teamsFileGUID.MatchString(id) {
			return nil, false, false, errors.New("OneDrive listing contains an invalid item ID")
		}
		out = append(out, item)
	}
	partial := body.ListData.NextHref != "" || body.ListData.RowLimit <= 0 || len(rows) >= body.ListData.RowLimit
	return out, partial, len(rows) > limit, nil
}

func fetchTeamsFilesList(cmd *cobra.Command, flags *rootFlags, limit int) ([]map[string]any, bool, bool, error) {
	scope, hc, err := teamsFilesClient(flags)
	if err != nil {
		return nil, false, false, err
	}
	q := url.Values{}
	for k, v := range scope.ObservedQuery {
		q.Set(k, v)
	}
	req, err := http.NewRequest(http.MethodPost, teamsFilesOrigin+scope.ObservedPath+"?"+q.Encode(), bytes.NewReader(scope.RequestBody))
	if err != nil {
		return nil, false, false, err
	}
	if scope.Headers["accept"] != "application/json;odata=verbose" || scope.Headers["content-type"] != "application/json;odata=verbose" {
		return nil, false, false, errors.New("captured OneDrive media type is unsupported")
	}
	req.Header.Set("Accept", scope.Headers["accept"])
	req.Header.Set("Content-Type", scope.Headers["content-type"])
	ctx, cancel := boundCtx(cmd.Context(), flags)
	defer cancel()
	resp, err := runTeamsFilesRequest(ctx, hc, req)
	if err != nil {
		return nil, false, false, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2_000_001))
	if err != nil || len(data) > 2_000_000 {
		return nil, false, false, errors.New("OneDrive listing exceeded the response limit")
	}
	items, partial, truncated, err := decodeTeamsFiles(data, limit)
	return items, partial, truncated, err
}

func newTeamsFilesListCmd(flags *rootFlags) *cobra.Command {
	var limit int
	cmd := &cobra.Command{Use: "list", Short: "List the captured OneDrive Documents folder", Args: cobra.NoArgs, Annotations: map[string]string{"mcp:read-only": "true", "pp:data-source": "live"}, RunE: func(cmd *cobra.Command, _ []string) error {
		if limit < 1 || limit > 1000 {
			return usageErr(errors.New("--limit must be between 1 and 1000"))
		}
		if dryRunOK(flags) {
			return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "files list"})
		}
		items, partial, truncated, err := fetchTeamsFilesList(cmd, flags, limit)
		if err != nil {
			return err
		}
		return flags.printJSON(cmd, map[string]any{"results": items, "count": len(items), "source": "onedrive_documents", "partial": partial, "truncated": truncated})
	}}
	cmd.Flags().IntVar(&limit, "limit", 100, "Maximum returned entries")
	return cmd
}

func teamsFileResponse(cmd *cobra.Command, flags *rootFlags, id string) (*http.Response, context.CancelFunc, error) {
	if !teamsFileGUID.MatchString(id) {
		return nil, nil, usageErr(errors.New("--id must be a OneDrive item GUID from files list"))
	}
	scope, hc, err := teamsFilesClient(flags)
	if err != nil {
		return nil, nil, err
	}
	match := teamsFilesPath.FindStringSubmatch(scope.ObservedPath)
	if len(match) != 2 {
		return nil, nil, errors.New("invalid OneDrive scope")
	}
	guard := hc.Transport.(teamsFilesTransport)
	guard.download = true
	hc.Transport = guard
	q := url.Values{"UniqueId": []string{id}}
	req, err := http.NewRequest(http.MethodGet, teamsFilesOrigin+"/personal/"+match[1]+"/_layouts/15/download.aspx?"+q.Encode(), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "*/*")
	ctx, cancel := boundCtx(cmd.Context(), flags)
	resp, err := runTeamsFilesRequest(ctx, hc, req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Disposition")), "attachment") {
		resp.Body.Close()
		cancel()
		return nil, nil, errors.New("OneDrive did not return an attachment response")
	}
	return resp, cancel, nil
}

func newTeamsFileReadCmd(flags *rootFlags) *cobra.Command {
	var id string
	cmd := &cobra.Command{Use: "read", Short: "Read a small OneDrive file as base64", Args: cobra.NoArgs, Annotations: map[string]string{"mcp:read-only": "true", "pp:data-source": "live"}, RunE: func(cmd *cobra.Command, _ []string) error {
		if !teamsFileGUID.MatchString(id) {
			return usageErr(errors.New("--id must be a OneDrive item GUID from files list"))
		}
		if dryRunOK(flags) {
			return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "files read", "id": id})
		}
		resp, cancel, err := teamsFileResponse(cmd, flags, id)
		if err != nil {
			return err
		}
		defer cancel()
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 2_000_001))
		if err != nil || len(data) > 2_000_000 {
			return errors.New("file exceeds the 2 MB inline limit; use files download")
		}
		return flags.printJSON(cmd, map[string]any{"id": id, "content_base64": base64.StdEncoding.EncodeToString(data), "size_bytes": len(data), "source": "onedrive_download"})
	}}
	cmd.Flags().StringVar(&id, "id", "", "OneDrive item GUID")
	return cmd
}

func newTeamsFileDownloadCmd(flags *rootFlags) *cobra.Command {
	var id, destination string
	cmd := &cobra.Command{Use: "download", Short: "Download a OneDrive file to a new local path", Args: cobra.NoArgs, Annotations: map[string]string{"mcp:hidden": "true", "pp:data-source": "live"}, RunE: func(cmd *cobra.Command, _ []string) error {
		if !teamsFileGUID.MatchString(id) || destination == "" {
			return usageErr(errors.New("--id and --output are required"))
		}
		if dryRunOK(flags) {
			return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "files download", "id": id, "output": destination})
		}
		resp, cancel, err := teamsFileResponse(cmd, flags, id)
		if err != nil {
			return err
		}
		defer cancel()
		defer resp.Body.Close()
		tmp, err := os.CreateTemp(filepath.Dir(destination), ".teams-download-*.tmp")
		if err != nil {
			return err
		}
		name := tmp.Name()
		defer os.Remove(name)
		if err := tmp.Chmod(0600); err != nil {
			tmp.Close()
			return err
		}
		n, copyErr := io.Copy(tmp, io.LimitReader(resp.Body, 50_000_001))
		syncErr := tmp.Sync()
		closeErr := tmp.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil || n > 50_000_000 {
			return errors.New("OneDrive download failed or exceeded 50 MB")
		}
		if err := os.Link(name, destination); err != nil {
			return fmt.Errorf("creating destination without overwriting: %w", err)
		}
		return flags.printJSON(cmd, map[string]any{"id": id, "output": destination, "size_bytes": n, "source": "onedrive_download"})
	}}
	cmd.Flags().StringVar(&id, "id", "", "OneDrive item GUID")
	cmd.Flags().StringVar(&destination, "output", "", "New local output path (existing files are not overwritten)")
	return cmd
}
func newTeamsFilesCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "files", Short: "Read owned OneDrive files", Annotations: map[string]string{"pp:parent-group": "true"}, RunE: parentNoSubcommandRunE(flags)}
	cmd.AddCommand(newTeamsFilesListCmd(flags), newTeamsFileReadCmd(flags), newTeamsFileDownloadCmd(flags), newTeamsFileUploadCmd(flags), newTeamsFileRenameCmd(flags), newTeamsFileDeleteCmd(flags), newTeamsFilesDigestCheckCmd(flags))
	return cmd
}
func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) { addNovelCommandIfAbsent(root, newTeamsFilesCmd(flags)) })
}
