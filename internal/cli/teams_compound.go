// Bounded compound workflows over selected Teams conversation pages.
package cli

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func teamsMessageTime(message map[string]any) (time.Time, bool) {
	for _, key := range []string{"originalarrivaltime", "composetime"} {
		switch value := message[key].(type) {
		case string:
			if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
				return parsed, true
			}
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
				return time.UnixMilli(n), true
			}
		case float64:
			if value > 0 && value < 1e15 {
				return time.UnixMilli(int64(value)), true
			}
		}
	}
	return time.Time{}, false
}

func teamsStructuredMention(message map[string]any, mri string) (bool, bool) {
	props, ok := message["properties"].(map[string]any)
	if !ok {
		return false, false
	}
	raw, exists := props["mentions"]
	if !exists || raw == nil {
		return false, false
	}
	if value, ok := raw.(string); ok {
		if json.Unmarshal([]byte(value), &raw) != nil {
			return false, false
		}
	}
	items, ok := raw.([]any)
	if !ok {
		return false, false
	}
	var match func(any) bool
	match = func(value any) bool {
		switch v := value.(type) {
		case string:
			return strings.EqualFold(v, mri)
		case []any:
			for _, x := range v {
				if match(x) {
					return true
				}
			}
		case map[string]any:
			for _, x := range v {
				if match(x) {
					return true
				}
			}
		}
		return false
	}
	return match(items), true
}

// teamsQuoteReferences recognizes only the observed qtdMsgs JSON array. It
// does not infer a channel thread relationship from neighboring text.
func teamsQuoteReferences(message map[string]any) ([]string, bool) {
	value, exists := message["properties"]
	if !exists || value == nil {
		return []string{}, true
	}
	props, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	var raw any
	found := false
	for key, value := range props {
		if strings.EqualFold(key, "qtdMsgs") {
			if found {
				return nil, false
			}
			found = true
			raw = value
		}
	}
	if !found || raw == nil || raw == "" {
		return []string{}, true
	}
	if text, ok := raw.(string); ok {
		if len(text) > 65536 || json.Unmarshal([]byte(text), &raw) != nil {
			return nil, false
		}
	}
	entries, ok := raw.([]any)
	if !ok || len(entries) > 20 {
		return nil, false
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, entry := range entries {
		item, ok := entry.(map[string]any)
		if !ok {
			return nil, false
		}
		id, ok := item["messageId"].(string)
		if !ok || !validTeamsMessagePathID(id) {
			return nil, false
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, true
}
func teamsNeighborWindow(messages []map[string]any, index, before, after int) ([]map[string]any, int) {
	lo := index - before
	if lo < 0 {
		lo = 0
	}
	hi := index + after + 1
	if hi > len(messages) {
		hi = len(messages)
	}
	return messages[lo:hi], index - lo
}

// Resolve references only within selected pages; missing related messages stay
// explicitly unresolved rather than triggering an inferred network request.
func teamsObservedQuoteContext(messages []map[string]any, targetIndex int) map[string]any {
	const maxRelated = 20
	targetID, _ := messages[targetIndex]["id"].(string)
	positions := map[string]int{}
	for i, m := range messages {
		if id, ok := m["id"].(string); ok && validTeamsMessagePathID(id) {
			if _, exists := positions[id]; !exists {
				positions[id] = i
			}
		}
	}
	related := []map[string]any{}
	seen := map[string]bool{}
	total, malformed := 0, 0
	add := func(kind, id string, index int) {
		key := kind + "\x00" + id
		if seen[key] || id == targetID {
			return
		}
		seen[key] = true
		total++
		if len(related) >= maxRelated {
			return
		}
		entry := map[string]any{"relation": kind, "message_id": id, "resolved": index >= 0}
		if index >= 0 {
			entry["message"] = messages[index]
		}
		related = append(related, entry)
	}
	refs, ok := teamsQuoteReferences(messages[targetIndex])
	if !ok {
		malformed++
	} else {
		for _, id := range refs {
			index, exists := positions[id]
			if !exists {
				index = -1
			}
			add("target_quotes", id, index)
		}
	}
	for i, m := range messages {
		if i == targetIndex {
			continue
		}
		ids, ok := teamsQuoteReferences(m)
		if !ok {
			malformed++
			continue
		}
		for _, id := range ids {
			if id == targetID {
				other, _ := m["id"].(string)
				if validTeamsMessagePathID(other) {
					add("quotes_target", other, i)
				}
				break
			}
		}
	}
	return map[string]any{"related": related, "related_count": len(related), "related_truncated": total > len(related), "invalid_quote_metadata_count": malformed, "relationship_basis": "observed qtdMsgs quote references in selected pages", "relationship_partial": true}
}

func teamsMentionReview(messages []map[string]any, mri string, limit, before, after int) ([]map[string]any, []map[string]any, int, int) {
	results := []map[string]any{}
	contexts := []map[string]any{}
	unknown, matched := 0, 0
	for i, message := range messages {
		found, known := teamsStructuredMention(message, mri)
		if !known {
			unknown++
		}
		if !found {
			continue
		}
		matched++
		if len(results) < limit {
			results = append(results, message)
			neighbors, target := teamsNeighborWindow(messages, i, before, after)
			contexts = append(contexts, map[string]any{"message_id": message["id"], "results": neighbors, "target_index": target, "partial": true})
		}
	}
	return results, contexts, unknown, matched
}

// pp:data-source live
func newTeamsCatchUpCmd(flags *rootFlags) *cobra.Command {
	var conversation, since string
	var limit int
	cmd := &cobra.Command{Use: "catch-up", Short: "Read recent activity in one conversation page", Args: cobra.NoArgs, Annotations: map[string]string{"mcp:read-only": "true", "pp:data-source": "live"}, RunE: func(cmd *cobra.Command, _ []string) error {
		if requiredInputWaived() {
			return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "catch-up"})
		}
		if !validTeamsMessagePathID(conversation) {
			return usageErr(errors.New("--conversation is required"))
		}
		if limit < 1 || limit > 200 {
			return usageErr(errors.New("--limit must be 1–200"))
		}
		var cutoff time.Time
		var err error
		if since != "" {
			cutoff, err = time.Parse(time.RFC3339, since)
			if err != nil {
				return usageErr(errors.New("--since must be RFC3339 with an offset"))
			}
		}
		if dryRunOK(flags) {
			return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "catch-up", "conversation": conversation})
		}
		page, err := fetchTeamsMessages(cmd, flags, conversation, "", 0, 200)
		if err != nil {
			return err
		}
		results := []map[string]any{}
		unknown := 0
		matched := 0
		for _, message := range page.Messages {
			if !cutoff.IsZero() {
				at, ok := teamsMessageTime(message)
				if !ok {
					unknown++
					continue
				}
				if at.Before(cutoff) {
					continue
				}
			}
			matched++
			if len(results) < limit {
				results = append(results, message)
			}
		}
		return flags.printJSON(cmd, map[string]any{"results": results, "count": len(results), "source": "live_page", "partial": true, "truncated": matched > len(results), "unknown_timestamp_count": unknown, "metadata": page.Metadata})
	}}
	cmd.Flags().StringVar(&conversation, "conversation", "", "Conversation ID")
	cmd.Flags().StringVar(&since, "since", "", "Optional RFC3339 lower time bound")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results from the current page")
	return cmd
}

