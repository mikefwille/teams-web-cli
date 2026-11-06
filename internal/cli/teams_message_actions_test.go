package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func syntheticQuoteMessage() (map[string]any, teamsTokenPrincipal, string) {
	principal := teamsTokenPrincipal{Object: "synthetic-user", Name: "Synthetic User"}
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	id := strconv.FormatInt(at.UnixMilli(), 10)
	message := map[string]any{
		"id": id, "conversationid": teamsSelfChat,
		"from":          "https://teams.cloud.microsoft/api/chatsvc/amer/v1/users/ME/contacts/8:orgid:synthetic-user",
		"imdisplayname": "A & B", "originalarrivaltime": at.Format(time.RFC3339Nano),
		"messagetype": "RichText/Html", "contenttype": "Text", "content": teamsMessageHTML("original <text>"),
		"properties": map[string]any{"mentions": "[]", "files": "[]"},
	}
	return message, principal, id
}

func TestTeamsQuotePayloadMatchesObservedShapeAndEscapesText(t *testing.T) {
	message, principal, id := syntheticQuoteMessage()
	payload, clientID, err := teamsQuotePayload(message, teamsSelfChat, id, "reply & <more>", principal, time.UnixMilli(1000))
	if err != nil || len(clientID) != 19 {
		t.Fatalf("quote: %v", err)
	}
	content := payload["content"].(string)
	for _, fragment := range []string{"<blockquote itemscope", `itemtype="http://schema.skype.com/Reply"`, "A &amp; B", "original &lt;text&gt;", "<p>reply &amp; &lt;more&gt;</p>"} {
		if !strings.Contains(content, fragment) {
			t.Fatalf("missing %q in %q", fragment, content)
		}
	}
	var quotes []struct {
		MessageID string `json:"messageId"`
		Sender    string `json:"sender"`
		Time      int64  `json:"time"`
	}
	value := payload["properties"].(map[string]any)["qtdMsgs"].(string)
	if json.Unmarshal([]byte(value), &quotes) != nil || len(quotes) != 1 || quotes[0].MessageID != id || quotes[0].Sender != "8:orgid:synthetic-user" || strconv.FormatInt(quotes[0].Time, 10) != id {
		t.Fatalf("quote metadata: %s", value)
	}
}

func TestTeamsQuoteRefusesUnsupportedSources(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"other conversation": func(m map[string]any) { m["conversationid"] = "other" },
		"other author":       func(m map[string]any) { m["from"] = "someone-else" },
		"deleted":            func(m map[string]any) { m["properties"].(map[string]any)["deletetime"] = "123" },
		"formatted":          func(m map[string]any) { m["content"] = "<p><b>formatted</b></p>" },
		"nested quote":       func(m map[string]any) { m["properties"].(map[string]any)["qtdMsgs"] = `[{}]` },
		"invalid timestamp":  func(m map[string]any) { m["originalarrivaltime"] = "invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			message, principal, id := syntheticQuoteMessage()
			mutate(message)
			if _, _, err := teamsQuotePayload(message, teamsSelfChat, id, "reply", principal, time.Now()); err == nil {
				t.Fatal("unsupported source accepted")
			}
		})
	}
}

func TestTeamsQuoteUsesSourceSenderAndAllowsMultiline(t *testing.T) {
	message, principal, id := syntheticQuoteMessage()
	message["conversationid"] = "chat-a"
	message["from"] = "https://teams.cloud.microsoft/api/chatsvc/amer/v1/users/ME/contacts/8:orgid:other-user"
	message["content"] = teamsMessageHTML("first\nsecond")
	payload, _, err := teamsQuotePayload(message, "chat-a", id, "reply\nnext", principal, time.Now())
	if err != nil || !strings.Contains(payload["content"].(string), "first<br>second") || !strings.Contains(payload["content"].(string), "reply<br>next") {
		t.Fatalf("multiline quote: %#v %v", payload, err)
	}
	var quoted []map[string]any
	data := payload["properties"].(map[string]any)["qtdMsgs"].(string)
	if json.Unmarshal([]byte(data), &quoted) != nil || len(quoted) != 1 || quoted[0]["sender"] != "8:orgid:other-user" || payload["conversationid"] != "chat-a" {
		t.Fatalf("quote used the wrong sender or chat: %#v", quoted)
	}
}

