// Chat rich-text and chat-level read state based on observed web traffic.
package cli

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
)

func teamsBoldHTML(bold, suffix string) (string, error) {
	if strings.TrimSpace(bold) == "" || strings.TrimSpace(suffix) == "" || len(bold) > 10000 || len(suffix) > 10000 {
		return "", usageErr(errors.New("--bold-text and --text must be nonempty and at most 10000 bytes each"))
	}
	b := strings.ReplaceAll(html.EscapeString(strings.ReplaceAll(bold, "\r\n", "\n")), "\n", "<br>")
	s := strings.ReplaceAll(html.EscapeString(strings.ReplaceAll(suffix, "\r\n", "\n")), "\n", "<br>")
	return "<p><strong>" + b + "</strong>" + s + "</p>", nil
}

func teamsBoldTextFromHTML(content string) bool {
	const prefix = "<p><strong>"
	const middle = "</strong>"
	if !strings.HasPrefix(content, prefix) || !strings.HasSuffix(content, "</p>") {
		return false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(content, prefix), "</p>")
	if strings.Count(inner, middle) != 1 {
		return false
	}
	parts := strings.SplitN(inner, middle, 2)
	bold := html.UnescapeString(strings.ReplaceAll(parts[0], "<br>", "\n"))
	suffix := html.UnescapeString(strings.ReplaceAll(parts[1], "<br>", "\n"))
	canonical, err := teamsBoldHTML(bold, suffix)
	return err == nil && canonical == content
}

func teamsBoldSendPayload(conversation, bold, suffix string, principal teamsTokenPrincipal, now time.Time) (map[string]any, string, error) {
	content, err := teamsBoldHTML(bold, suffix)
	if err != nil {
		return nil, "", err
	}
	payload, clientID, err := teamsSendPayload(conversation, suffix, principal, now)
	if err != nil {
		return nil, "", err
	}
	payload["content"] = content
	return payload, clientID, nil
}

func newTeamsMessageRichSendCmd(flags *rootFlags) *cobra.Command {
	var conversation, bold, suffix string
	var execute, confirm bool
	cmd := &cobra.Command{Use: "send-rich", Short: "Preview or send one bold span in a confirmed chat", Annotations: map[string]string{"mcp:read-only-unless-flags": "execute,confirm"}}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !validTeamsMessagePathID(conversation) || len(args) != 0 {
			return usageErr(errors.New("send-rich requires --conversation and no positional arguments"))
		}
		if _, err := teamsBoldHTML(bold, suffix); err != nil {
			return err
		}
		if flags.dryRun || !execute {
			return flags.printJSON(cmd, map[string]any{"executed": false, "action": "send_rich", "conversation": conversation, "bold_text": bold, "text": suffix, "note": "the selected chat is checked at execution; pass --execute --confirm to apply"})
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
		payload, clientID, err := teamsBoldSendPayload(conversation, bold, suffix, principal, time.Now())
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
			return apiErr(fmt.Errorf("outcome_unknown: Teams returned an unrecognized rich-send receipt; check the chat before trying again; client_message_id=%s", clientID))
		}
		id := messageIDFromLocation(result.Location, conversation)
		out := map[string]any{"executed": true, "status": "accepted", "message_id": id, "client_message_id": clientID, "verification": "not_checked"}
		if id != "" {
			page, fetchErr := fetchTeamsMessagesWithClient(cmd, flags, c, conversation, "", 0, 200)
			if fetchErr == nil {
				for _, message := range page.Messages {
					if message["id"] == id && message["clientmessageid"] == clientID && message["content"] == payload["content"] {
						out["verification"] = "rich_content_in_history"
						break
					}
				}
			}
		}
		return flags.printJSON(cmd, out)
	}
	cmd.Flags().StringVar(&conversation, "conversation", "", "Confirmed chat ID; channel routing is not supported")
	cmd.Flags().StringVar(&bold, "bold-text", "", "Text in the bold span")
	cmd.Flags().StringVar(&suffix, "text", "", "Ordinary text after the bold span, including desired spacing")
	cmd.Flags().BoolVar(&execute, "execute", false, "Execute the previewed rich-text message")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm the rich-text message")
	return cmd
}

var teamsClientMessageID = regexp.MustCompile(`^[0-9]{19}$`)

// The observed unread request used the newest message's arrival minus one
// millisecond, a current timestamp, and its client message ID. The read
// request used zero, a current timestamp, and zero. This is a bounded
// inference from one paired self-chat capture, not a general Teams contract.
func teamsChatStatePayload(unread bool, message map[string]any, now time.Time) (map[string]any, error) {
	stamp := now.UnixMilli()
	if stamp <= 0 {
		return nil, errors.New("invalid current time")
	}
	first, last := int64(0), "0"
	if unread {
		arrival, _ := message["originalarrivaltime"].(string)
		at, err := time.Parse(time.RFC3339Nano, arrival)
		clientID, _ := message["clientmessageid"].(string)
		if err != nil || at.UnixMilli() <= 1 || at.UnixMilli() > stamp || !teamsClientMessageID.MatchString(clientID) {
			return nil, errors.New("latest self-chat message lacks the observed unread anchor fields")
		}
		first, last = at.UnixMilli()-1, clientID
	}
	return map[string]any{"consumptionHorizonBookmark": fmt.Sprintf("%d;%d;%s", first, stamp, last)}, nil
}

