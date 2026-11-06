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
	"time"
)

func TestTeamsStatusMessagePayloadUsesObservedOneHourShape(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 15, 0, 0, time.UTC)
	body, expiry, err := teamsStatusMessagePayload("Ready <now> & later", now)
	if err != nil || body["message"] != "<p>Ready &lt;now&gt; &amp; later</p>" || body["expiry"] != "2026-11-06T11:15:00.000Z" || !expiry.Equal(now.Add(time.Hour)) {
		t.Fatalf("unexpected status payload: %#v %v", body, err)
	}
	for _, text := range []string{"", "  ", "two\nlines", strings.Repeat("a", 1025)} {
		if _, _, err := teamsStatusMessagePayload(text, now); err == nil {
			t.Fatalf("accepted invalid status text %q", text)
		}
	}
}

func TestTeamsPresenceUsesOnlyObservedAudienceRoutes(t *testing.T) {
	for _, path := range []string{teamsPresencePath, teamsForceAvailabilityPath, teamsPublishNotePath} {
		if teamsServiceFamily(path) != "presence" {
			t.Fatalf("presence route not mapped: %s", path)
		}
	}
	if teamsServiceFamily("/ups/noam/v1/me/unobserved") == "presence" {
		t.Fatal("unobserved route inherited presence credentials")
	}
}

func TestTeamsPresenceWriteSendsOneGuardedRequest(t *testing.T) {
	old := teamsPresenceWriteLimiter
	teamsPresenceWriteLimiter = nil
	defer func() { teamsPresenceWriteLimiter = old }()
	for _, tc := range []struct {
		path string
		body any
		want string
	}{
		{teamsForceAvailabilityPath, map[string]string{"availability": "Busy"}, `{"availability":"Busy"}`},
		{teamsForceAvailabilityPath, nil, ""},
		{teamsPublishNotePath, map[string]string{"message": ""}, `{"message":""}`},
	} {
		base := &messageWriteTransport{response: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}}
		c := messageWriteClient(base)
		c.HTTPClient.Transport = teamsOriginTransport{next: base, scopes: map[string]map[string]string{"presence": {"authorization": "Bearer synthetic", "content-type": "text/plain"}}}
		if err := performTeamsPresenceWrite(context.Background(), c, tc.path, tc.body); err != nil {
			t.Fatal(err)
		}
		if base.calls != 1 || base.method != http.MethodPut || string(base.body) != tc.want || base.headers.Get("Authorization") != "Bearer synthetic" || base.headers.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected presence request: %#v", base)
		}
	}
	base := &messageWriteTransport{err: errors.New("synthetic transport failure")}
	c := messageWriteClient(base)
	if err := performTeamsPresenceWrite(context.Background(), c, teamsForceAvailabilityPath, nil); err == nil || !strings.Contains(err.Error(), "outcome_unknown") || base.calls != 1 {
		t.Fatalf("ambiguous write was retried or hidden: %v calls=%d", err, base.calls)
	}
	if err := performTeamsPresenceWrite(context.Background(), c, "/ups/noam/v1/me/unobserved", nil); err == nil || base.calls != 1 {
		t.Fatal("unobserved write path accepted")
	}
}

func TestTeamsPresencePreviewAndConfirmation(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	t.Setenv("TEAMS_WEB_SESSION_DIR", t.TempDir())
	for _, tc := range []struct {
		args    []string
		wantErr bool
	}{
		{[]string{"presence", "set", "--availability", "Busy", "--json", "--no-learn"}, false},
		{[]string{"presence", "reset", "--json", "--no-learn"}, false},
		{[]string{"presence", "message", "set", "--text", "Synthetic status", "--json", "--no-learn"}, false},
		{[]string{"presence", "message", "clear", "--json", "--no-learn"}, false},
		{[]string{"presence", "set", "--availability", "Away", "--json", "--no-learn"}, true},
		{[]string{"presence", "set", "--availability", "Busy", "--execute", "--json", "--no-learn"}, true},
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
			var value map[string]any
			if json.Unmarshal(out.Bytes(), &value) != nil || value["executed"] != false {
				t.Fatalf("not a preview: %s", out.String())
			}
		}
	}
}