func TestTeamsQuotePreservesSafePlainHTMLSerialization(t *testing.T) {
	for _, body := range []string{"first<br>\nsecond", "first<br>&nbsp;second", "first<br>\r\nsecond", "literal &lt;word&gt;<br>second"} {
		t.Run(body, func(t *testing.T) {
			message, principal, id := syntheticQuoteMessage()
			message["content"] = "<p>" + body + "</p>"
			payload, _, err := teamsQuotePayload(message, teamsSelfChat, id, "reply", principal, time.Now())
			if err != nil || !strings.Contains(payload["content"].(string), `<p itemprop="preview">`+body+`</p>`) {
				t.Fatalf("plain serialization was rejected or changed: %v", err)
			}
		})
	}
	message, principal, id := syntheticQuoteMessage()
	message["content"] = `<p>first<br><img src="https://evil.invalid/x"></p>`
	if _, _, err := teamsQuotePayload(message, teamsSelfChat, id, "reply", principal, time.Now()); err == nil {
		t.Fatal("unsupported HTML tag passed the plain-content guard")
	}
}

func TestTeamsOrdinaryChatCheckRejectsChannelAndUnknown(t *testing.T) {
	body := teamsBootstrap{Chats: []map[string]any{{"id": "chat-a"}, {"id": "ambiguous"}}, Channels: []map[string]any{{"id": "ambiguous"}, {"id": "channel-a"}}}
	if !teamsIsOrdinaryChat(body, "chat-a") || !teamsIsOrdinaryChat(body, teamsSelfChat) || teamsIsOrdinaryChat(body, "channel-a") || teamsIsOrdinaryChat(body, "ambiguous") || teamsIsOrdinaryChat(body, "missing") {
		t.Fatal("chat inventory classification failed")
	}
}

func TestTeamsReactionPayloadAndNoAmbiguousRetry(t *testing.T) {
	add := teamsReactionPayload(true, time.UnixMilli(1234))["emotions"].(map[string]any)
	remove := teamsReactionPayload(false, time.UnixMilli(1234))["emotions"].(map[string]any)
	if add["key"] != "like" || add["value"] != int64(1234) || remove["key"] != "like" || len(remove) != 1 {
		t.Fatalf("reaction payloads: %#v %#v", add, remove)
	}
	old := teamsMessageLimiter
	teamsMessageLimiter = nil
	defer func() { teamsMessageLimiter = old }()
	transport := &messageWriteTransport{err: errors.New("synthetic transport failure")}
	c := messageWriteClient(transport)
	_, err := performTeamsMessageWrite(context.Background(), c, http.MethodPut, teamsMessagePath(teamsSelfChat)+"/123/properties?name=emotions", teamsReactionPayload(true, time.Now()))
	if err == nil || !strings.Contains(err.Error(), "outcome_unknown") || transport.calls != 1 || transport.method != http.MethodPut {
		t.Fatalf("reaction retry or error: %v, calls=%d", err, transport.calls)
	}
	var body map[string]map[string]any
	if json.Unmarshal(transport.body, &body) != nil || body["emotions"]["key"] != "like" {
		t.Fatalf("reaction body: %s", transport.body)
	}
	transport = &messageWriteTransport{response: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`null`))}}
	c = messageWriteClient(transport)
	if _, err := performTeamsMessageWrite(context.Background(), c, http.MethodDelete, teamsMessagePath(teamsSelfChat)+"/123/properties?name=emotions", teamsReactionPayload(false, time.Now())); err != nil || transport.calls != 1 || transport.method != http.MethodDelete {
		t.Fatalf("reaction removal: %v calls=%d", err, transport.calls)
	}
}

func TestTeamsReplyAndReactionRequireSelfChatAndExplicitConfirmation(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	t.Setenv("TEAMS_WEB_SESSION_DIR", t.TempDir())
	for _, tc := range []struct {
		args    []string
		wantErr bool
	}{
		{[]string{"messages", "reply", "123", "--conversation", "48:notes", "--text", "reply", "--json", "--no-learn"}, false},
		{[]string{"messages", "reactions", "add", "123", "--conversation", "48:notes", "--json", "--no-learn"}, false},
		{[]string{"messages", "reactions", "remove", "123", "--conversation", "48:notes", "--execute", "--json", "--no-learn"}, true},
		{[]string{"messages", "reply", "123", "--conversation", "other", "--text", "reply", "--json", "--no-learn"}, false},
	} {
		root := newRootCmd(&rootFlags{})
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(tc.args)
		err := root.Execute()
		if (err != nil) != tc.wantErr {
			t.Fatalf("args %v: %v", tc.args, err)
		}
		if !tc.wantErr {
			var result map[string]any
			if json.Unmarshal(out.Bytes(), &result) != nil || result["executed"] != false {
				t.Fatalf("not a preview: %s", out.String())
			}
		}
	}
}