func teamsLatestChatMessage(page teamsMessagePage, conversation, target string) (map[string]any, error) {
	var latest map[string]any
	var latestTime time.Time
	for _, message := range page.Messages {
		if teamsMessageDeleted(message) {
			continue
		}
		arrival, _ := message["originalarrivaltime"].(string)
		at, err := time.Parse(time.RFC3339Nano, arrival)
		if err != nil {
			return nil, errors.New("chat history has a message with an unknown arrival time")
		}
		if latest != nil && at.Equal(latestTime) && message["id"] != latest["id"] {
			return nil, errors.New("chat has ambiguous latest message timestamps")
		}
		if latest == nil || at.After(latestTime) {
			latest, latestTime = message, at
		}
	}
	if latest == nil || latest["id"] != target || latest["conversationid"] != conversation {
		return nil, errors.New("the selected message is not the latest undeleted message in the current chat page")
	}
	return latest, nil
}

func newTeamsChatStateCmd(flags *rootFlags, unread bool) *cobra.Command {
	var chat string
	var execute, confirm bool
	action, use := "mark-read", "mark-read"
	if unread {
		action, use = "mark-unread", "mark-unread [latest-message-id]"
	}
	cmd := &cobra.Command{Use: use, Short: "Preview or " + action + " for the whole confirmed chat", Annotations: map[string]string{"mcp:read-only-unless-flags": "execute,confirm"}}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !validTeamsMessagePathID(chat) || (unread && (len(args) != 1 || !validTeamsMessagePathID(args[0]))) || (!unread && len(args) != 0) {
			return usageErr(errors.New("mark-read needs --chat; mark-unread also needs the latest message ID"))
		}
		preview := map[string]any{"executed": false, "action": action, "chat": chat, "effect": "change read state of the whole chat"}
		if unread {
			preview["latest_message_id"] = args[0]
		}
		if flags.dryRun || !execute {
			preview["note"] = "the selected chat is checked at execution; pass --execute --confirm to apply"
			return flags.printJSON(cmd, preview)
		}
		if !confirm {
			return usageErr(errors.New("--execute requires --confirm"))
		}
		if cliutil.IsAnyHarness() {
			return errors.New("chat state changes are disabled in verification and dogfood harnesses")
		}
		c, err := flags.newClient()
		if err != nil {
			return err
		}
		_, ok := teamsPrincipal(c.Config.AuthHeader())
		if c.Config.AuthSource != "web-session" || !ok {
			return authErr(errors.New("a Teams web session with account identity is required"))
		}
		if err := requireTeamsOrdinaryChat(cmd, flags, chat); err != nil {
			return err
		}
		var message map[string]any
		if unread {
			page, err := fetchTeamsMessagesWithClient(cmd, flags, c, chat, "", 0, 200)
			if err != nil {
				return err
			}
			message, err = teamsLatestChatMessage(page, chat, args[0])
			if err != nil {
				return err
			}
		}
		payload, err := teamsChatStatePayload(unread, message, time.Now())
		if err != nil {
			return err
		}
		path := "/api/chatsvc/amer/v1/users/ME/conversations/" + url.PathEscape(chat) + "/properties?name=consumptionHorizonBookmark"
		ctx, cancel := boundCtx(cmd.Context(), flags)
		defer cancel()
		result, err := performTeamsMessageWrite(ctx, c, http.MethodPut, path, payload)
		if err != nil {
			return err
		}
		if result.Status != http.StatusOK || len(bytes.TrimSpace(result.Body)) != 0 {
			return apiErr(errors.New("outcome_unknown: Teams returned an unrecognized chat-state receipt; inspect the chat before trying again"))
		}
		return flags.printJSON(cmd, map[string]any{"executed": true, "status": "accepted", "verification": "not_checked", "action": action, "chat": chat, "effect": "whole chat read state"})
	}
	cmd.Flags().StringVar(&chat, "chat", "", "Confirmed chat ID; channel routing is not supported")
	cmd.Flags().BoolVar(&execute, "execute", false, "Execute the previewed chat-state change")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm the chat-state change")
	return cmd
}

func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		var chats *cobra.Command
		for _, cmd := range root.Commands() {
			if cmd.Name() == "chats" {
				chats = cmd
				break
			}
		}
		if chats == nil {
			chats = &cobra.Command{Use: "chats", Short: "Browse Teams chats", Annotations: map[string]string{"pp:parent-group": "true"}, RunE: parentNoSubcommandRunE(flags)}
			root.AddCommand(chats)
		}
		addNovelCommandIfAbsent(chats, newTeamsChatStateCmd(flags, false))
		addNovelCommandIfAbsent(chats, newTeamsChatStateCmd(flags, true))
	})
}
