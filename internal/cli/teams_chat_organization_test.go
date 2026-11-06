package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestTeamsChatDiscoveryIncludesObservedNoteShapesWithoutContent(t *testing.T) {
	body := teamsBootstrap{
		Chats: []map[string]any{{"id": "19:ordinary-chat@thread.v2", "threadType": "chat"}},
		PrivateFeeds: []map[string]any{
			{"id": "48:synthetic-notes", "type": "Conversation", "threadType": "streamofnotes", "streamType": "streamofnotes", "lastMessage": map[string]any{"content": "never-copy-this"}},
			{"id": "unsupported", "type": "Conversation", "threadType": "unknown", "streamType": "unknown"},
		},
		ConversationFolders: json.RawMessage(`{"conversationFolders":[{"id":"section-a","isDeleted":false,"conversationFolderItems":[{"conversationId":"48:synthetic-notes","threadType":"streamofnotes"},{"conversationId":"48:another-note","threadType":"streamofnotes"},{"conversationId":"19:channel","threadType":"channel"}]}]}`),
	}
	rows := collectTeamsChats(body)
	if len(rows) != 3 || teamsObjectID(rows[0]) != "19:ordinary-chat@thread.v2" || teamsObjectID(rows[1]) != "48:synthetic-notes" || teamsObjectID(rows[2]) != "48:another-note" {
		t.Fatalf("unexpected bounded discovery: %#v", rows)
	}
	encoded, _ := json.Marshal(rows)
	if bytes.Contains(encoded, []byte("never-copy-this")) || bytes.Contains(encoded, []byte("unsupported")) || bytes.Contains(encoded, []byte("19:channel")) {
		t.Fatalf("unobserved type or message content leaked into added references: %s", encoded)
	}
}

func TestTeamsFavoriteStateRequiresCompleteUnambiguousSnapshot(t *testing.T) {
	good := `{"folderHierarchyVersion":123,"isPartialResponse":false,"conversationFolders":[{"id":"folder:ordinary","folderType":"Favorites","isDeleted":false,"conversationFolderItems":[{"conversationId":"19:ordinary-chat@thread.v2","threadType":"chat"}]}]}`
	state, err := teamsReadFavoriteState(json.RawMessage(good), "19:ordinary-chat@thread.v2")
	if err != nil || state.Version != 123 || state.FolderID != "folder:ordinary" || !state.Present {
		t.Fatalf("unexpected state: %#v %v", state, err)
	}
	state, err = teamsReadFavoriteState(json.RawMessage(good), "19:another-chat@thread.v2")
	if err != nil || state.Present {
		t.Fatalf("missing item was not distinguishable: %#v %v", state, err)
	}
	for _, invalid := range []string{
		`{}`,
		`{"folderHierarchyVersion":123,"isPartialResponse":true,"conversationFolders":[]}`,
		`{"folderHierarchyVersion":123,"conversationFolders":[]}`,
		`{"folderHierarchyVersion":123,"isPartialResponse":false,"conversationFolders":[{"id":"f","folderType":"Favorites","isDeleted":false}]}`,
		`{"folderHierarchyVersion":123,"isPartialResponse":false,"conversationFolders":[{"id":"f","folderType":"Favorites","isDeleted":false,"conversationFolderItems":[]},{"id":"g","folderType":"Favorites","isDeleted":false,"conversationFolderItems":[]}]}`,
		`{"folderHierarchyVersion":123,"isPartialResponse":false,"conversationFolders":[{"id":"f","folderType":"Favorites","isDeleted":false,"conversationFolderItems":[]},{"id":"g","folderType":"Custom","isDeleted":false,"conversationFolderItems":[{"conversationId":"19:ordinary-chat@thread.v2"}]}]}`,
		`{"folderHierarchyVersion":123,"isPartialResponse":false,"conversationFolders":[{"id":"f","folderType":"Favorites","isDeleted":false,"conversationFolderItems":[{"conversationId":"19:ordinary-chat@thread.v2"},{"conversationId":"19:ordinary-chat@thread.v2"}]}]}`,
	} {
		if _, err := teamsReadFavoriteState(json.RawMessage(invalid), "19:ordinary-chat@thread.v2"); err == nil {
			t.Fatalf("unsafe snapshot accepted: %s", invalid)
		}
	}
}

func TestTeamsFavoriteResultDoesNotClaimRenderedPlacement(t *testing.T) {
	for _, present := range []bool{false, true} {
		result := teamsFavoriteMembershipResult(teamsFavoriteState{Version: 123, Present: present}, "48:synthetic-notes")
		if result["favorite_membership_record"] != present || result["rendered_section"] != "not_determined" || result["folder_hierarchy_version"] != int64(123) {
			t.Fatalf("stored membership was confused with rendered placement: %#v", result)
		}
		if _, ok := result["favorite"]; ok {
			t.Fatalf("ambiguous favorite field returned: %#v", result)
		}
	}
}

