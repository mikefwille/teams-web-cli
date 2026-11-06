// Message writes based on observed Teams web traffic. Mutations are never retried.
package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
)

var teamsMessageLimiter = cliutil.NewAdaptiveLimiterAuto(1)
var teamsWriteDiagName = regexp.MustCompile(`^teams-write-diag-[a-f0-9]{16}\.json$`)

// Diagnostics are opt-in and can only be written into the private session
// directory. They contain no request headers or request body.
func openTeamsWriteDiagnostic() (*os.File, error) {
	name := os.Getenv("TEAMS_WEB_WRITE_DIAGNOSTIC")
	if name == "" {
		return nil, nil
	}
	dir, err := teamsSessionDir()
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(name) || filepath.Dir(name) != filepath.Clean(dir) || !teamsWriteDiagName.MatchString(filepath.Base(name)) {
		return nil, errors.New("invalid private write diagnostic path")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("unsafe private write diagnostic directory")
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errors.New("could not create private write diagnostic")
	}
	return f, nil
}

func recordTeamsWriteDiagnostic(f *os.File, stage, method, contentType, location string, status int, data []byte, failure error) {
	if f == nil {
		return
	}
	const limit = 65536
	truncated := len(data) > limit
	if truncated {
		data = data[:limit]
	}
	if len(location) > 8192 {
		location = location[:8192]
	}
	result := map[string]any{"version": 1, "stage": stage, "method": method, "http_status": status, "response_content_type": contentType, "response_location": location, "response_body": string(data), "response_truncated": truncated}
	if failure != nil {
		result["transport_error"] = failure.Error()
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return
	}
	if _, err = f.Write(append(encoded, '\n')); err == nil {
		_ = f.Sync()
	}
}

func teamsMessagePath(conversation string) string {
	return "/api/chatsvc/amer/v1/users/me/conversations/" + url.PathEscape(conversation) + "/messages"
}

func teamsMessageHTML(text string) string {
	return "<p>" + strings.ReplaceAll(html.EscapeString(strings.ReplaceAll(text, "\r\n", "\n")), "\n", "<br>") + "</p>"
}

func teamsSendPayload(conversation, text string, principal teamsTokenPrincipal, now time.Time) (map[string]any, string, error) {
	if principal.Object == "" || principal.Name == "" {
		return nil, "", errors.New("the Teams session does not identify the sender")
	}
	n, err := rand.Int(rand.Reader, big.NewInt(8_000_000_000_000_000_000))
	if err != nil {
		return nil, "", err
	}
	clientID := n.Add(n, big.NewInt(1_000_000_000_000_000_000)).String()
	timestamp := now.UTC().Format("2006-01-02T15:04:05.000Z")
	author := "8:orgid:" + principal.Object
	payload := map[string]any{
		"id": "-1", "type": "Message", "conversationid": conversation, "conversationLink": "blah/" + conversation,
		"from": author, "fromUserId": author, "composetime": timestamp, "originalarrivaltime": timestamp,
		"content": teamsMessageHTML(text), "messagetype": "RichText/Html", "contenttype": "Text",
		"imdisplayname": principal.Name, "clientmessageid": clientID, "callId": "", "state": 0, "version": "0",
		"amsreferences": []any{}, "crossPostChannels": []any{},
		"properties": map[string]any{"importance": "", "subject": "", "title": "", "cards": "[]", "links": "[]", "mentions": "[]", "onbehalfof": nil, "files": "[]", "policyViolation": nil, "formatVariant": "TEAMS"},
	}
	return payload, clientID, nil
}

// messageIDFromLocation inspects an observed response header; it never follows it.
func messageIDFromLocation(location, conversation string) string {
	u, err := url.Parse(location)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "amer.ng.msg.teams.microsoft.com") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(parts) != 7 || parts[0] != "v1" || parts[1] != "users" || !strings.EqualFold(parts[2], "me") || parts[3] != "conversations" || parts[5] != "messages" {
		return ""
	}
	gotConversation, e1 := url.PathUnescape(parts[4])
	id, e2 := url.PathUnescape(parts[6])
	if e1 != nil || e2 != nil || gotConversation != conversation || id == "" || strings.Contains(id, "/") {
		return ""
	}
	return id
}

type teamsWriteResult struct {
	Status   int
	Body     json.RawMessage
	Location string
}

func validTeamsSendReceipt(data []byte) bool {
	var receipt struct {
		OriginalArrivalTime int64 `json:"OriginalArrivalTime"`
	}
	return json.Unmarshal(data, &receipt) == nil && receipt.OriginalArrivalTime > 0
}

