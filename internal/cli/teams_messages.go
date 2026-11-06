// Message history and mutation commands built from observed Teams web traffic.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/client"
)

type teamsMessagePage struct {
	Messages []map[string]any `json:"messages"`
	Metadata map[string]any   `json:"_metadata"`
}

func decodeTeamsMessagePage(data []byte) (teamsMessagePage, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil || envelope == nil {
		return teamsMessagePage{}, errors.New("Teams returned invalid message history JSON")
	}
	if raw := strings.TrimSpace(string(envelope["error"])); raw != "" && raw != "null" {
		return teamsMessagePage{}, errors.New("Teams reported a message history error")
	}
	var page teamsMessagePage
	if raw := envelope["messages"]; len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" || json.Unmarshal(raw, &page.Messages) != nil || page.Messages == nil {
		return teamsMessagePage{}, errors.New("Teams returned an unexpected message history envelope")
	}
	if raw := envelope["_metadata"]; len(raw) != 0 && strings.TrimSpace(string(raw)) != "null" && json.Unmarshal(raw, &page.Metadata) != nil {
		return teamsMessagePage{}, errors.New("Teams returned invalid message history metadata")
	}
	return page, nil
}

func validTeamsMessagePathID(id string) bool {
	return strings.TrimSpace(id) != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\?#\r\n")
}

func teamsHistoryParams(cursor string, start int64, pageSize int) (map[string]string, error) {
	if pageSize < 1 || pageSize > 200 {
		return nil, usageErr(errors.New("--page-size must be between 1 and 200"))
	}
	if start < 0 {
		return nil, usageErr(errors.New("--start-time cannot be negative"))
	}
	if len(cursor) > 65536 {
		return nil, usageErr(errors.New("sync state is too long"))
	}
	params := map[string]string{"view": "msnp24Equivalent|supportsMessageProperties", "pageSize": fmt.Sprint(pageSize)}
	if start > 0 {
		params["startTime"] = fmt.Sprint(start)
	}
	if cursor != "" {
		params["syncState"] = cursor
	}
	return params, nil
}

func teamsHistoryPointParams(point teamsHistoryPoint, pageSize int) (map[string]string, error) {
	params, err := teamsHistoryParams(point.SyncState, point.StartTime, pageSize)
	if err != nil {
		return nil, err
	}
	if point.HasStartTime {
		params["startTime"] = fmt.Sprint(point.StartTime)
	}
	if point.View != "" {
		if point.View != "msnp24Equivalent" && point.View != "msnp24Equivalent|supportsMessageProperties" {
			return nil, errors.New("invalid Teams history view")
		}
		params["view"] = point.View
	}
	return params, nil
}

// pp:data-source live
func fetchTeamsMessages(cmd *cobra.Command, flags *rootFlags, conversation, cursor string, start int64, pageSize int) (teamsMessagePage, error) {
	c, err := flags.newClient()
	if err != nil {
		return teamsMessagePage{}, err
	}
	return fetchTeamsMessagesWithClient(cmd, flags, c, conversation, cursor, start, pageSize)
}

func fetchTeamsMessagesWithClient(cmd *cobra.Command, flags *rootFlags, c *client.Client, conversation, cursor string, start int64, pageSize int) (teamsMessagePage, error) {
	return fetchTeamsMessagePoint(cmd, flags, c, conversation, teamsHistoryPoint{StartTime: start, SyncState: cursor}, pageSize)
}

func fetchTeamsMessagePoint(cmd *cobra.Command, flags *rootFlags, c *client.Client, conversation string, point teamsHistoryPoint, pageSize int) (teamsMessagePage, error) {
	if !validTeamsMessagePathID(conversation) {
		return teamsMessagePage{}, usageErr(errors.New("--conversation must be a valid opaque conversation ID"))
	}
	params, err := teamsHistoryPointParams(point, pageSize)
	if err != nil {
		return teamsMessagePage{}, err
	}
	ctx, cancel := boundCtx(cmd.Context(), flags)
	defer cancel()
	path := "/api/chatsvc/amer/v1/users/me/conversations/" + url.PathEscape(conversation) + "/messages"
	data, err := c.GetNoCache(ctx, path, params)
	if err != nil {
		// A transport error can include the full query, including sync state.
		return teamsMessagePage{}, teamsHistoryRequestError{cause: classifyAPIErrorOnly(err)}
	}
	return decodeTeamsMessagePage(data)
}

