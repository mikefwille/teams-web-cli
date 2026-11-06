// Chat quote replies and Like reactions based on observed web requests.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
)

const teamsSelfChat = "48:notes"

func teamsPlainHTMLBody(content string) (string, bool) {
	if !strings.HasPrefix(content, "<p>") || !strings.HasSuffix(content, "</p>") {
		return "", false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(content, "<p>"), "</p>")
	// Only an exact paragraph and line-break tags are allowed. Preserve the
	// original safe body, including entity and whitespace serialization, when
	// it is used as a quote preview; never carry arbitrary HTML through.
	if strings.Contains(strings.ReplaceAll(inner, "<br>", ""), "<") {
		return "", false
	}
	return inner, true
}

func teamsPlainTextFromHTML(content string) (string, bool) {
	inner, ok := teamsPlainHTMLBody(content)
	if !ok {
		return "", false
	}
	return html.UnescapeString(strings.ReplaceAll(inner, "<br>", "\n")), true
}

func teamsMessageSender(message map[string]any) (string, error) {
	raw, _ := message["from"].(string)
	u, err := url.Parse(raw)
	if err != nil || !teamsOrigin(raw) || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("source message sender has an unsupported identity")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 8 || parts[0] != "api" || parts[1] != "chatsvc" || parts[2] != "amer" || parts[3] != "v1" || parts[4] != "users" || !strings.EqualFold(parts[5], "me") || parts[6] != "contacts" || !strings.HasPrefix(strings.ToLower(parts[7]), "8:orgid:") {
		return "", errors.New("source message sender has an unsupported identity")
	}
	identity := parts[7][len("8:orgid:"):]
	if identity == "" || len(identity) > 128 || strings.Trim(identity, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") != "" {
		return "", errors.New("source message sender has an unsupported identity")
	}
	return "8:orgid:" + identity, nil
}

func teamsQuotePayload(message map[string]any, conversation, id, text string, principal teamsTokenPrincipal, now time.Time) (map[string]any, string, error) {
	if !validTeamsMessagePathID(conversation) || message["id"] != id || message["conversationid"] != conversation || teamsMessageDeleted(message) || message["messagetype"] != "RichText/Html" || message["contenttype"] != "Text" {
		return nil, "", errors.New("quote reply requires an undeleted text message in the selected chat")
	}
	for _, key := range []string{"amsreferences", "attachments"} {
		if value, exists := message[key]; exists && !teamsEmptyStructuredValue(value) {
			return nil, "", errors.New("quote reply does not support source attachments or structured content")
		}
	}
	if value, exists := message["properties"]; exists && value != nil {
		properties, ok := value.(map[string]any)
		if !ok {
			return nil, "", errors.New("source message properties have an unsupported format")
		}
		for key, value := range properties {
			lower := strings.ToLower(key)
			if (lower == "qtdmsgs" || lower == "onbehalfof" || strings.Contains(lower, "mention") || strings.Contains(lower, "attachment") || strings.Contains(lower, "card") || strings.Contains(lower, "link") || strings.Contains(lower, "file")) && !teamsEmptyStructuredValue(value) {
				return nil, "", errors.New("quote reply does not support source attachments or structured content")
			}
		}
	}
	content, _ := message["content"].(string)
	previewHTML, ok := teamsPlainHTMLBody(content)
	if !ok {
		return nil, "", errors.New("quote reply currently supports only the observed plain-text HTML shape")
	}
	sender, err := teamsMessageSender(message)
	if err != nil {
		return nil, "", err
	}
	name, _ := message["imdisplayname"].(string)
	arrival, _ := message["originalarrivaltime"].(string)
	at, err := time.Parse(time.RFC3339Nano, arrival)
	if name == "" || len(name) > 1024 || err != nil || at.UnixMilli() <= 0 {
		return nil, "", errors.New("source message name or timestamp is unavailable")
	}
	idTime := at.UnixMilli()
	payload, clientID, err := teamsSendPayload(conversation, text, principal, now)
	if err != nil {
		return nil, "", err
	}
	quote := "<blockquote itemscope itemtype=\"http://schema.skype.com/Reply\" itemid=\"" + html.EscapeString(id) + "\"><strong itemprop=\"mri\" itemid=\"" + html.EscapeString(sender) + "\">" + html.EscapeString(name) + "</strong><span itemprop=\"time\" itemid=\"" + strconv.FormatInt(idTime, 10) + "\"></span><p itemprop=\"preview\">" + previewHTML + "</p></blockquote>"
	payload["content"] = quote + teamsMessageHTML(text)
	quoted, err := json.Marshal([]map[string]any{{"messageId": id, "sender": sender, "time": idTime}})
	if err != nil {
		return nil, "", err
	}
	payload["properties"].(map[string]any)["qtdMsgs"] = string(quoted)
	return payload, clientID, nil
}

