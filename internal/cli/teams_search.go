// Scoped message search using an observed Teams web search request.
package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
)

const teamsSearchOrigin = "https://substrate.office.com"
const teamsSearchPath = "/searchservice/api/v2/query"

var teamsSearchTerm = regexp.MustCompile(`^[\pL\pN_-]{1,128}$`)
var teamsSearchConversation = regexp.MustCompile(`^[A-Za-z0-9:@._-]{1,512}$`)
var teamsSearchLimiter = cliutil.NewAdaptiveLimiterAuto(1)

var teamsSearchFields = []string{
	"Extension_SkypeSpaces_ConversationPost_Extension_FromSkypeInternalId_String",
	"Extension_SkypeSpaces_ConversationPost_Extension_FileData_String",
	"Extension_SkypeSpaces_ConversationPost_Extension_ThreadType_String",
	"GroupId",
	"Extension_SkypeSpaces_ConversationPost_Extension_SenderTenantId_String",
	"Extension_SkypeSpaces_ConversationPost_Extension_ParentMessageId_String",
	"Extension_SkypeSpaces_ConversationPost_Extension_ConversationType_String",
	"Extension_SkypeSpaces_ConversationPost_Extension_Topic_String",
	"Extension_SkypeSpaces_ConversationPost_Extension_ImageSrc_String",
	"Extension_SkypeSpaces_ConversationPost_Extension_ParentMessageId_String",
	"Extension_SkypeSpaces_ConversationPost_Extension_MessageSubType_String",
}
var teamsGlobalSearchFields = []string{
	"Extension_SkypeSpaces_ConversationPost_Extension_FromSkypeInternalId_String",
	"Extension_SkypeSpaces_ConversationPost_Extension_FileData_String",
	"Extension_SkypeSpaces_ConversationPost_Extension_ThreadType_String",
	"Extension_SkypeSpaces_ConversationPost_Extension_SkypeGroupId_String",
	"Extension_SkypeSpaces_ConversationPost_Extension_SenderTenantId_String",
	"Extension_SkypeSpaces_ConversationPost_Extension_ImageSrc_String",
	"Extension_SkypeSpaces_ConversationPost_Extension_ParentMessageId_String",
	"Extension_SkypeSpaces_ConversationPost_Extension_ImageSrc_String",
	"Extension_SkypeSpaces_ConversationPost_Extension_AmsReferences_StringArray",
}

func teamsSearchUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func teamsSearchPayload(conversation, term, zone string, offset, size int) (map[string]any, error) {
	if conversation != "" && !teamsSearchConversation.MatchString(conversation) {
		return nil, usageErr(errors.New("--conversation must be a supported opaque chat ID"))
	}
	terms, err := teamsSearchTerms(term)
	if err != nil || len(terms) != 1 || terms[0] != term {
		return nil, usageErr(errors.New("each live search request must contain one literal word"))
	}
	if offset < 0 || offset > 10000 || size < 1 || size > 25 {
		return nil, usageErr(errors.New("--offset must be 0–10000 and --limit must be 1–25"))
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return nil, usageErr(errors.New("--timezone must be a valid IANA time zone"))
	}
	cvid, err := teamsSearchUUID()
	if err != nil {
		return nil, err
	}
	logical, err := teamsSearchUUID()
	if err != nil {
		return nil, err
	}
	query := "NOT (isClientSoftDeleted:TRUE) " + term
	fields, queryType, formFactor := teamsGlobalSearchFields, "Messages", "general.web.reactSearch"
	request := map[string]any{"entityType": "Message", "contentSources": []string{"Teams"}, "query": map[string]string{"queryString": query, "displayQueryString": term}, "propertySet": "Optimized", "from": offset, "size": size}
	if conversation != "" {
		query = "clientthreadid:" + conversation + " AND NOT (Extension_SkypeSpaces_ConversationPost_Extension_ThreadType_String:(topic OR space)) AND NOT (isClientSoftDeleted:TRUE) " + term
		fields, queryType, formFactor = teamsSearchFields, "ContextualTypeahead", "general.cdlworker.reactSearch"
		request["query"] = map[string]string{"queryString": query, "displayQueryString": term}
		request["filter"] = map[string]any{"term": map[string]string{"conversationType": "OneToOne"}}
	} else {
		top := 5
		if size < top {
			top = size
		}
		request["topResultsCount"] = top
	}
	request["fields"] = fields
	return map[string]any{
		"entityRequests": []any{request},
		"cvid":           cvid, "logicalId": logical, "QueryAlterationOptions": map[string]any{"EnableAlteration": true, "EnableSuggestion": true, "SupportedRecourseDisplayTypes": []string{"Suggestion"}},
		"scenario": map[string]any{"Dimensions": []any{map[string]string{"DimensionName": "QueryType", "DimensionValue": queryType}, map[string]string{"DimensionName": "FormFactor", "DimensionValue": formFactor}}, "Name": "powerbar"}, "timezone": zone,
	}, nil
}

