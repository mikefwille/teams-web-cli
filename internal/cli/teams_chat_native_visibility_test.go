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
	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/config"
	"testing"
	"time"
)

type nativeVisibilityCall struct {
	name         string
	body         map[string]any
	method, auth string
}
type nativeVisibilityStep struct {
	status int
	body   string
	err    error
}
type nativeVisibilityTransport struct {
	steps []nativeVisibilityStep
	calls []nativeVisibilityCall
}

func (t *nativeVisibilityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	data, _ := io.ReadAll(req.Body)
	var body map[string]any
	_ = json.Unmarshal(data, &body)
	t.calls = append(t.calls, nativeVisibilityCall{name: req.URL.Query().Get("name"), body: body, method: req.Method, auth: req.Header.Get("Authorization")})
	i := len(t.calls) - 1
	if i >= len(t.steps) {
		return nil, errors.New("unexpected extra request")
	}
	step := t.steps[i]
	if step.err != nil {
		return nil, step.err
	}
	return &http.Response{StatusCode: step.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(step.body))}, nil
}
func nativeVisibilityClient(t *nativeVisibilityTransport) *client.Client {
	return &client.Client{BaseURL: "https://teams.cloud.microsoft", Config: &config.Config{AuthSource: "web-session"}, HTTPClient: &http.Client{Transport: teamsOriginTransport{next: t, scopes: map[string]map[string]string{"chatsvc": {"authorization": "Bearer synthetic"}}}}}
}
func TestTeamsNativeVisibilityMatchesObservedOrderedPair(t *testing.T) {
	old := teamsVisibilityLimiter
	teamsVisibilityLimiter = nil
	defer func() { teamsVisibilityLimiter = old }()
	data, err := os.ReadFile("testdata/teams_native_visibility_sanitized.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Hide, Unhide []struct {
			Name  string `json:"name"`
			Value any    `json:"value"`
		}
	}
	if json.Unmarshal(data, &fixture) != nil {
		t.Fatal("bad fixture")
	}
	for _, tc := range []struct {
		hide bool
		want []struct {
			Name  string `json:"name"`
			Value any    `json:"value"`
		}
	}{{true, fixture.Hide}, {false, fixture.Unhide}} {
		tr := &nativeVisibilityTransport{steps: []nativeVisibilityStep{{status: 200}, {status: 200}}}
		err = performTeamsNativeVisibilityWrite(context.Background(), nativeVisibilityClient(tr), tc.hide, time.UnixMilli(1700000000123))
		if err != nil || len(tr.calls) != 2 {
			t.Fatalf("pair failed: %v", err)
		}
		for i, call := range tr.calls {
			if call.name != tc.want[i].Name || call.method != http.MethodPut || call.auth != "Bearer synthetic" || !reflect.DeepEqual(call.body, map[string]any{tc.want[i].Name: tc.want[i].Value}) {
				t.Fatalf("captured pair mismatch at %d", i)
			}
		}
	}
}
func TestTeamsNativeVisibilityDoesNotRepeatPartialOrUnknownWrites(t *testing.T) {
	old := teamsVisibilityLimiter
	teamsVisibilityLimiter = nil
	defer func() { teamsVisibilityLimiter = old }()
	for _, tc := range []struct {
		steps            []nativeVisibilityStep
		calls            int
		partial, unknown bool
	}{
		{[]nativeVisibilityStep{{err: errors.New("private")}}, 1, false, true},
		{[]nativeVisibilityStep{{status: 200}, {err: errors.New("private")}}, 2, true, true},
		{[]nativeVisibilityStep{{status: 200}, {status: 500}}, 2, true, true},
		{[]nativeVisibilityStep{{status: 200}, {status: 400}}, 2, true, false},
	} {
		tr := &nativeVisibilityTransport{steps: tc.steps}
		err := performTeamsNativeVisibilityWrite(context.Background(), nativeVisibilityClient(tr), true, time.UnixMilli(1700000000123))
		if err == nil || len(tr.calls) != tc.calls || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "partial_write") != tc.partial || strings.Contains(err.Error(), "outcome_unknown") != tc.unknown {
			t.Fatalf("unsafe partial handling: calls=%d err=%v", len(tr.calls), err)
		}
	}
	tr := &nativeVisibilityTransport{steps: []nativeVisibilityStep{{status: 200}}}
	if err := performTeamsVisibilityProperty(context.Background(), nativeVisibilityClient(tr), "unobserved", nil); err == nil || len(tr.calls) != 0 {
		t.Fatal("unobserved property accepted")
	}
}
func TestTeamsNativeVisibilityPreviewAndSelfOnly(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	t.Setenv("TEAMS_WEB_SESSION_DIR", t.TempDir())
	for _, tc := range []struct {
		args      []string
		wantError bool
	}{
		{[]string{"chats", "hide", "--chat", "48:notes", "--json", "--no-learn"}, false},
		{[]string{"chats", "unhide", "--chat", "48:notes", "--json", "--no-learn"}, false},
		{[]string{"chats", "hide", "--chat", "19:other@thread.v2", "--json", "--no-learn"}, true},
		{[]string{"chats", "hide", "--chat", "48:notes", "--execute", "--json", "--no-learn"}, true},
	} {
		root := newRootCmd(&rootFlags{})
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(tc.args)
		err := root.Execute()
		if (err != nil) != tc.wantError {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if err == nil {
			var v map[string]any
			if json.Unmarshal(out.Bytes(), &v) != nil || v["executed"] != false || v["explicit_read_state_write"] != false || !strings.Contains(v["effect"].(string), "rendered section require") {
				t.Fatalf("unsafe preview: %s", out.String())
			}
			props := v["property_writes"].([]any)
			if len(props) != 2 || props[0] != "unpinnedTime" || props[1] != "historyHiddenTime" {
				t.Fatal("preview hides ordered effects")
			}
		}
	}
}
