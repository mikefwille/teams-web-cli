package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
	"github.com/mikefwille/teams-web-cli/internal/config"
)

type messageWriteTransport struct {
	calls    int
	response *http.Response
	err      error
	body     []byte
	method   string
	headers  http.Header
}

func (t *messageWriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls++
	t.method = req.Method
	t.headers = req.Header.Clone()
	if req.Body != nil {
		t.body, _ = io.ReadAll(req.Body)
	}
	if t.err != nil {
		return nil, t.err
	}
	return t.response, nil
}

func TestTeamsMessageWriteUsesCapturedAudienceAndWriteHeaders(t *testing.T) {
	old := teamsMessageLimiter
	teamsMessageLimiter = nil
	defer func() { teamsMessageLimiter = old }()
	transport := &messageWriteTransport{response: &http.Response{StatusCode: 201, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"OriginalArrivalTime":"2026-01-01T00:00:00Z"}`))}}
	c := messageWriteClient(transport)
	c.HTTPClient.Transport = teamsOriginTransport{next: transport, scopes: map[string]map[string]string{"chatsvc": {"authorization": "Bearer synthetic", "content-type": "text/plain"}}}
	_, err := performTeamsMessageWrite(context.Background(), c, http.MethodPost, "/api/chatsvc/amer/v1/users/me/conversations/test/messages", map[string]any{"content": "message"})
	if err != nil || transport.calls != 1 || transport.method != http.MethodPost || transport.headers.Get("Authorization") != "Bearer synthetic" || transport.headers.Get("Content-Type") != "application/json" || transport.headers.Get("Origin") != "https://teams.cloud.microsoft" {
		t.Fatalf("unexpected write transport: err=%v calls=%d headers=%v", err, transport.calls, transport.headers)
	}
}

func messageWriteClient(transport *messageWriteTransport) *client.Client {
	return &client.Client{BaseURL: "https://teams.cloud.microsoft", Config: &config.Config{AuthSource: "web-session"}, HTTPClient: &http.Client{Transport: teamsOriginTransport{next: transport}}}
}

func TestTeamsMessagePreviewAndConfirmationNeverSend(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	t.Setenv("TEAMS_WEB_SESSION_DIR", t.TempDir())
	for _, tc := range []struct {
		args    []string
		wantErr bool
	}{
		{[]string{"messages", "send", "--conversation", "conversation-a", "--text", "hello", "--json", "--no-learn"}, false},
		{[]string{"messages", "send", "--conversation", "conversation-a", "--text", "hello", "--confirm", "--json", "--no-learn"}, false},
		{[]string{"messages", "send", "--conversation", "conversation-a", "--text", "hello", "--execute", "--json", "--no-learn"}, true},
		{[]string{"messages", "delete", "message-a", "--conversation", "conversation-a", "--json", "--no-learn"}, false},
		{[]string{"messages", "edit", "message-a", "--conversation", "conversation-a", "--text", "updated", "--json", "--no-learn"}, false},
	} {
		root := newRootCmd(&rootFlags{})
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(tc.args)
		err := root.Execute()
		if (err != nil) != tc.wantErr {
			t.Fatalf("args %v: error %v", tc.args, err)
		}
		if err == nil {
			var result map[string]any
			if json.Unmarshal(out.Bytes(), &result) != nil || result["executed"] != false {
				t.Fatalf("not a preview: %s", out.String())
			}
		}
	}
}

func TestTeamsMessageHistoryDistinguishesEmptyFromError(t *testing.T) {
	page, err := decodeTeamsMessagePage([]byte(`{"messages":[],"_metadata":{"syncState":"opaque"}}`))
	if err != nil || page.Messages == nil || len(page.Messages) != 0 || page.Metadata["syncState"] != "opaque" {
		t.Fatalf("empty history rejected: %#v %v", page, err)
	}
	for _, body := range []string{`{"error":{"code":"denied"}}`, `{"messages":null}`, `{}`, `{"messages":{}}`} {
		if _, err := decodeTeamsMessagePage([]byte(body)); err == nil {
			t.Fatalf("invalid history accepted: %s", body)
		}
	}
}

func TestTeamsMessageHistoryInitialRequestAndPopulatedPage(t *testing.T) {
	params, err := teamsHistoryParams("", 0, 200)
	if err != nil || params["view"] == "" || params["pageSize"] != "200" {
		t.Fatalf("invalid initial history parameters: %#v %v", params, err)
	}
	if _, ok := params["startTime"]; ok {
		t.Fatal("initial request included a start time")
	}
	if _, ok := params["syncState"]; ok {
		t.Fatal("initial request included a captured cursor")
	}
	page, err := decodeTeamsMessagePage([]byte(`{"messages":[{"id":"101","clientmessageid":"c1","content":"<p>hello</p>","properties":{}},{"id":"102","clientmessageid":"c2","content":"","properties":{"deletetime":"123"}}],"tenantId":"synthetic","_metadata":{"syncState":"returned-cursor"}}`))
	if err != nil || len(page.Messages) != 2 || page.Messages[0]["id"] != "101" || page.Metadata["syncState"] != "returned-cursor" {
		t.Fatalf("populated history not preserved: %#v %v", page, err)
	}
	props, ok := page.Messages[1]["properties"].(map[string]any)
	if !ok || props["deletetime"] != "123" {
		t.Fatal("tombstone not preserved")
	}
	next, err := teamsHistoryParams("returned-cursor", 101, 30)
	if err != nil || next["syncState"] != "returned-cursor" || next["startTime"] != "101" {
		t.Fatalf("explicit history paging lost: %#v %v", next, err)
	}
}

func TestTeamsSendReceiptAcceptsObservedNumericArrivalTime(t *testing.T) {
	if !validTeamsSendReceipt([]byte(`{"OriginalArrivalTime":1793953251237}`)) {
		t.Fatal("observed numeric send receipt was rejected")
	}
	for _, body := range []string{`{}`, `{"OriginalArrivalTime":null}`, `{"OriginalArrivalTime":{}}`, `{"error":"denied"}`} {
		if validTeamsSendReceipt([]byte(body)) {
			t.Fatalf("invalid send receipt accepted: %s", body)
		}
	}
}

func TestTeamsMessageEditPreservesIdentityAndRejectsStructuredContent(t *testing.T) {
	principal := teamsTokenPrincipal{Object: "synthetic-user", Name: "Synthetic User"}
	message := map[string]any{"id": "123", "conversationid": "conversation-a", "from": "https://teams.cloud.microsoft/api/chatsvc/amer/v1/users/ME/contacts/8:orgid:synthetic-user", "messagetype": "RichText/Html", "contenttype": "Text", "content": "<p>old</p>", "composetime": "2026-01-01T00:00:00.000Z", "originalarrivaltime": "2026-01-01T00:00:00.000Z", "version": "v1", "clientmessageid": "1234", "properties": map[string]any{"mentions": "[]", "serviceMetadata": "synthetic-value", "hasAttachments": false}}
	payload, err := teamsEditPayload(message, "conversation-a", "123", "new & improved", principal, time.UnixMilli(123))
	if err != nil || payload["from"] != "worker/8:orgid:synthetic-user" || payload["version"] != "v1" || payload["content"] != "<p>new &amp; improved</p>" {
		t.Fatalf("invalid edit payload: %#v, %v", payload, err)
	}
	if payload["properties"].(map[string]any)["serviceMetadata"] != "synthetic-value" {
		t.Fatal("service metadata was discarded")
	}
	message["properties"].(map[string]any)["mentions"] = `[{"id":"someone"}]`
	if _, err := teamsEditPayload(message, "conversation-a", "123", "new", principal, time.Now()); err == nil {
		t.Fatal("edit accepted a message containing mentions")
	}
	message["properties"].(map[string]any)["mentions"] = "[]"
	message["properties"].(map[string]any)["qtdMsgs"] = `[{"messageId":"previous"}]`
	if _, err := teamsEditPayload(message, "conversation-a", "123", "new", principal, time.Now()); err == nil {
		t.Fatal("edit accepted a quoted message")
	}
	delete(message["properties"].(map[string]any), "qtdMsgs")
	message["content"] = "<p>old <b>formatted</b></p>"
	if _, err := teamsEditPayload(message, "conversation-a", "123", "new", principal, time.Now()); err == nil {
		t.Fatal("edit accepted formatted content")
	}
}

func TestTeamsSendPayloadEscapesTextAndUsesObservedShape(t *testing.T) {
	payload, id, err := teamsSendPayload("conversation-a", "hello <team>\n& bye", teamsTokenPrincipal{Object: "synthetic-user", Name: "Synthetic User"}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if payload["content"] != "<p>hello &lt;team&gt;<br>&amp; bye</p>" || len(id) != 19 || payload["fromUserId"] != "8:orgid:synthetic-user" {
		t.Fatalf("unexpected message payload: %#v", payload)
	}
}

func TestTeamsMessageLocationRejectsOtherOriginsAndConversations(t *testing.T) {
	good := "https://amer.ng.msg.teams.microsoft.com/v1/users/ME/conversations/conversation-a/messages/123"
	if got := messageIDFromLocation(good, "conversation-a"); got != "123" {
		t.Fatalf("message ID = %q", got)
	}
	for _, location := range []string{strings.Replace(good, "amer.ng.msg.teams.microsoft.com", "evil.invalid", 1), strings.Replace(good, "conversation-a", "other", 1), good + "?token=secret"} {
		if messageIDFromLocation(location, "conversation-a") != "" {
			t.Fatal("untrusted location accepted")
		}
	}
}

func TestTeamsMessageWriteDoesNotRetryAmbiguousFailures(t *testing.T) {
	old := teamsMessageLimiter
	teamsMessageLimiter = nil
	defer func() { teamsMessageLimiter = old }()
	transport := &messageWriteTransport{err: errors.New("connection lost")}
	_, err := performTeamsMessageWrite(context.Background(), messageWriteClient(transport), http.MethodPost, "/api/chatsvc/amer/v1/users/me/conversations/test/messages", map[string]any{"content": "test"})
	if err == nil || !strings.Contains(err.Error(), "outcome_unknown") || transport.calls != 1 {
		t.Fatalf("err=%v calls=%d", err, transport.calls)
	}
}

func TestTeamsMessageWriteRateLimitIsTypedAndNotRetried(t *testing.T) {
	old := teamsMessageLimiter
	teamsMessageLimiter = nil
	defer func() { teamsMessageLimiter = old }()
	transport := &messageWriteTransport{response: &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"10"}}, Body: io.NopCloser(strings.NewReader(`{}`))}}
	_, err := performTeamsMessageWrite(context.Background(), messageWriteClient(transport), http.MethodPost, "/api/chatsvc/amer/v1/users/me/conversations/test/messages", nil)
	var limited *cliutil.RateLimitError
	if !errors.As(err, &limited) || limited.RetryAfter != 10*time.Second || transport.calls != 1 {
		t.Fatalf("err=%v calls=%d", err, transport.calls)
	}
}

func TestTeamsMessageWriteRejectsApplicationError(t *testing.T) {
	old := teamsMessageLimiter
	teamsMessageLimiter = nil
	defer func() { teamsMessageLimiter = old }()
	transport := &messageWriteTransport{response: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"denied"}}`))}}
	_, err := performTeamsMessageWrite(context.Background(), messageWriteClient(transport), http.MethodPost, "/api/chatsvc/amer/v1/users/me/conversations/test/messages", nil)
	if err == nil || transport.calls != 1 {
		t.Fatal("application error accepted")
	}
}

