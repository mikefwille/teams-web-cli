package cli

import (
	"errors"
	"strings"
	"time"
)

// Each term is sent as an independent observed single-term request. Never
// concatenate user-provided search syntax into the server's query expression.
func teamsSearchTerms(query string) ([]string, error) {
	words := strings.Fields(query)
	if len(words) == 0 || len(words) > 4 {
		return nil, usageErr(errors.New("--query must contain one to four literal words"))
	}
	terms := make([]string, 0, len(words))
	seen := map[string]bool{}
	for _, word := range words {
		upper := strings.ToUpper(word)
		if !teamsSearchTerm.MatchString(word) || upper == "AND" || upper == "OR" || upper == "NOT" {
			return nil, usageErr(errors.New("--query accepts literal words only; operators and punctuation are not supported"))
		}
		key := strings.ToLower(word)
		if !seen[key] {
			terms = append(terms, word)
			seen[key] = true
		}
	}
	return terms, nil
}

type teamsSearchFilters struct {
	from        string
	since       time.Time
	until       time.Time
	attachments *bool
}

func parseTeamsSearchFilters(from, since, until string, attachments *bool) (teamsSearchFilters, error) {
	f := teamsSearchFilters{from: strings.TrimSpace(from), attachments: attachments}
	if f.from != "" && (strings.ContainsAny(f.from, " \t\r\n") || !strings.Contains(f.from, "@")) {
		return f, usageErr(errors.New("--from must be an exact sender email address"))
	}
	var err error
	if since != "" {
		f.since, err = time.Parse(time.RFC3339, since)
		if err != nil {
			return f, usageErr(errors.New("--since must be RFC3339 with an offset"))
		}
	}
	if until != "" {
		f.until, err = time.Parse(time.RFC3339, until)
		if err != nil {
			return f, usageErr(errors.New("--until must be RFC3339 with an offset"))
		}
	}
	if !f.since.IsZero() && !f.until.IsZero() && !f.since.Before(f.until) {
		return f, usageErr(errors.New("--since must be earlier than --until"))
	}
	return f, nil
}

func (f teamsSearchFilters) active() bool {
	return f.from != "" || !f.since.IsZero() || !f.until.IsZero() || f.attachments != nil
}

// Missing or invalid filter metadata is kept distinct from a known mismatch.
func (f teamsSearchFilters) matches(hit map[string]any) (bool, bool) {
	source, ok := hit["Source"].(map[string]any)
	if !ok {
		return false, false
	}
	if f.from != "" {
		from, ok := source["From"].(map[string]any)
		if !ok {
			return false, false
		}
		email, ok := from["EmailAddress"].(map[string]any)
		if !ok {
			return false, false
		}
		address, ok := email["Address"].(string)
		if !ok || address == "" {
			return false, false
		}
		if !strings.EqualFold(address, f.from) {
			return false, true
		}
	}
	if !f.since.IsZero() || !f.until.IsZero() {
		value, ok := source["DateTimeSent"].(string)
		at, err := time.Parse(time.RFC3339Nano, value)
		if !ok || err != nil {
			return false, false
		}
		if (!f.since.IsZero() && at.Before(f.since)) || (!f.until.IsZero() && !at.Before(f.until)) {
			return false, true
		}
	}
	if f.attachments != nil {
		value, ok := source["HasAttachments"].(bool)
		if !ok {
			return false, false
		}
		if value != *f.attachments {
			return false, true
		}
	}
	return true, true
}

type teamsSearchIdentity struct{ thread, id string }

func teamsSearchHitIdentity(hit map[string]any) (teamsSearchIdentity, bool) {
	source, ok := hit["Source"].(map[string]any)
	if !ok {
		return teamsSearchIdentity{}, false
	}
	thread, threadOK := source["ClientThreadId"].(string)
	id, idOK := source["Id"].(string)
	return teamsSearchIdentity{thread: thread, id: id}, threadOK && idOK && thread != "" && id != ""
}

// Intersect only identities actually present in every bounded response. A hit
// lacking an identity cannot safely be matched and is counted as unavailable.
func intersectTeamsSearchPages(pages [][]map[string]any) ([]map[string]any, int) {
	if len(pages) == 0 {
		return []map[string]any{}, 0
	}
	sets := make([]map[teamsSearchIdentity]bool, len(pages))
	unknown := 0
	for i, page := range pages {
		sets[i] = map[teamsSearchIdentity]bool{}
		for _, hit := range page {
			id, ok := teamsSearchHitIdentity(hit)
			if !ok {
				unknown++
				continue
			}
			sets[i][id] = true
		}
	}
	results := []map[string]any{}
	seen := map[teamsSearchIdentity]bool{}
	for _, hit := range pages[0] {
		id, ok := teamsSearchHitIdentity(hit)
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		found := true
		for _, set := range sets[1:] {
			if !set[id] {
				found = false
				break
			}
		}
		if found {
			results = append(results, hit)
		}
	}
	return results, unknown
}

func filterTeamsSearchHits(hits []map[string]any, f teamsSearchFilters) ([]map[string]any, int) {
	if !f.active() {
		return hits, 0
	}
	results := []map[string]any{}
	unknown := 0
	for _, hit := range hits {
		match, known := f.matches(hit)
		if !known {
			unknown++
		} else if match {
			results = append(results, hit)
		}
	}
	return results, unknown
}