func teamsCursor(value, file string) (string, error) {
	if value != "" && file != "" {
		return "", usageErr(errors.New("use either --sync-state or --cursor-file"))
	}
	if file == "" {
		return value, nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("reading cursor file: %w", err)
	}
	if len(data) > 65536 {
		return "", errors.New("cursor file is too large")
	}
	return strings.TrimSpace(string(data)), nil
}

func configureTeamsMessageRead(cmd *cobra.Command, flags *rootFlags, mode string) {
	var conversation, cursor, cursorFile, planFile string
	var start int64
	var pageSize, maxPages int
	cmd.Short = "Read selected pages of a Teams conversation"
	cmd.Annotations = map[string]string{"mcp:read-only": "true", "mcp:read-flags": "cursor-file,page-plan-file", "pp:data-source": "live"}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if dryRunOK(flags) {
			return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "messages " + mode})
		}
		if mode == "get" && (len(args) != 1 || strings.TrimSpace(args[0]) == "") {
			return usageErr(errors.New("one message ID is required"))
		}
		if mode == "list" && len(args) != 0 {
			return usageErr(errors.New("list takes no positional arguments"))
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
		info := map[string]any{"source": "live", "partial": true, "pages_scanned": scan.PagesScanned, "stop_reason": scan.StopReason, "overlap_count": scan.OverlapCount, "conflicting_copies": scan.ConflictingCopies, "unidentified_count": scan.Unidentified, "ordering": "service page order; tombstones take precedence over overlapping content"}
		if mode == "get" {
			for _, message := range scan.Messages {
				if id, _ := message["id"].(string); id == args[0] {
					info["result"] = message
					return flags.printJSON(cmd, info)
				}
			}
			return fmt.Errorf("message not found in %d selected history page(s); this is not proof it does not exist", scan.PagesScanned)
		}
		info["results"] = scan.Messages
		info["count"] = len(scan.Messages)
		info["metadata"] = scan.Metadata[0] // Backward-compatible first-page metadata.
		if len(scan.Metadata) > 1 {
			info["page_metadata"] = scan.Metadata
		}
		info["pagination"] = "bounded backward links or explicit page selectors; complete history is not established"
		return flags.printJSON(cmd, info)
	}
	cmd.Flags().StringVar(&conversation, "conversation", "", "Opaque conversation or channel ID")
	cmd.Flags().IntVar(&pageSize, "page-size", 200, "Maximum messages requested in one page")
	cmd.Flags().IntVar(&maxPages, "max-pages", 1, "Maximum backward pages to read, including the first (1–10)")
	cmd.Flags().Int64Var(&start, "start-time", 0, "Optional page start time as Unix milliseconds")
	cmd.Flags().StringVar(&cursor, "sync-state", "", "Opaque sync state returned by Teams")
	cmd.Flags().StringVar(&cursorFile, "cursor-file", "", "Read the opaque sync state from a local file")
	cmd.Flags().StringVar(&planFile, "page-plan-file", "", "Read up to nine additional explicit history selectors from a JSON file")
}

func newTeamsMessageListCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "list"}
	configureTeamsMessageRead(cmd, flags, "list")
	return cmd
}

func newTeamsMessageGetCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "get [message-id]"}
	configureTeamsMessageRead(cmd, flags, "get")
	return cmd
}

func newTeamsMessagesCmd(flags *rootFlags) *cobra.Command {
	group := &cobra.Command{Use: "messages", Short: "Read and manage Teams conversation messages", Annotations: map[string]string{"pp:parent-group": "true"}, RunE: parentNoSubcommandRunE(flags)}
	group.AddCommand(newTeamsMessageListCmd(flags), newTeamsMessageGetCmd(flags), newTeamsMessageSendCmd(flags), newTeamsMessageRichSendCmd(flags), newTeamsMessageEditCmd(flags), newTeamsMessageDeleteCmd(flags), newTeamsMessageReplyCmd(flags), newTeamsMessageReactionsCmd(flags), newTeamsContextCmd(flags))
	return group
}

func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		addTeamsCommandGroup(root, newTeamsMessagesCmd(flags))
	})
}
