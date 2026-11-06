package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestTeamsBoldSendPayloadUsesObservedShapeAndEscapesText(t *testing.T) {
	principal := teamsTokenPrincipal{Object: "synthetic-user", Name: "Synthetic User"}
	payload, clientID, err := teamsBoldSendPayload(teamsSelfChat, "Bold <part>", " & regular", principal, time.UnixMilli(2000))
	if err != nil || len(clientID) != 19 || payload["content"] != "<p><strong>Bold &lt;part&gt;</strong> &amp; regular</p>" || payload["conversationid"] != teamsSelfChat {
		t.Fatalf("rich payload: %#v %v", payload, err)
	}
	if payload["properties"].(map[string]any)["mentions"] != "[]" {
		t.Fatal("unexpected mention metadata")
	}
	for _, input := range []struct{ bold, suffix string }{{"", "normal"}, {"bold", ""}} {
		if _, _, err := teamsBoldSendPayload(teamsSelfChat, input.bold, input.suffix, principal, time.Now()); err == nil {
			t.Fatal("accepted an unsupported rich-text shape")
		}
	}
}

func TestTeamsChatStatePayloadUsesObservedSelfChatBookmarkShape(t *testing.T) {
	message, _, _ := syntheticQuoteMessage()
	message["clientmessageid"] = "1234567890123456789"
	arrival, _ := time.Parse(time.RFC3339Nano, message["originalarrivaltime"].(string))
	now := arrival.Add(time.Minute)
	unread, err := teamsChatStatePayload(true, message, now)
	if err != nil || unread["consumptionHorizonBookmark"] != fmt.Sprintf("%d;%d;1234567890123456789", arrival.UnixMilli()-1, now.UnixMilli()) {
		t.Fatalf("unread payload: %#v %v", unread, err)
	}
	read, err := teamsChatStatePayload(false, nil, now)
	if err != nil || read["consumptionHorizonBookmark"] != fmt.Sprintf("0;%d;0", now.UnixMilli()) {
		t.Fatalf("read payload: %#v %v", read, err)
	}
	message["clientmessageid"] = "not-numeric"
	if _, err := teamsChatStatePayload(true, message, now); err == nil {
		t.Fatal("invalid client message ID accepted")
	}
}

func TestTeamsUnreadRequiresLatestUndeletedMessage(t *testing.T) {
	message, _, id := syntheticQuoteMessage()
	older := map[string]any{"id": "older", "originalarrivaltime": "2026-11-06T11:00:00Z"}
	deleted := map[string]any{"id": "deleted", "originalarrivaltime": "2026-11-06T13:00:00Z", "properties": map[string]any{"deletetime": "1"}}
	page := teamsMessagePage{Messages: []map[string]any{older, message, deleted}}
	if found, err := teamsLatestChatMessage(page, teamsSelfChat, id); err != nil || found["id"] != id {
		t.Fatalf("latest selection: %#v %v", found, err)
	}
	if _, err := teamsLatestChatMessage(page, teamsSelfChat, "older"); err == nil {
		t.Fatal("older target accepted")
	}
	page.Messages = append(page.Messages, map[string]any{"id": "unknown"})
	if _, err := teamsLatestChatMessage(page, teamsSelfChat, id); err == nil {
		t.Fatal("unknown timestamp accepted")
	}
}

func TestTeamsRichEditPreservesSupportedShapeAndRejectsOtherStructure(t *testing.T) {
	message, principal, id := syntheticQuoteMessage()
	message["content"] = "<p><strong>before</strong> normal</p>"
	message["composetime"] = message["originalarrivaltime"]
	message["version"] = "1"
	message["clientmessageid"] = "1234567890123456789"
	message["properties"].(map[string]any)["serviceMetadata"] = "keep"
	replacement, err := teamsBoldHTML("after\nline", " & normal")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := teamsEditPayloadContent(message, teamsSelfChat, id, replacement, true, principal, time.Now())
	if err != nil || payload["content"] != "<p><strong>after<br>line</strong> &amp; normal</p>" || payload["properties"].(map[string]any)["serviceMetadata"] != "keep" {
		t.Fatalf("rich edit: %#v %v", payload, err)
	}
	if _, err := teamsEditPayload(message, teamsSelfChat, id, "plain replacement", principal, time.Now()); err == nil {
		t.Fatal("plain edit silently replaced a bold message")
	}
	message["content"] = "<p><em>unrecognized</em></p>"
	if _, err := teamsEditPayloadContent(message, teamsSelfChat, id, replacement, true, principal, time.Now()); err == nil {
		t.Fatal("unrecognized rich structure accepted")
	}
}

func TestTeamsRichAndChatStatePreviewRequireSelfChatAndConfirmation(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	t.Setenv("TEAMS_WEB_SESSION_DIR", t.TempDir())
	for _, tc := range []struct {
		args    []string
		wantErr bool
	}{
		{[]string{"messages", "send-rich", "--conversation", "48:notes", "--bold-text", "Bold", "--text", " normal", "--json", "--no-learn"}, false},
		{[]string{"chats", "mark-unread", "123", "--chat", "48:notes", "--json", "--no-learn"}, false},
		{[]string{"chats", "mark-read", "--chat", "48:notes", "--execute", "--json", "--no-learn"}, true},
		{[]string{"chats", "mark-read", "--chat", "other", "--json", "--no-learn"}, false},
		{[]string{"chats", "list", "--dry-run", "--json", "--no-learn"}, false},
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
			if json.Unmarshal(out.Bytes(), &result) != nil {
				t.Fatalf("invalid preview: %s", out.String())
			}
			if tc.args[0] != "chats" || tc.args[1] != "list" {
				if result["executed"] != false {
					t.Fatalf("not a preview: %s", out.String())
				}
			}
		}
	}
}
