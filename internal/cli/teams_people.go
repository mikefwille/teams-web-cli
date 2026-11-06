// Bounded Teams people suggestions from the observed powerbar service.
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
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
)

const teamsPeoplePath = "/search/api/v1/suggestions"
const teamsPeopleFlights = "EnableMessageImageSearch,EnableTeamsChannelDomainPowerbar"

var teamsPeopleFields = []string{"Id", "MRI", "DisplayName", "EmailAddresses", "PeopleType", "PeopleSubtype", "Phones", "UserPrincipalName", "GivenName", "Surname", "Cid", "CompanyName", "JobTitle", "ImAddress", "ExternalDirectoryObjectId", "ConcatenatedId", "Department"}
var teamsPeopleUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func teamsPeopleAnchor(principal teamsTokenPrincipal) (string, error) {
	if !teamsPeopleUUID.MatchString(principal.Object) || !teamsPeopleUUID.MatchString(principal.Tenant) {
		return "", errors.New("Teams account identity is unavailable for people lookup")
	}
	return "Oid:" + principal.Object + "@" + principal.Tenant, nil
}

func teamsPeopleQuery(input string) (string, error) {
	query := strings.Join(strings.Fields(input), " ")
	if query == "" || !utf8.ValidString(input) || utf8.RuneCountInString(query) > 128 {
		return "", usageErr(errors.New("--query must contain 1–128 characters"))
	}
	for _, r := range input {
		if unicode.IsControl(r) || (!unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsSpace(r) && !strings.ContainsRune("@._-+'", r)) {
			return "", usageErr(errors.New("--query must be a literal name or address"))
		}
	}
	return query, nil
}

func teamsPeoplePayload(query string) (map[string]any, error) {
	query, err := teamsPeopleQuery(query)
	if err != nil {
		return nil, err
	}
	cvid, err := teamsSearchUUID()
	if err != nil {
		return nil, err
	}
	logical, err := teamsSearchUUID()
	if err != nil {
		return nil, err
	}
	term := func(key, value string) map[string]any { return map[string]any{"Term": map[string]string{key: value}} }
	people := map[string]any{
		"EntityType": "People", "Size": 5, "From": 0,
		"Query":       map[string]string{"QueryString": query, "DisplayQueryString": query, "NormalizedQueryString": query},
		"Provenances": []string{"Mailbox", "Directory"}, "Fields": teamsPeopleFields,
		"Filter": map[string]any{"And": []any{
			map[string]any{"Or": []any{term("PeopleType", "Person"), term("PeopleType", "Other")}},
			map[string]any{"Or": []any{term("PeopleSubtype", "OrganizationUser"), term("PeopleSubtype", "MTOUser"), term("PeopleSubtype", "PersonalContact"), term("PeopleSubtype", "Guest")}},
			map[string]any{"Or": []any{term("Flags", "NonHidden")}},
		}},
	}
	return map[string]any{
		"AppName": "Microsoft Teams", "Cvid": cvid, "LogicalId": logical, "dataSource": "personScoped",
		"Scenario":       map[string]any{"Name": "powerbar", "Dimensions": []any{map[string]string{"DimensionName": "QueryType", "DimensionValue": "PeopleCentricSearch"}}},
		"EntityRequests": []any{people},
	}, nil
}

type teamsPeopleTransport struct {
	next    http.RoundTripper
	headers map[string]string
	anchor  string
	flight  string
}

func teamsPeopleURLAllowed(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "substrate.office.com") || u.User != nil || u.Opaque != "" || u.Path != teamsPeoplePath || u.Fragment != "" || u.ForceQuery {
		return false
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(query) != 2 || len(query["scenario"]) != 1 || query.Get("scenario") != "powerbar" || len(query["setflight"]) != 1 || query.Get("setflight") != teamsPeopleFlights {
		return false
	}
	return true
}

func (t teamsPeopleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || !teamsPeopleURLAllowed(req.URL) {
		return nil, errors.New("refusing to send people-search credentials to an unobserved endpoint")
	}
	if t.next == nil || t.anchor == "" || strings.ContainsAny(t.anchor, "\r\n") || len(t.flight) > 2048 || !teamsPeopleFlightValue.MatchString(t.flight) {
		return nil, errors.New("Teams people-search context is unavailable")
	}
	omit := map[string]bool{"host": true, "connection": true, "content-length": true, "accept-encoding": true, "transfer-encoding": true, "proxy-authorization": true, "proxy-connection": true, "upgrade": true, "te": true, "trailer": true, "content-type": true}
	for key, value := range t.headers {
		if !omit[strings.ToLower(key)] && !strings.HasPrefix(key, ":") && !strings.ContainsAny(value, "\r\n") {
			req.Header.Set(key, value)
		}
	}
	// The observed powerbar request routed to the current object and tenant;
	// the independently refreshed message-search scope can carry another anchor.
	req.Header.Set("X-AnchorMailbox", t.anchor)
	req.Header.Set("X-Client-Flights", t.flight)
	return t.next.RoundTrip(req)
}

