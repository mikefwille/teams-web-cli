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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/config"
)

type visibilityTransport struct {
	calls               int
	status              int
	body                string
	err                 error
	method, path, query string
	sent                []byte
	auth                string
}

func (t *visibilityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls++
	t.method = req.Method
	t.path = req.URL.Path
	t.query = req.URL.RawQuery
	t.auth = req.Header.Get("Authorization")
	if req.Body != nil {
		t.sent, _ = io.ReadAll(req.Body)
	}
	if t.err != nil {
		return nil, t.err
	}
	return &http.Response{StatusCode: t.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(t.body))}, nil
}
func visibilityClient(t *visibilityTransport) *client.Client {
	return &client.Client{BaseURL: "https://teams.cloud.microsoft", Config: &config.Config{AuthSource: "web-session"}, HTTPClient: &http.Client{Transport: teamsOriginTransport{next: t, scopes: map[string]map[string]string{"chatsvc": {"authorization": "Bearer synthetic"}}}}}
}
func TestTeamsVisibilityMatchesSanitizedCapture(t *testing.T) {
	data, err := os.ReadFile("testdata/teams_visibility_sanitized.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct{ Hide, Unhide map[string]any }
	if json.Unmarshal(data, &f) != nil {
		t.Fatal("invalid fixture")
	}
	h, err := teamsVisibilityPayload(true, time.UnixMilli(1700000000123))
	if err != nil || !reflect.DeepEqual(h, f.Hide) {
		t.Fatal("hide payload differs from captured shape")
	}
	u, err := teamsVisibilityPayload(false, time.Time{})
	if err != nil || !reflect.DeepEqual(u, f.Unhide) {
		t.Fatal("unhide payload differs from captured shape")
	}
	if _, err = teamsVisibilityPayload(true, time.Time{}); err == nil {
		t.Fatal("invalid hide timestamp accepted")
	}
}
func TestTeamsVisibilitySendsOneScopedWriteAndRejectsAmbiguity(t *testing.T) {
	old := teamsVisibilityLimiter
	teamsVisibilityLimiter = nil
	defer func() { teamsVisibilityLimiter = old }()
	for _, hide := range []bool{true, false} {
		tr := &visibilityTransport{status: 200}
		err := performTeamsVisibilityWrite(context.Background(), visibilityClient(tr), hide, time.UnixMilli(1700000000123))
		if err != nil || tr.calls != 1 || tr.method != http.MethodPut || tr.path != "/api/chatsvc/amer/v1/users/ME/conversations/48:notes/properties" || tr.query != "name=historyHiddenTime" || tr.auth != "Bearer synthetic" {
			t.Fatalf("wrong captured request: %v", err)
		}
		var got map[string]any
		if json.Unmarshal(tr.sent, &got) != nil {
			t.Fatal("bad body")
		}
		want, _ := teamsVisibilityPayload(hide, time.UnixMilli(1700000000123))
		if !reflect.DeepEqual(got, want) {
			t.Fatal("wrong property body")
		}
	}
	for _, tr := range []*visibilityTransport{{err: errors.New("private transport")}, {status: 500, body: "private response"}, {status: 302}, {status: 201}, {status: 200, body: `true`}} {
		err := performTeamsVisibilityWrite(context.Background(), visibilityClient(tr), true, time.UnixMilli(1700000000123))
		if err == nil || !strings.Contains(err.Error(), "outcome_unknown") || tr.calls != 1 || strings.Contains(err.Error(), "private") {
			t.Fatal("ambiguous visibility write retried or leaked")
		}
	}
}
func TestTeamsVisibilityPreviewConfirmationAndSelfOnly(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	t.Setenv("TEAMS_WEB_SESSION_DIR", t.TempDir())
	for _, tc := range []struct {
		args      []string
		wantError bool
	}{
		{[]string{"chats", "hide-history", "--chat", "48:notes", "--json", "--no-learn"}, false},
		{[]string{"chats", "show-history", "--chat", "48:notes", "--json", "--no-learn"}, false},
		{[]string{"chats", "hide-history", "--chat", "19:other@thread.v2", "--json", "--no-learn"}, true},
		{[]string{"chats", "hide-history", "--chat", "48:notes", "--execute", "--json", "--no-learn"}, true},
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
			if json.Unmarshal(out.Bytes(), &v) != nil || v["executed"] != false || v["explicit_read_state_write"] != false || !strings.Contains(v["effect"].(string), "rendered section is not determined") {
				t.Fatalf("unsafe preview: %s", out.String())
			}
		}
	}
}
func TestTeamsVisibilityLockIsPrivateAndAccountScoped(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	a, err := teamsVisibilityLockPath(teamsTokenPrincipal{Tenant: "tenant", Object: "one"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := teamsVisibilityLockPath(teamsTokenPrincipal{Tenant: "tenant", Object: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("lock not scoped to account")
	}
	dir := filepath.Dir(a)
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		t.Fatal("lock directory is not private")
	}
	if err = os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), a+".lock"); err != nil {
		t.Fatal(err)
	}
	if _, err = teamsVisibilityLockPath(teamsTokenPrincipal{Tenant: "tenant", Object: "one"}); err == nil {
		t.Fatal("symlink lock accepted")
	}
}