func TestTeamsMessageWriteDiagnosticIsPrivateAndExcludesRequestBody(t *testing.T) {
	old := teamsMessageLimiter
	teamsMessageLimiter = nil
	defer func() { teamsMessageLimiter = old }()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEAMS_WEB_SESSION_DIR", dir)
	name := filepath.Join(dir, "teams-write-diag-0123456789abcdef.json")
	t.Setenv("TEAMS_WEB_WRITE_DIAGNOSTIC", name)
	transport := &messageWriteTransport{response: &http.Response{StatusCode: 500, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":"server failure"}`))}}
	_, err := performTeamsMessageWrite(context.Background(), messageWriteClient(transport), http.MethodPost, "/api/chatsvc/amer/v1/users/me/conversations/test/messages", map[string]any{"content": "private-request-body"})
	if err == nil || !strings.Contains(err.Error(), "outcome_unknown") {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if json.Unmarshal(data, &result) != nil || result["http_status"] != float64(500) || !strings.Contains(result["response_body"].(string), "server failure") {
		t.Fatalf("unexpected diagnostic: %s", data)
	}
	if strings.Contains(string(data), "private-request-body") {
		t.Fatal("request body leaked into diagnostic")
	}
	if st, err := os.Stat(name); err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("diagnostic permissions are not private")
	}
}

func TestTeamsMessageWriteRejectsUnsafeDiagnosticBeforeSending(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TEAMS_WEB_SESSION_DIR", dir)
	t.Setenv("TEAMS_WEB_WRITE_DIAGNOSTIC", filepath.Join(t.TempDir(), "other.json"))
	transport := &messageWriteTransport{}
	_, err := performTeamsMessageWrite(context.Background(), messageWriteClient(transport), http.MethodPost, "/api/chatsvc/amer/v1/users/me/conversations/test/messages", nil)
	if err == nil || transport.calls != 0 {
		t.Fatal("unsafe diagnostic path did not block write before request")
	}
}