type favoriteTestTransport struct {
	calls  int
	status int
	body   string
	err    error
	req    *http.Request
	sent   []byte
}

func (t *favoriteTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls++
	t.req = req.Clone(req.Context())
	t.sent, _ = io.ReadAll(req.Body)
	if t.err != nil {
		return nil, t.err
	}
	return &http.Response{StatusCode: t.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(t.body))}, nil
}

func TestTeamsFavoriteWriteUsesCapturedShapeAndNeverRetries(t *testing.T) {
	old := teamsFavoriteLimiter
	teamsFavoriteLimiter = nil
	defer func() { teamsFavoriteLimiter = old }()
	state := teamsFavoriteState{FolderID: "folder:ordinary", Version: 123}
	conversation := "19:ordinary-chat@thread.v2"
	response := `{"folderHierarchyVersion":124,"isPartialResponse":false,"conversationFolders":[{"id":"folder:ordinary","folderType":"Favorites","isDeleted":false,"conversationFolderItems":[{"conversationId":"19:ordinary-chat@thread.v2"}]}]}`
	transport := &favoriteTestTransport{status: 200, body: response}
	c := messageWriteClient(&messageWriteTransport{})
	c.HTTPClient.Transport = teamsOriginTransport{next: transport, scopes: map[string]map[string]string{"csa": {"authorization": "Bearer synthetic"}}}
	if err := performTeamsFavoriteWrite(context.Background(), c, state, conversation, true); err != nil {
		t.Fatal(err)
	}
	if transport.calls != 1 || transport.req.Method != http.MethodPost || transport.req.URL.Path != teamsConversationFoldersPath || transport.req.URL.Query().Get("supportsSliceItems") != "true" || transport.req.URL.Query().Get("supportsAdditionalSystemGeneratedFolders") != "true" || transport.req.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("unexpected Favorites request: %#v", transport.req)
	}
	var sent teamsFavoritePayload
	if json.Unmarshal(transport.sent, &sent) != nil || len(sent.Actions) != 1 || sent.Actions[0] != (teamsFavoriteAction{Action: "AddItem", FolderID: state.FolderID, ItemID: conversation}) || sent.FolderHierarchyVersion != state.Version || !sent.SupportsAdditionalSystemFolders || !sent.SupportsSliceItems {
		t.Fatalf("unexpected Favorites body: %s", transport.sent)
	}
	for _, tc := range []favoriteTestTransport{
		{err: errors.New("synthetic secret-bearing transport error")},
		{status: 500, body: "private error body"},
		{status: 200, body: `{}`},
		{status: 302, body: ""},
	} {
		testTransport := tc
		c.HTTPClient.Transport = teamsOriginTransport{next: &testTransport}
		err := performTeamsFavoriteWrite(context.Background(), c, state, conversation, true)
		if err == nil || !strings.Contains(err.Error(), "outcome_unknown") || testTransport.calls != 1 || strings.Contains(err.Error(), "secret-bearing") || strings.Contains(err.Error(), "private error body") {
			t.Fatalf("ambiguous write was retried or leaked data: %v, calls=%d", err, testTransport.calls)
		}
	}
	if body := teamsFavoriteBody(state, conversation, false); len(body.Actions) != 1 || body.Actions[0].Action != "RemoveItem" {
		t.Fatalf("unexpected inverse: %#v", body)
	}
}

func TestTeamsFavoritePreviewAndConfirmationNeverSend(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	t.Setenv("TEAMS_WEB_SESSION_DIR", t.TempDir())
	for _, tc := range []struct {
		args    []string
		wantErr bool
	}{
		{[]string{"chats", "favorite", "--conversation", "19:ordinary-chat@thread.v2", "--json", "--no-learn"}, false},
		{[]string{"chats", "unfavorite", "--conversation", "48:synthetic-notes", "--if-version", "123", "--confirm", "--json", "--no-learn"}, false},
		{[]string{"chats", "favorite", "--conversation", "19:ordinary-chat@thread.v2", "--execute", "--json", "--no-learn"}, true},
		{[]string{"chats", "favorite", "--conversation", "bad\nvalue", "--json", "--no-learn"}, true},
		{[]string{"chats", "favorite", "--conversation", "valid", "--if-version", "-1", "--json", "--no-learn"}, true},
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
		if err == nil {
			var result map[string]any
			if json.Unmarshal(out.Bytes(), &result) != nil || result["executed"] != false {
				t.Fatalf("not a preview: %s", out.String())
			}
		}
	}
}