func loadTeamsSearchHeaders(dir string, primary teamsTokenPrincipal) (map[string]string, error) {
	var catalog teamsAudienceCatalog
	if err := readPrivateSessionFile(filepath.Join(dir, "audiences-teams.json"), &catalog); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("no captured Teams search audience; import a successful search observation")
		}
		return nil, err
	}
	if catalog.Version != 1 || catalog.Service != "teams" {
		return nil, errors.New("invalid Teams audience catalog")
	}
	for _, entry := range catalog.Scopes {
		if entry.Origin != teamsSearchOrigin {
			continue
		}
		name := filepath.Base(entry.ExportPath)
		if !teamsScopeID.MatchString(entry.ScopeID) || entry.ExportPath != filepath.Join(dir, name) || !strings.HasPrefix(name, "authscope-teams-"+entry.ScopeID+"-") || !strings.HasSuffix(name, ".json") {
			return nil, errors.New("invalid search audience path")
		}
		var scope teamsAudienceScope
		if err := readPrivateSessionFile(entry.ExportPath, &scope); err != nil {
			return nil, err
		}
		principal, ok := teamsPrincipal(scope.Headers["authorization"])
		if scope.Version != 1 || scope.Service != "teams" || scope.ScopeID != entry.ScopeID || scope.Origin != teamsSearchOrigin || scope.ObservedPath != teamsSearchPath || !strings.HasPrefix(strings.ToLower(scope.Headers["authorization"]), "bearer ") || !ok || principal.Tenant != primary.Tenant || principal.Object != primary.Object {
			return nil, errors.New("search audience does not match the current Teams account")
		}
		if exp, known := accessTokenExpiry(scope.Headers["authorization"]); !known || time.Now().After(exp) {
			return nil, errors.New("Teams search audience expired; capture and import a fresh observation")
		}
		return scope.Headers, nil
	}
	return nil, errors.New("no captured Teams search audience; import a successful search observation")
}

type teamsSearchTransport struct {
	next    http.RoundTripper
	headers map[string]string
}

func (t teamsSearchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, "substrate.office.com") || req.URL.User != nil || req.URL.Path != teamsSearchPath || req.URL.RawQuery != "" || req.Method != http.MethodPost {
		return nil, errors.New("refusing to send search credentials to an unobserved endpoint")
	}
	omit := map[string]bool{"host": true, "connection": true, "content-length": true, "accept-encoding": true, "transfer-encoding": true, "proxy-authorization": true, "proxy-connection": true, "upgrade": true, "te": true, "trailer": true, "content-type": true}
	for key, value := range t.headers {
		if !omit[strings.ToLower(key)] && !strings.HasPrefix(key, ":") && !strings.ContainsAny(value, "\r\n") {
			req.Header.Set(key, value)
		}
	}
	return t.next.RoundTrip(req)
}

type teamsSearchResponse struct {
	EntitySets []struct {
		EntityType string `json:"EntityType"`
		IsPartial  bool   `json:"IsPartial"`
		ResultSets []struct {
			Results []map[string]any `json:"Results"`
			Total   int              `json:"Total"`
			More    bool             `json:"MoreResultsAvailable"`
		} `json:"ResultSets"`
	} `json:"EntitySets"`
}