func decodeTeamsPeople(data []byte) ([]map[string]any, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || root == nil {
		return nil, errors.New("Teams returned invalid people-search JSON")
	}
	for _, key := range []string{"error", "Error"} {
		if raw := bytes.TrimSpace(root[key]); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
			return nil, errors.New("Teams reported a people-search error")
		}
	}
	var groups []struct {
		Type        string          `json:"Type"`
		Suggestions json.RawMessage `json:"Suggestions"`
	}
	if raw, ok := root["Groups"]; !ok || json.Unmarshal(raw, &groups) != nil || groups == nil {
		return nil, errors.New("Teams returned an unexpected people-search envelope")
	}
	results := []map[string]any{}
	allowed := map[string]bool{"Id": true, "MRI": true, "DisplayName": true, "EmailAddresses": true, "PeopleType": true, "PeopleSubtype": true, "Phones": true, "UserPrincipalName": true, "GivenName": true, "Surname": true, "CompanyName": true, "JobTitle": true, "ImAddress": true, "ExternalDirectoryObjectId": true, "Department": true}
	for _, group := range groups {
		if group.Type != "People" {
			continue
		}
		var suggestions []map[string]any
		if len(group.Suggestions) == 0 || json.Unmarshal(group.Suggestions, &suggestions) != nil || suggestions == nil {
			return nil, errors.New("Teams returned invalid people suggestions")
		}
		for _, suggestion := range suggestions {
			if suggestion == nil {
				return nil, errors.New("Teams returned an invalid person")
			}
			item := make(map[string]any)
			for key, value := range suggestion {
				if allowed[key] {
					item[key] = value
				}
			}
			if len(item) == 0 {
				return nil, errors.New("Teams returned a person without usable fields")
			}
			results = append(results, item)
		}
	}
	return results, nil
}

func runTeamsPeopleSuggestions(ctx context.Context, hc *http.Client, body any) ([]byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	q := url.Values{"scenario": {"powerbar"}, "setflight": {teamsPeopleFlights}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, teamsSearchOrigin+teamsPeoplePath+"?"+q.Encode(), bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if err := teamsSearchLimiter.Wait(ctx); err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, errors.New("Teams people-search request failed")
	}
	defer resp.Body.Close()
	if remaining, reset, ok := cliutil.ParseRateLimitHeaders(resp.Header); ok {
		teamsSearchLimiter.ObserveHeaders(remaining, reset)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2_000_001))
	if err != nil || len(data) > 2_000_000 {
		return nil, errors.New("Teams people-search response could not be read within the limit")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		teamsSearchLimiter.OnRateLimit()
		return nil, rateLimitErr(&cliutil.RateLimitError{URL: "Teams search service", RetryAfter: cliutil.RetryAfter(resp)})
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifyAPIErrorOnly(fmt.Errorf("Teams people search returned HTTP %d", resp.StatusCode))
	}
	teamsSearchLimiter.OnSuccess()
	return data, nil
}

// pp:data-source live
func newTeamsPeopleSearchCmd(flags *rootFlags) *cobra.Command {
	var query string
	var limit int
	cmd := &cobra.Command{Use: "search", Short: "Search bounded Teams people suggestions", Args: cobra.NoArgs, Annotations: map[string]string{"mcp:read-only": "true", "pp:data-source": "live"}}
	cmd.Example = `teams-web-pp-cli people search --query "$PERSON_QUERY" --json --no-learn`
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if limit < 1 || limit > 5 {
			return usageErr(errors.New("--limit must be 1–5"))
		}
		body, err := teamsPeoplePayload(query)
		if err != nil {
			return err
		}
		if dryRunOK(flags) {
			return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "people search", "source": "teams_native_typeahead", "partial": true})
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
		anchor, err := teamsPeopleAnchor(principal)
		if err != nil {
			return authErr(err)
		}
		dir, err := teamsSessionDir()
		if err != nil {
			return err
		}
		headers, err := loadTeamsSearchHeaders(dir, principal)
		if err != nil {
			return authErr(err)
		}
		flight, err := loadTeamsPeopleFlight(dir, principal, headers)
		if err != nil {
			return authErr(err)
		}
		guard, ok := c.HTTPClient.Transport.(teamsOriginTransport)
		if !ok {
			return errors.New("Teams session origin guard is unavailable")
		}
		hc := *c.HTTPClient
		hc.Transport = teamsPeopleTransport{next: guard.next, headers: headers, anchor: anchor, flight: flight}
		hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		ctx, cancel := boundCtx(cmd.Context(), flags)
		defer cancel()
		data, err := runTeamsPeopleSuggestions(ctx, &hc, body)
		if err != nil {
			return err
		}
		results, err := decodeTeamsPeople(data)
		if err != nil {
			return err
		}
		truncated := len(results) > limit
		if truncated {
			results = results[:limit]
		}
		return flags.printJSON(cmd, map[string]any{"results": results, "count": len(results), "source": "teams_native_typeahead", "partial": true, "truncated": truncated, "total": nil})
	}
	cmd.Flags().StringVar(&query, "query", "", "Literal person name or address")
	cmd.Flags().IntVar(&limit, "limit", 5, "Maximum returned suggestions (1–5)")
	_ = cmd.MarkFlagRequired("query")
	return cmd
}

func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		group := &cobra.Command{Use: "people", Short: "Find people in Teams", Annotations: map[string]string{"pp:parent-group": "true"}, RunE: parentNoSubcommandRunE(flags)}
		group.AddCommand(newTeamsPeopleSearchCmd(flags))
		addTeamsCommandGroup(root, group)
	})
}
