package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/config"
)

const syntheticOwnedTeam = "19:synthetic-team@thread.tacv2"

func TestTeamsLifecycleMatchesSanitizedCapturedEvidence(t *testing.T) {
	data, err := os.ReadFile("testdata/teams_owned_sanitized.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Request  teamsPrivateCreateBody `json:"request"`
		Response teamsCreatedTeam       `json:"response"`
		Thread   json.RawMessage        `json:"thread"`
		Roster   json.RawMessage        `json:"roster"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.Request, teamsPrivateCreatePayload("Synthetic Team", "Initial")) {
		t.Fatalf("create payload differs from captured shape: %#v", fixture.Request)
	}
	if fixture.Response.Value.ObjectID != syntheticOwnedTeam || fixture.Response.Value.SkypeThreadID != syntheticOwnedTeam || fixture.Response.Value.DisplayName != "Synthetic Team" || !strings.EqualFold(fixture.Response.Value.AccessType, "private") {
		t.Fatal("captured create response shape does not match")
	}
	if err := validateTeamsOwnedTeam(fixture.Thread, fixture.Roster, syntheticOwnedTeam, "Synthetic Team", "synthetic-user"); err != nil {
		t.Fatalf("captured owner and roster shape was rejected: %v", err)
	}
}

type lifecycleTestTransport struct {
	calls  int
	status int
	body   string
	err    error
	method string
	path   string
	sent   []byte
}

func (t *lifecycleTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls++
	t.method, t.path = req.Method, req.URL.EscapedPath()
	if req.Body != nil {
		t.sent, _ = io.ReadAll(req.Body)
	}
	if t.err != nil {
		return nil, t.err
	}
	return &http.Response{StatusCode: t.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(t.body))}, nil
}

func lifecycleTestClient(t *lifecycleTestTransport) *client.Client {
	return &client.Client{BaseURL: "https://teams.cloud.microsoft", Config: &config.Config{AuthSource: "web-session"}, HTTPClient: &http.Client{Transport: teamsOriginTransport{next: t, scopes: map[string]map[string]string{"mt": {"authorization": "Bearer synthetic"}}}}}
}

func TestTeamsPrivateTeamCreateAndDeleteWireShape(t *testing.T) {
	old := teamsLifecycleLimiter
	teamsLifecycleLimiter = nil
	defer func() { teamsLifecycleLimiter = old }()
	response := `{"value":{"objectId":"` + syntheticOwnedTeam + `","skypeThreadId":"` + syntheticOwnedTeam + `","displayName":"Synthetic Team","accessType":"Private"}}`
	transport := &lifecycleTestTransport{status: 201, body: response}
	c := lifecycleTestClient(transport)
	id, err := performTeamsLifecycleWrite(context.Background(), c, "create", "", "Synthetic Team", "Initial")
	if err != nil || id != syntheticOwnedTeam || transport.calls != 1 || transport.method != http.MethodPost || transport.path != teamsLifecycleBase+"/create" {
		t.Fatalf("unexpected create result: id=%q err=%v calls=%d path=%q", id, err, transport.calls, transport.path)
	}
	var sent teamsPrivateCreateBody
	if json.Unmarshal(transport.sent, &sent) != nil || sent.DisplayName != "Synthetic Team" || sent.TopicThreadTopic != "Initial" || sent.AccessType != 1 || sent.IsTenantWide || sent.ValidationRequired || sent.SensitivityLabelID != nil || sent.Description != "" || sent.DisplayNamePrefix != "" || sent.DisplayNameSuffix != "" {
		t.Fatalf("unexpected captured request shape: %s", transport.sent)
	}
	transport = &lifecycleTestTransport{status: 200, body: `true`}
	c = lifecycleTestClient(transport)
	id, err = performTeamsLifecycleWrite(context.Background(), c, "delete", syntheticOwnedTeam, "", "")
	if err != nil || id != syntheticOwnedTeam || transport.calls != 1 || transport.method != http.MethodDelete || !strings.HasSuffix(transport.path, "/delete") {
		t.Fatalf("unexpected delete result: id=%q err=%v calls=%d", id, err, transport.calls)
	}
}

func TestTeamsLifecycleAmbiguousWriteNeverRetries(t *testing.T) {
	old := teamsLifecycleLimiter
	teamsLifecycleLimiter = nil
	defer func() { teamsLifecycleLimiter = old }()
	for _, tc := range []lifecycleTestTransport{
		{err: errors.New("private transport failure")},
		{status: 500, body: "private server body"},
		{status: 302, body: ""},
		{status: 201, body: `{}`},
		{status: 200, body: `{}`},
	} {
		transport := tc
		_, err := performTeamsLifecycleWrite(context.Background(), lifecycleTestClient(&transport), "create", "", "Synthetic Team", "Initial")
		if err == nil || !strings.Contains(err.Error(), "outcome_unknown") || transport.calls != 1 || strings.Contains(err.Error(), "private transport") || strings.Contains(err.Error(), "private server") {
			t.Fatalf("ambiguous result was retried or leaked: %v calls=%d", err, transport.calls)
		}
	}
	transport := &lifecycleTestTransport{status: 200, body: `false`}
	_, err := performTeamsLifecycleWrite(context.Background(), lifecycleTestClient(transport), "delete", syntheticOwnedTeam, "", "")
	if err == nil || !strings.Contains(err.Error(), "outcome_unknown") || transport.calls != 1 {
		t.Fatalf("unexpected delete result: %v", err)
	}
}

func TestTeamsDeleteRequiresMatchingOwnerAndCompleteSingleHumanRoster(t *testing.T) {
	thread := `{"id":"` + syntheticOwnedTeam + `","threadProperties":{"isCreator":true,"spaceThreadTopic":"Synthetic Team","threadType":"space"},"memberProperties":{"role":"Admin"}}`
	roster := `{"isDeleted":false,"totalMemberCount":2,"members":[{"id":"8:orgid:synthetic-user","role":"Admin"},{"id":"28:synthetic-bot","role":"User"}]}`
	if err := validateTeamsOwnedTeam([]byte(thread), []byte(roster), syntheticOwnedTeam, "Synthetic Team", "synthetic-user"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, thread, roster, team, expected, own string }{
		{"wrong name", thread, roster, syntheticOwnedTeam, "Other", "synthetic-user"},
		{"wrong id", thread, roster, "19:other@thread.tacv2", "Synthetic Team", "synthetic-user"},
		{"not creator", strings.Replace(thread, `"isCreator":true`, `"isCreator":false`, 1), roster, syntheticOwnedTeam, "Synthetic Team", "synthetic-user"},
		{"not owner", thread, strings.Replace(roster, `"role":"Admin"`, `"role":"User"`, 1), syntheticOwnedTeam, "Synthetic Team", "synthetic-user"},
		{"other human", thread, strings.Replace(roster, `28:synthetic-bot`, `8:orgid:another`, 1), syntheticOwnedTeam, "Synthetic Team", "synthetic-user"},
		{"unknown member", thread, strings.Replace(roster, `28:synthetic-bot`, `unknown`, 1), syntheticOwnedTeam, "Synthetic Team", "synthetic-user"},
		{"partial count", thread, strings.Replace(roster, `"totalMemberCount":2`, `"totalMemberCount":3`, 1), syntheticOwnedTeam, "Synthetic Team", "synthetic-user"},
		{"continuation", thread, strings.Replace(roster, `"isDeleted":false`, `"continuationToken":"more","isDeleted":false`, 1), syntheticOwnedTeam, "Synthetic Team", "synthetic-user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateTeamsOwnedTeam([]byte(tc.thread), []byte(tc.roster), tc.team, tc.expected, tc.own); err == nil {
				t.Fatal("unsafe deletion accepted")
			}
		})
	}
}

func TestTeamsLifecyclePreviewAndConfirmation(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	t.Setenv("TEAMS_WEB_SESSION_DIR", t.TempDir())
	for _, tc := range []struct {
		args    []string
		wantErr bool
	}{
		{[]string{"teams", "create", "--name", "Synthetic Team", "--initial-channel", "Initial", "--json", "--no-learn"}, false},
		{[]string{"teams", "delete", syntheticOwnedTeam, "--if-name", "Synthetic Team", "--json", "--no-learn"}, false},
		{[]string{"teams", "create", "--name", "Synthetic Team", "--initial-channel", "Initial", "--execute", "--json", "--no-learn"}, true},
		{[]string{"teams", "delete", syntheticOwnedTeam, "--if-name", "Synthetic Team", "--execute", "--json", "--no-learn"}, true},
		{[]string{"teams", "delete", "../bad", "--if-name", "Synthetic Team", "--json", "--no-learn"}, true},
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