func teamsMessageTarget(cmd *cobra.Command, flags *rootFlags, c *client.Client, conversation, id, cursor, cursorFile, planFile string, start int64, maxPages int) (map[string]any, error) {
	state, err := teamsCursor(cursor, cursorFile)
	if err != nil {
		return nil, err
	}
	points, err := readTeamsHistoryPlan(planFile, teamsHistoryPoint{StartTime: start, SyncState: state}, 200)
	if err != nil {
		return nil, err
	}
	if maxPages < 1 || maxPages > 10 || (planFile != "" && maxPages != 1) {
		return nil, usageErr(errors.New("--max-pages must be 1–10 and cannot be combined with --page-plan-file"))
	}
	scan, err := fetchTeamsHistoryScanWithClient(cmd, flags, c, conversation, points, 200, maxPages)
	if err != nil {
		return nil, err
	}
	if scan.ConflictingCopies != 0 {
		return nil, errors.New("selected pages contain conflicting message copies; resolve the current state before writing")
	}
	for _, message := range scan.Messages {
		if message["id"] == id && message["conversationid"] == conversation && !teamsMessageDeleted(message) {
			return message, nil
		}
	}
	return nil, errors.New("undeleted target was not found in the selected chat pages")
}

func teamsIsOrdinaryChat(body teamsBootstrap, conversation string) bool {
	if conversation == teamsSelfChat {
		return true
	}
	for _, channel := range collectTeamsChannels(body) {
		if teamsObjectID(channel) == conversation {
			return false
		}
	}
	for _, chat := range body.Chats {
		if teamsObjectID(chat) == conversation {
			return true
		}
	}
	return false
}

func requireTeamsOrdinaryChat(cmd *cobra.Command, flags *rootFlags, conversation string) error {
	if conversation == teamsSelfChat {
		return nil
	}
	body, err := fetchTeamsBootstrap(cmd, flags)
	if err != nil {
		return err
	}
	if !teamsIsOrdinaryChat(body, conversation) {
		return errors.New("conversation is not confirmed as a chat in the current, possibly partial inventory; channel routing is not supported")
	}
	return nil
}