func performTeamsMessageWrite(ctx context.Context, c *client.Client, method, path string, body any) (teamsWriteResult, error) {
	if c == nil || c.HTTPClient == nil || c.Config == nil || c.Config.AuthSource != "web-session" || !teamsOrigin(c.BaseURL) {
		return teamsWriteResult{}, authErr(errors.New("a verified-origin Teams web session is required for message writes"))
	}
	if _, ok := c.HTTPClient.Transport.(teamsOriginTransport); !ok {
		return teamsWriteResult{}, errors.New("Teams session origin guard is unavailable")
	}
	diag, err := openTeamsWriteDiagnostic()
	if err != nil {
		return teamsWriteResult{}, err
	}
	if diag != nil {
		defer diag.Close()
	}
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return teamsWriteResult{}, err
		}
		requestBody = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, requestBody)
	if err != nil {
		return teamsWriteResult{}, err
	}
	req.Header.Set("Accept", "application/json")
	// The captured write, unlike the history GET, included the app origin.
	req.Header.Set("Origin", "https://teams.cloud.microsoft")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := teamsMessageLimiter.Wait(ctx); err != nil {
		return teamsWriteResult{}, err
	}
	httpClient := *c.HTTPClient
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := httpClient.Do(req)
	if err != nil {
		recordTeamsWriteDiagnostic(diag, "transport_error", method, "", "", 0, nil, err)
		return teamsWriteResult{}, apiErr(errors.New("outcome_unknown: the message request did not complete; check the conversation before trying again"))
	}
	defer resp.Body.Close()
	if remaining, reset, ok := cliutil.ParseRateLimitHeaders(resp.Header); ok {
		teamsMessageLimiter.ObserveHeaders(remaining, reset)
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1_000_001))
	recordTeamsWriteDiagnostic(diag, "response", method, resp.Header.Get("Content-Type"), resp.Header.Get("Location"), resp.StatusCode, data, readErr)
	if readErr != nil || len(data) > 1_000_000 {
		return teamsWriteResult{}, apiErr(errors.New("outcome_unknown: the message response could not be read; check the conversation before trying again"))
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		teamsMessageLimiter.OnRateLimit()
		return teamsWriteResult{}, rateLimitErr(&cliutil.RateLimitError{URL: "Teams message service", RetryAfter: cliutil.RetryAfter(resp)})
	}
	if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500 {
		return teamsWriteResult{}, apiErr(fmt.Errorf("outcome_unknown: Teams returned HTTP %d; check the conversation before trying again", resp.StatusCode))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return teamsWriteResult{}, classifyAPIErrorOnly(fmt.Errorf("Teams message request returned HTTP %d", resp.StatusCode))
	}
	if len(bytes.TrimSpace(data)) != 0 && !json.Valid(data) {
		return teamsWriteResult{}, apiErr(errors.New("outcome_unknown: Teams returned an unexpected message response; check the conversation before trying again"))
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) == nil {
		if raw := bytes.TrimSpace(envelope["error"]); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
			return teamsWriteResult{}, apiErr(errors.New("Teams reported a message error; check the conversation before trying again"))
		}
	}
	teamsMessageLimiter.OnSuccess()
	return teamsWriteResult{Status: resp.StatusCode, Body: data, Location: resp.Header.Get("Location")}, nil
}

func teamsOwnMessage(message map[string]any, principal teamsTokenPrincipal) bool {
	from, _ := message["from"].(string)
	const prefix = "https://teams.cloud.microsoft/api/chatsvc/amer/v1/users/ME/contacts/"
	return strings.EqualFold(from, prefix+"8:orgid:"+principal.Object) && principal.Object != ""
}

func teamsEmptyStructuredValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case bool:
		return !v
	case string:
		return v == "" || v == "[]" || v == "null"
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	default:
		return false
	}
}

func teamsEditPayload(message map[string]any, conversation, id, text string, principal teamsTokenPrincipal, now time.Time) (map[string]any, error) {
	return teamsEditPayloadContent(message, conversation, id, teamsMessageHTML(text), false, principal, now)
}