func decodeTeamsSearch(data []byte, conversation string) ([]map[string]any, int, bool, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || root == nil {
		return nil, 0, false, errors.New("Teams returned invalid search JSON")
	}
	if raw := bytes.TrimSpace(root["error"]); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		return nil, 0, false, errors.New("Teams reported a search error")
	}
	if _, ok := root["EntitySets"]; !ok {
		return nil, 0, false, errors.New("Teams returned an unexpected search envelope")
	}
	var decoded teamsSearchResponse
	if json.Unmarshal(data, &decoded) != nil || decoded.EntitySets == nil {
		return nil, 0, false, errors.New("Teams returned an unexpected search envelope")
	}
	results := []map[string]any{}
	total := 0
	partial := false
	for _, entity := range decoded.EntitySets {
		if entity.EntityType != "Message" {
			continue
		}
		partial = partial || entity.IsPartial
		for _, set := range entity.ResultSets {
			total += set.Total
			partial = partial || set.More
			for _, hit := range set.Results {
				source, ok := hit["Source"].(map[string]any)
				if !ok {
					return nil, 0, false, errors.New("Teams search result has no scoped source")
				}
				thread, _ := source["ClientThreadId"].(string)
				if conversation != "" && thread != conversation {
					return nil, 0, false, errors.New("Teams search returned a result outside the requested chat")
				}
				results = append(results, hit)
			}
		}
	}
	return results, total, partial, nil
}

func runTeamsSearch(ctx context.Context, client *http.Client, body any) ([]byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, teamsSearchOrigin+teamsSearchPath, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if err := teamsSearchLimiter.Wait(ctx); err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("Teams search request failed")
	}
	defer resp.Body.Close()
	if remaining, reset, ok := cliutil.ParseRateLimitHeaders(resp.Header); ok {
		teamsSearchLimiter.ObserveHeaders(remaining, reset)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2_000_001))
	if err != nil || len(data) > 2_000_000 {
		return nil, errors.New("Teams search response could not be read within the limit")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		teamsSearchLimiter.OnRateLimit()
		return nil, rateLimitErr(&cliutil.RateLimitError{URL: "Teams search service", RetryAfter: cliutil.RetryAfter(resp)})
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifyAPIErrorOnly(fmt.Errorf("Teams search returned HTTP %d", resp.StatusCode))
	}
	teamsSearchLimiter.OnSuccess()
	return data, nil
}