func newTeamsMessageReplyCmd(flags *rootFlags) *cobra.Command {
	var conversation, text, cursor, cursorFile, planFile string
	var start int64
	var maxPages int
	var execute, confirm bool
	cmd := &cobra.Command{Use: "reply [message-id]", Short: "Preview or send a quote reply in a confirmed chat", Annotations: map[string]string{"mcp:read-only-unless-flags": "execute,confirm", "mcp:read-flags": "cursor-file,page-plan-file"}}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !validTeamsMessagePathID(conversation) || len(args) != 1 || !validTeamsMessagePathID(args[0]) || strings.TrimSpace(text) == "" {
			return usageErr(errors.New("reply requires --conversation, one message ID, and --text"))
		}
		if flags.dryRun || !execute {
			return flags.printJSON(cmd, map[string]any{"executed": false, "action": "quote_reply", "conversation": conversation, "message_id": args[0], "text": text, "note": "the selected chat and source are checked at execution; pass --execute --confirm to apply"})
		}
		if !confirm {
			return usageErr(errors.New("--execute requires --confirm"))
		}
		if cliutil.IsAnyHarness() {
			return errors.New("message writes are disabled in verification and dogfood harnesses")
		}
		c, err := flags.newClient()
		if err != nil {
			return err
		}
		principal, ok := teamsPrincipal(c.Config.AuthHeader())
		if c.Config.AuthSource != "web-session" || !ok {
			return authErr(errors.New("a Teams web session with account identity is required"))
		}
		if err := requireTeamsOrdinaryChat(cmd, flags, conversation); err != nil {
			return err
		}
		message, err := teamsMessageTarget(cmd, flags, c, conversation, args[0], cursor, cursorFile, planFile, start, maxPages)
		if err != nil {
			return err
		}
		payload, clientID, err := teamsQuotePayload(message, conversation, args[0], text, principal, time.Now())
		if err != nil {
			return err
		}
		ctx, cancel := boundCtx(cmd.Context(), flags)
		defer cancel()
		result, err := performTeamsMessageWrite(ctx, c, http.MethodPost, teamsMessagePath(conversation), payload)
		if err != nil {
			if strings.Contains(err.Error(), "outcome_unknown") {
				return fmt.Errorf("%w; client_message_id=%s", err, clientID)
			}
			return err
		}
		if result.Status != http.StatusCreated || !validTeamsSendReceipt(result.Body) {
			return apiErr(fmt.Errorf("outcome_unknown: Teams returned an unrecognized reply receipt; check the chat before trying again; client_message_id=%s", clientID))
		}
		id := messageIDFromLocation(result.Location, conversation)
		out := map[string]any{"executed": true, "status": "accepted", "verification": "not_checked", "client_message_id": clientID, "message_id": id, "reply_to_message_id": args[0]}
		if id != "" {
			page, fetchErr := fetchTeamsMessagesWithClient(cmd, flags, c, conversation, "", 0, 200)
			if fetchErr == nil {
				for _, found := range page.Messages {
					props, _ := found["properties"].(map[string]any)
					if found["id"] == id && found["clientmessageid"] == clientID && props["qtdMsgs"] == payload["properties"].(map[string]any)["qtdMsgs"] {
						out["verification"] = "quote_reference_in_history"
						break
					}
				}
			}
		}
		return flags.printJSON(cmd, out)
	}
	cmd.Flags().StringVar(&conversation, "conversation", "", "Confirmed chat ID; channel thread routing is not supported")
	cmd.Flags().StringVar(&text, "text", "", "Plain text for the quote reply")
	cmd.Flags().StringVar(&cursor, "sync-state", "", "Opaque sync state for the first selected page")
	cmd.Flags().StringVar(&cursorFile, "cursor-file", "", "Read the first page sync state from a local file")
	cmd.Flags().StringVar(&planFile, "page-plan-file", "", "Read additional explicit history selectors from a JSON file")
	cmd.Flags().Int64Var(&start, "start-time", 0, "First page start time as Unix milliseconds")
	cmd.Flags().IntVar(&maxPages, "max-pages", 1, "Maximum backward pages to search for the source message (1–10)")
	cmd.Flags().BoolVar(&execute, "execute", false, "Execute the previewed quote reply")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm the quote reply")
	return cmd
}

func teamsReactionPayload(add bool, now time.Time) map[string]any {
	emotion := map[string]any{"key": "like"}
	if add {
		emotion["value"] = now.UnixMilli()
	}
	return map[string]any{"emotions": emotion}
}