func teamsEditPayloadContent(message map[string]any, conversation, id, replacement string, allowBold bool, principal teamsTokenPrincipal, now time.Time) (map[string]any, error) {
	if !teamsOwnMessage(message, principal) || message["id"] != id || message["conversationid"] != conversation || message["messagetype"] != "RichText/Html" || message["contenttype"] != "Text" {
		return nil, errors.New("editing is limited to owned text messages in the selected conversation")
	}
	content, _ := message["content"].(string)
	if _, ok := teamsPlainTextFromHTML(content); !ok && (!allowBold || !teamsBoldTextFromHTML(content)) {
		return nil, errors.New("message contains unsupported rich text; refusing to overwrite it")
	}
	get := func(key string) (string, error) {
		value, ok := message[key].(string)
		if !ok || value == "" {
			return "", fmt.Errorf("message is missing %s; refusing to replace it", key)
		}
		return value, nil
	}
	composed, err := get("composetime")
	if err != nil {
		return nil, err
	}
	arrived, err := get("originalarrivaltime")
	if err != nil {
		return nil, err
	}
	version, err := get("version")
	if err != nil {
		return nil, err
	}
	clientID, err := get("clientmessageid")
	if err != nil {
		return nil, err
	}
	properties := map[string]any{"importance": "", "subject": "", "title": "", "cards": "[]", "links": "[]", "mentions": "[]", "onbehalfof": nil, "files": "[]", "policyViolation": nil, "formatVariant": "TEAMS"}
	for _, key := range []string{"amsreferences", "attachments"} {
		if value, exists := message[key]; exists && !teamsEmptyStructuredValue(value) {
			return nil, errors.New("message has structured content or attachments; refusing to overwrite them")
		}
	}
	if raw, exists := message["properties"]; exists && raw != nil {
		original, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("message properties have an unsupported format")
		}
		for key, value := range original {
			if key == "edittime" {
				continue
			}
			if strings.EqualFold(key, "deletetime") && !teamsEmptyStructuredValue(value) {
				return nil, errors.New("deleted messages cannot be edited")
			}
			lower := strings.ToLower(key)
			structured := lower == "onbehalfof" || lower == "qtdmsgs" || strings.Contains(lower, "mention") || strings.Contains(lower, "attachment") || strings.Contains(lower, "card") || strings.Contains(lower, "link") || strings.Contains(lower, "file")
			if structured && !teamsEmptyStructuredValue(value) {
				return nil, errors.New("message has structured content or attachments; refusing to overwrite them")
			}
			properties[key] = value
		}
	}
	properties["edittime"] = now.UnixMilli()
	author := "8:orgid:" + principal.Object
	return map[string]any{"id": id, "type": "Message", "conversationid": conversation, "conversationLink": "conversation/" + conversation + ";messageid=" + id,
		"from": "worker/" + author, "fromUserId": author, "composetime": composed, "originalarrivaltime": arrived, "content": replacement,
		"messagetype": "RichText/Html", "contenttype": "Text", "imdisplayname": principal.Name, "clientmessageid": clientID, "state": 0, "version": version,
		"amsreferences": []any{}, "properties": properties, "crossPostChannels": []any{}}, nil
}