// pp:data-source auto
func newTeamsSearchMessagesCmd(flags *rootFlags) *cobra.Command {
	var conversation, term, zone, from, since, until string
	var offset, limit int
	var local, hasAttachments bool
	cmd := &cobra.Command{Use: "messages", Short: "Search Teams messages live or in the local mirror", Annotations: map[string]string{"mcp:read-only": "true", "pp:data-source": "auto"}, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if local || flags.dataSource == "local" {
				if local && flags.dataSource == "live" && cmd.Flags().Changed("data-source") {
					return usageErr(errors.New("--local conflicts with --data-source live"))
				}
				if cmd.Flags().Changed("offset") || cmd.Flags().Changed("timezone") || cmd.Flags().Changed("from") || cmd.Flags().Changed("since") || cmd.Flags().Changed("until") || cmd.Flags().Changed("has-attachments") {
					return usageErr(errors.New("--offset, --timezone, --from, --since, --until, and --has-attachments apply only to live search"))
				}
				return searchTeamsMirror(cmd, flags, term, conversation, limit)
			}
			terms, err := teamsSearchTerms(term)
			if err != nil {
				return err
			}
			if len(terms) > 1 && offset != 0 {
				return usageErr(errors.New("--offset is unavailable for multi-word live search"))
			}
			if limit < 1 || limit > 25 {
				return usageErr(errors.New("--limit must be 1–25 for live search"))
			}
			var attachmentFilter *bool
			if cmd.Flags().Changed("has-attachments") {
				attachmentFilter = &hasAttachments
			}
			filters, err := parseTeamsSearchFilters(from, since, until, attachmentFilter)
			if err != nil {
				return err
			}
			pageSize := limit
			if len(terms) > 1 {
				pageSize = 25
			}
			bodies := make([]map[string]any, 0, len(terms))
			for _, word := range terms {
				body, err := teamsSearchPayload(conversation, word, zone, offset, pageSize)
				if err != nil {
					return err
				}
				bodies = append(bodies, body)
			}
			if dryRunOK(flags) {
				return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "search messages", "conversation": conversation, "query": term, "term_requests": len(terms), "filters_local": filters.active()})
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
			headers, err := loadTeamsSearchHeaders(dir, principal)
			if err != nil {
				return authErr(err)
			}
			guard, ok := c.HTTPClient.Transport.(teamsOriginTransport)
			if !ok {
				return errors.New("Teams session origin guard is unavailable")
			}
			hc := *c.HTTPClient
			hc.Transport = teamsSearchTransport{next: guard.next, headers: headers}
			hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			pages := make([][]map[string]any, 0, len(bodies))
			pageInfo := make([]map[string]any, 0, len(bodies))
			partial := false
			serverTotal := 0
			for _, body := range bodies {
				data, err := runTeamsSearch(ctx, &hc, body)
				if err != nil {
					return err
				}
				results, total, pagePartial, err := decodeTeamsSearch(data, conversation)
				if err != nil {
					return err
				}
				pages = append(pages, results)
				pageInfo = append(pageInfo, map[string]any{"count": len(results), "server_total": total, "server_partial": pagePartial})
				partial = partial || pagePartial
				serverTotal = total
			}
			results := pages[0]
			identityUnknown := 0
			if len(pages) > 1 {
				results, identityUnknown = intersectTeamsSearchPages(pages)
				// Even when every response reports no more results, identity
				// stability and cross-query ordering have not been live proved.
				partial = true
			}
			results, filterUnknown := filterTeamsSearchHits(results, filters)
			partial = partial || filterUnknown > 0 || identityUnknown > 0
			truncated := len(results) > limit
			if truncated {
				results = results[:limit]
				partial = true
			}
			scope := "all_accessible_chats"
			if conversation != "" {
				scope = "one_to_one_chat"
			}
			out := map[string]any{"results": results, "count": len(results), "partial": partial, "truncated": truncated, "offset": offset, "source": "live_search", "scope": scope, "filter_metadata_unavailable": filterUnknown}
			if len(pages) == 1 && !filters.active() {
				out["total"] = serverTotal
			} else {
				out["total"] = nil
				out["term_pages"] = pageInfo
				out["filter_basis"] = "local filtering of returned search result fields"
			}
			if len(pages) > 1 {
				out["match_basis"] = "intersection of one bounded server result page per distinct word; not a phrase query"
				out["unavailable_identities"] = identityUnknown
			}
			return flags.printJSON(cmd, out)
		}}
	cmd.Flags().StringVar(&conversation, "conversation", "", "Limit search to this one-to-one chat ID")
	cmd.Flags().StringVar(&term, "query", "", "One to four literal words; live multi-word results intersect bounded pages")
	cmd.Flags().StringVar(&zone, "timezone", "UTC", "IANA time zone for query interpretation")
	cmd.Flags().IntVar(&offset, "offset", 0, "Starting result offset")
	cmd.Flags().IntVar(&limit, "limit", 25, "Maximum results in this page (1–25)")
	cmd.Flags().StringVar(&from, "from", "", "Filter returned live results by exact sender email")
	cmd.Flags().StringVar(&since, "since", "", "Filter returned live results sent on or after this RFC3339 time")
	cmd.Flags().StringVar(&until, "until", "", "Filter returned live results sent before this RFC3339 time")
	cmd.Flags().BoolVar(&hasAttachments, "has-attachments", false, "Filter returned live results by attachment presence (true or false)")
	cmd.Flags().BoolVar(&local, "local", false, "Search only pages already saved in the local mirror")
	return cmd
}
func newTeamsSearchCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "search", Short: "Search Teams", Annotations: map[string]string{"pp:parent-group": "true"}, RunE: parentNoSubcommandRunE(flags)}
	cmd.AddCommand(newTeamsSearchMessagesCmd(flags))
	return cmd
}
func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) { addTeamsCommandGroup(root, newTeamsSearchCmd(flags)) })
}