func newTeamsMessageReactionCmd(flags *rootFlags, action string) *cobra.Command {
	var conversation, cursor, cursorFile, planFile string
	var start int64
	var maxPages int
	var execute, confirm bool
	cmd := &cobra.Command{Use: action + " [message-id]", Short: "Preview or " + action + " a Like in a confirmed chat", Annotations: map[string]string{"mcp:read-only-unless-flags": "execute,confirm", "mcp:read-flags": "cursor-file,page-plan-file"}}
	cmd.Example = `teams-web-pp-cli messages reactions ` + action + ` "$MESSAGE_ID" --conversation 48:notes --json --no-learn`
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !validTeamsMessagePathID(conversation) || len(args) != 1 || !validTeamsMessagePathID(args[0]) {
			return usageErr(errors.New("reaction requires --conversation and one message ID"))
		}
		if flags.dryRun || !execute {
			return flags.printJSON(cmd, map[string]any{"executed": false, "action": action + "_reaction", "reaction": "like", "conversation": conversation, "message_id": args[0], "note": "the selected chat and source are checked at execution; pass --execute --confirm to apply"})
		}
		if !confirm {
			return usageErr(errors.New("--execute requires --confirm"))
		}
		if cliutil.IsAnyHarness() {
			return errors.New("message writes are disabled in verification and dogfood harnesses")
		}
		c, err := flags.newClient()
		if err != nil {
			return err
		}
		_, ok := teamsPrincipal(c.Config.AuthHeader())
		if c.Config.AuthSource != "web-session" || !ok {
			return authErr(errors.New("a Teams web session with account identity is required"))
		}
		if err := requireTeamsOrdinaryChat(cmd, flags, conversation); err != nil {
			return err
		}
		if _, err := teamsMessageTarget(cmd, flags, c, conversation, args[0], cursor, cursorFile, planFile, start, maxPages); err != nil {
			return err
		}
		add := action == "add"
		method := http.MethodDelete
		if add {
			method = http.MethodPut
		}
		path := teamsMessagePath(conversation) + "/" + url.PathEscape(args[0]) + "/properties?name=emotions"
		ctx, cancel := boundCtx(cmd.Context(), flags)
		defer cancel()
		result, err := performTeamsMessageWrite(ctx, c, method, path, teamsReactionPayload(add, time.Now()))
		if err != nil {
			return err
		}
		validReceipt := result.Status == http.StatusOK
		if add {
			var value int64
			validReceipt = validReceipt && json.Unmarshal(result.Body, &value) == nil && value > 0
		} else {
			validReceipt = validReceipt && bytes.Equal(bytes.TrimSpace(result.Body), []byte("null"))
		}
		if !validReceipt {
			return apiErr(errors.New("outcome_unknown: Teams returned an unrecognized reaction receipt; check the chat before trying again"))
		}
		return flags.printJSON(cmd, map[string]any{"executed": true, "status": "accepted", "verification": "not_checked", "action": action + "_reaction", "reaction": "like", "message_id": args[0]})
	}
	cmd.Flags().StringVar(&conversation, "conversation", "", "Confirmed chat ID; channel reactions are not supported")
	cmd.Flags().StringVar(&cursor, "sync-state", "", "Opaque sync state for the first selected page")
	cmd.Flags().StringVar(&cursorFile, "cursor-file", "", "Read the first page sync state from a local file")
	cmd.Flags().StringVar(&planFile, "page-plan-file", "", "Read additional explicit history selectors from a JSON file")
	cmd.Flags().Int64Var(&start, "start-time", 0, "First page start time as Unix milliseconds")
	cmd.Flags().IntVar(&maxPages, "max-pages", 1, "Maximum backward pages to search for the source message (1–10)")
	cmd.Flags().BoolVar(&execute, "execute", false, "Execute the previewed reaction change")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm the reaction change")
	return cmd
}

func newTeamsMessageReactionsCmd(flags *rootFlags) *cobra.Command {
	group := &cobra.Command{Use: "reactions", Short: "Manage observed chat reactions", Annotations: map[string]string{"pp:parent-group": "true"}, RunE: parentNoSubcommandRunE(flags)}
	group.AddCommand(newTeamsMessageReactionCmd(flags, "add"), newTeamsMessageReactionCmd(flags, "remove"))
	return group
}