// pp:data-source live
func newTeamsMentionsCmd(flags *rootFlags) *cobra.Command {
	var conversation string
	var limit, before, after int
	cmd := &cobra.Command{Use: "mentions", Short: "Find structured mentions of the current account in one page", Args: cobra.NoArgs, Annotations: map[string]string{"mcp:read-only": "true", "pp:data-source": "live"}, RunE: func(cmd *cobra.Command, _ []string) error {
		if requiredInputWaived() {
			return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "mentions"})
		}
		if !validTeamsMessagePathID(conversation) {
			return usageErr(errors.New("--conversation is required"))
		}
		if limit < 1 || limit > 200 {
			return usageErr(errors.New("--limit must be 1–200"))
		}
		if before < 0 || before > 10 || after < 0 || after > 10 {
			return usageErr(errors.New("--before and --after must be 0–10"))
		}
		if dryRunOK(flags) {
			return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "mentions", "conversation": conversation})
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
			return authErr(errors.New("Teams session identity is unavailable"))
		}
		page, err := fetchTeamsMessagesWithClient(cmd, flags, c, conversation, "", 0, 200)
		if err != nil {
			return err
		}
		mri := "8:orgid:" + p.Object
		results, contexts, unknown, matched := teamsMentionReview(page.Messages, mri, limit, before, after)
		return flags.printJSON(cmd, map[string]any{"results": results, "contexts": contexts, "count": len(results), "source": "live_page", "partial": true, "truncated": matched > len(results), "unavailable_mention_metadata_count": unknown, "match_basis": "exact current-account MRI in structured mention metadata", "metadata": page.Metadata})
	}}
	cmd.Flags().StringVar(&conversation, "conversation", "", "Conversation ID")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum returned mentions")
	cmd.Flags().IntVar(&before, "before", 2, "Messages before each match in service order (0–10)")
	cmd.Flags().IntVar(&after, "after", 2, "Messages after each match in service order (0–10)")
	return cmd
}