func configureTeamsMessageWrite(cmd *cobra.Command, flags *rootFlags, action string) {
	var conversation, text, syncState, boldText string
	var start int64
	var execute, confirm bool
	cmd.Short = "Preview or confirm a Teams message " + action
	cmd.Annotations = map[string]string{"mcp:read-only-unless-flags": "execute,confirm"}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !validTeamsMessagePathID(conversation) {
			return usageErr(errors.New("--conversation must be a valid opaque conversation ID"))
		}
		if action == "send" && (len(args) != 0 || strings.TrimSpace(text) == "") {
			return usageErr(errors.New("send needs --text and no positional arguments"))
		}
		if (action == "delete" || action == "edit") && (len(args) != 1 || !validTeamsMessagePathID(args[0])) {
			return usageErr(errors.New(action + " needs one message ID"))
		}
		if action == "edit" && strings.TrimSpace(text) == "" {
			return usageErr(errors.New("edit needs --text"))
		}
		replacement := teamsMessageHTML(text)
		boldEdit := action == "edit" && cmd.Flags().Changed("bold-text")
		if boldEdit {
			var err error
			replacement, err = teamsBoldHTML(boldText, text)
			if err != nil {
				return err
			}
		}
		preview := map[string]any{"executed": false, "action": action, "conversation": conversation}
		if action == "send" {
			preview["text"] = text
		} else {
			preview["message_id"] = args[0]
			if action == "delete" {
				preview["effect"] = "soft delete; a tombstone may remain"
			} else {
				preview["text"] = text
				preview["effect"] = "replace the message text"
				if boldEdit {
					preview["bold_text"] = boldText
					preview["effect"] = "replace the message with one bold span and ordinary text"
				}
			}
		}
		if flags.dryRun || !execute {
			preview["note"] = "pass --execute --confirm to apply"
			return flags.printJSON(cmd, preview)
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
		if c.Config.AuthSource != "web-session" {
			return authErr(errors.New("a Teams web session is required"))
		}
		principal, ok := teamsPrincipal(c.Config.AuthHeader())
		if !ok {
			return authErr(errors.New("Teams session identity is unavailable"))
		}
		path := teamsMessagePath(conversation)
		ctx, cancel := boundCtx(cmd.Context(), flags)
		defer cancel()
		if action == "send" {
			payload, clientID, err := teamsSendPayload(conversation, text, principal, time.Now())
			if err != nil {
				return err
			}
			result, err := performTeamsMessageWrite(ctx, c, http.MethodPost, path, payload)
			if err != nil {
				if strings.Contains(err.Error(), "outcome_unknown") {
					return fmt.Errorf("%w; client_message_id=%s", err, clientID)
				}
				return err
			}
			if result.Status != http.StatusCreated || !validTeamsSendReceipt(result.Body) {
				return apiErr(fmt.Errorf("outcome_unknown: Teams returned an unrecognized send receipt; check the conversation before trying again; client_message_id=%s", clientID))
			}
			id := messageIDFromLocation(result.Location, conversation)
			out := map[string]any{"executed": true, "status": "accepted", "verification": "not_checked", "client_message_id": clientID, "message_id": id}
			if id != "" {
				page, fetchErr := fetchTeamsMessages(cmd, flags, conversation, "", 0, 200)
				if fetchErr == nil {
					for _, message := range page.Messages {
						if message["id"] == id && message["clientmessageid"] == clientID {
							out["verification"] = "found_in_history"
							break
						}
					}
				}
			}
			return flags.printJSON(cmd, out)
		}
		page, err := fetchTeamsMessages(cmd, flags, conversation, syncState, start, 200)
		if err != nil {
			return err
		}
		var owned map[string]any
		for _, message := range page.Messages {
			if message["id"] == args[0] && teamsOwnMessage(message, principal) {
				owned = message
				break
			}
		}
		if owned == nil {
			return errors.New("message was not found as an owned message in this page; provide its sync state or start time")
		}
		if action == "edit" {
			payload, err := teamsEditPayloadContent(owned, conversation, args[0], replacement, boldEdit, principal, time.Now())
			if err != nil {
				return err
			}
			result, err := performTeamsMessageWrite(ctx, c, http.MethodPut, path+"/"+url.PathEscape(args[0]), payload)
			if err != nil {
				return err
			}
			if result.Status != http.StatusOK {
				return apiErr(errors.New("outcome_unknown: Teams returned an unrecognized edit receipt; check the conversation before trying again"))
			}
			out := map[string]any{"executed": true, "status": "accepted", "message_id": args[0], "verification": "not_checked"}
			if after, fetchErr := fetchTeamsMessages(cmd, flags, conversation, syncState, start, 200); fetchErr == nil {
				for _, message := range after.Messages {
					if message["id"] == args[0] && message["content"] == replacement {
						out["verification"] = "edited_text_in_history"
						break
					}
				}
			}
			return flags.printJSON(cmd, out)
		}
		result, err := performTeamsMessageWrite(ctx, c, http.MethodDelete, path+"/"+url.PathEscape(args[0])+"?behavior=softDelete", nil)
		if err != nil {
			return err
		}
		if result.Status != http.StatusOK || !bytes.Equal(bytes.TrimSpace(result.Body), []byte("null")) {
			return apiErr(errors.New("outcome_unknown: Teams returned an unrecognized delete receipt; check the conversation before trying again"))
		}
		out := map[string]any{"executed": true, "status": "accepted", "message_id": args[0], "verification": "not_checked", "effect": "soft delete; a tombstone may remain"}
		if after, fetchErr := fetchTeamsMessages(cmd, flags, conversation, syncState, start, 200); fetchErr == nil {
			for _, message := range after.Messages {
				if message["id"] == args[0] {
					if p, ok := message["properties"].(map[string]any); ok && p["deletetime"] != nil {
						out["verification"] = "tombstone_in_history"
					}
					break
				}
			}
		}
		return flags.printJSON(cmd, out)
	}
	cmd.Flags().StringVar(&conversation, "conversation", "", "Opaque conversation ID")
	cmd.Flags().BoolVar(&execute, "execute", false, "Execute the previewed operation")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm the operation")
	if action == "send" || action == "edit" {
		cmd.Flags().StringVar(&text, "text", "", "Plain text message")
	}
	if action == "edit" || action == "delete" {
		cmd.Flags().StringVar(&syncState, "sync-state", "", "Opaque history sync state containing this message")
		cmd.Flags().Int64Var(&start, "start-time", 0, "History start time as Unix milliseconds")
	}
	if action == "edit" {
		cmd.Flags().StringVar(&boldText, "bold-text", "", "Replace with a bold span followed by --text; preserves supported bold structure")
	}
}

func newTeamsMessageSendCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "send"}
	configureTeamsMessageWrite(cmd, flags, "send")
	return cmd
}

func newTeamsMessageEditCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "edit [message-id]"}
	configureTeamsMessageWrite(cmd, flags, "edit")
	return cmd
}

func newTeamsMessageDeleteCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "delete [message-id]"}
	configureTeamsMessageWrite(cmd, flags, "delete")
	return cmd
}
