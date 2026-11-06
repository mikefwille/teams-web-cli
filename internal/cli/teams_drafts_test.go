package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTeamsLocalDraftLifecycleAndAccountIsolation(t *testing.T) {
	home := t.TempDir()
	sessionDir := t.TempDir()
	t.Setenv("TEAMS_WEB_HOME", home)
	t.Setenv("TEAMS_WEB_SESSION_DIR", sessionDir)
	writeTeamsFixture(t, sessionDir, time.Now().Add(time.Hour))
	run := func(args ...string) map[string]any {
		t.Helper()
		root := newRootCmd(&rootFlags{})
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append(args, "--json", "--no-learn"))
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, out.String())
		}
		var value map[string]any
		if err := json.Unmarshal(out.Bytes(), &value); err != nil {
			t.Fatalf("invalid JSON: %s", out.String())
		}
		return value
	}
	preview := run("drafts", "create", "--conversation", "conversation-a", "--text", "original")
	if preview["executed"] != false || preview["sends_message"] != false {
		t.Fatal("draft creation preview was not safe")
	}
	dir, err := teamsDraftDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("preview created draft storage")
	}
	created := run("drafts", "create", "--conversation", "conversation-a", "--text", "original", "--execute", "--confirm")
	draft := created["result"].(map[string]any)
	id := draft["id"].(string)
	if created["sends_message"] != false || draft["storage"] != "local" {
		t.Fatal("draft was not labeled local")
	}
	file := filepath.Join(dir, id+".json")
	if s, err := os.Stat(file); err != nil || s.Mode().Perm() != 0600 {
		t.Fatal("draft file is not private")
	}
	if got := run("drafts", "list"); got["count"] != float64(1) {
		t.Fatalf("list: %#v", got)
	}
	updatePreview := run("drafts", "update", id, "--text", "changed")
	if updatePreview["current"].(map[string]any)["text"] != "original" {
		t.Fatal("update preview lacked current content")
	}
	if got := run("drafts", "get", id); got["result"].(map[string]any)["text"] != "original" {
		t.Fatal("preview changed draft")
	}
	run("drafts", "update", id, "--text", "changed", "--execute", "--confirm")
	if got := run("drafts", "get", id); got["result"].(map[string]any)["text"] != "changed" {
		t.Fatal("draft update did not persist")
	}
	fileSession := filepath.Join(sessionDir, "session-teams-abc-1234abcd.json")
	data, err := os.ReadFile(fileSession)
	if err != nil {
		t.Fatal(err)
	}
	var session teamsSessionExport
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	originalAuth := session.Headers["authorization"]
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "tid": "tenant-test", "oid": "different-account"})
	session.Headers["authorization"] = "Bearer aa." + base64.RawURLEncoding.EncodeToString(claims) + ".bb"
	data, _ = json.Marshal(session)
	if err := os.WriteFile(fileSession, data, 0600); err != nil {
		t.Fatal(err)
	}
	if got := run("drafts", "list"); got["count"] != float64(0) {
		t.Fatal("drafts leaked across accounts")
	}
	session.Headers["authorization"] = originalAuth
	data, _ = json.Marshal(session)
	if err := os.WriteFile(fileSession, data, 0600); err != nil {
		t.Fatal(err)
	}
	run("drafts", "delete", id, "--execute", "--confirm")
	if got := run("drafts", "list"); got["count"] != float64(0) {
		t.Fatal("draft deletion failed")
	}
}

func TestTeamsDraftOperationsHonorLocalLogout(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	dir := t.TempDir()
	t.Setenv("TEAMS_WEB_SESSION_DIR", dir)
	writeTeamsFixture(t, dir, time.Now().Add(time.Hour))
	run := func(args ...string) (map[string]any, error) {
		t.Helper()
		root := newRootCmd(&rootFlags{})
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append(args, "--json", "--no-learn"))
		err := root.Execute()
		var value map[string]any
		_ = json.Unmarshal(out.Bytes(), &value)
		return value, err
	}
	created, err := run("drafts", "create", "--conversation", "synthetic-chat", "--text", "synthetic draft", "--execute", "--confirm")
	if err != nil {
		t.Fatal(err)
	}
	id := created["result"].(map[string]any)["id"].(string)
	draftDir, err := teamsDraftDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run("auth", "logout", "--execute"); err != nil {
		t.Fatal(err)
	}
	preview, err := run("drafts", "create", "--conversation", "synthetic-chat", "--text", "preview only")
	if err != nil || preview["executed"] != false || preview["sends_message"] != false {
		t.Fatal("a local creation preview should not need access to stored drafts")
	}
	for _, args := range [][]string{
		{"drafts", "list"},
		{"drafts", "get", id},
		{"drafts", "update", id, "--text", "preview"},
		{"drafts", "delete", id},
		{"drafts", "update", id, "--text", "changed", "--execute", "--confirm"},
		{"drafts", "delete", id, "--execute", "--confirm"},
		{"drafts", "create", "--conversation", "synthetic-chat", "--text", "second", "--execute", "--confirm"},
	} {
		if _, err := run(args...); err == nil || !strings.Contains(err.Error(), "locally disabled") {
			t.Fatalf("draft operation after logout was not refused: %v, %v", args, err)
		}
	}
	rows, err := listTeamsDrafts(draftDir)
	if err != nil || len(rows) != 1 || rows[0].ID != id || rows[0].Text != "synthetic draft" {
		t.Fatal("refused operations changed the local draft")
	}
	if err := setTeamsSessionDisabled(false); err != nil {
		t.Fatal(err)
	}
	if result, err := run("drafts", "list"); err != nil || result["count"] != float64(1) {
		t.Fatal("draft was not available after local reactivation")
	}
}