// pp:data-source live
func newTeamsContextCmd(flags *rootFlags) *cobra.Command {
	var conversation, id, cursor, cursorFile, planFile string
	var before, after, pageSize, maxPages int
	var start int64
	cmd := &cobra.Command{Use: "context", Short: "Collect neighbors from selected conversation pages", Args: cobra.NoArgs, Annotations: map[string]string{"mcp:read-only": "true", "mcp:read-flags": "cursor-file,page-plan-file", "pp:data-source": "live"}, RunE: func(cmd *cobra.Command, _ []string) error {
		if requiredInputWaived() {
			return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "context"})
		}
		if !validTeamsMessagePathID(conversation) || !validTeamsMessagePathID(id) {
			return usageErr(errors.New("--conversation and --message-id are required"))
		}
		if before < 0 || before > 50 || after < 0 || after > 50 {
			return usageErr(errors.New("--before and --after must be 0–50"))
		}
		if dryRunOK(flags) {
			return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "context", "conversation": conversation, "message_id": id})
		}
		state, err := teamsCursor(cursor, cursorFile)
		if err != nil {
			return err
		}
		points, err := readTeamsHistoryPlan(planFile, teamsHistoryPoint{StartTime: start, SyncState: state}, pageSize)
		if err != nil {
			return err
		}
		if maxPages < 1 || maxPages > 10 || (planFile != "" && maxPages != 1) {
			return usageErr(errors.New("--max-pages must be 1–10 and cannot be combined with --page-plan-file"))
		}
		scan, err := fetchTeamsHistoryScan(cmd, flags, conversation, points, pageSize, maxPages)
		if err != nil {
			return err
		}
		index := -1
		for i, message := range scan.Messages {
			if message["id"] == id {
				index = i
				break
			}
		}
		if index < 0 {
			return errors.New("message not found in selected pages; this is not proof it does not exist")
		}
		neighbors, target := teamsNeighborWindow(scan.Messages, index, before, after)
		out := map[string]any{"results": neighbors, "count": len(neighbors), "target_index": target, "source": "live_pages", "partial": true, "ordering": "service page order; tombstones take precedence over overlapping content", "pages_scanned": scan.PagesScanned, "stop_reason": scan.StopReason, "overlap_count": scan.OverlapCount, "conflicting_copies": scan.ConflictingCopies, "unidentified_count": scan.Unidentified, "metadata": scan.Metadata[0]}
		for key, value := range teamsObservedQuoteContext(scan.Messages, index) {
			out[key] = value
		}
		return flags.printJSON(cmd, out)
	}}
	cmd.Example = `teams-web-pp-cli messages context --conversation "$CONVERSATION_ID" --message-id "$MESSAGE_ID" --json --no-learn`
	cmd.Flags().StringVar(&conversation, "conversation", "", "Conversation ID")
	cmd.Flags().StringVar(&id, "message-id", "", "Target message ID")
	cmd.Flags().IntVar(&before, "before", 5, "Messages before target in service order")
	cmd.Flags().IntVar(&after, "after", 5, "Messages after target in service order")
	cmd.Flags().IntVar(&pageSize, "page-size", 200, "Maximum messages requested in each page")
	cmd.Flags().IntVar(&maxPages, "max-pages", 1, "Maximum backward pages to read, including the first (1–10)")
	cmd.Flags().Int64Var(&start, "start-time", 0, "First page start time as Unix milliseconds")
	cmd.Flags().StringVar(&cursor, "sync-state", "", "Opaque sync state for the first selected page")
	cmd.Flags().StringVar(&cursorFile, "cursor-file", "", "Read the first page sync state from a local file")
	cmd.Flags().StringVar(&planFile, "page-plan-file", "", "Read up to nine additional explicit history selectors from a JSON file")
	return cmd
}

func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		addNovelCommandIfAbsent(root, newTeamsCatchUpCmd(flags))
		addNovelCommandIfAbsent(root, newTeamsMentionsCmd(flags))
		addNovelCommandIfAbsent(root, newTeamsContextCmd(flags))
	})
}
