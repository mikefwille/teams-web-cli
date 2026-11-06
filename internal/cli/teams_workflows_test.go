package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCollectTeamsChannelsDeduplicatesNestedChannels(t *testing.T) {
	body := teamsBootstrap{
		Channels: []map[string]any{{"id": "channel-a", "parentTeamId": "team-a"}},
		Teams:    []map[string]any{{"id": "team-a", "channels": []any{map[string]any{"id": "channel-a"}, map[string]any{"id": "channel-b"}}}},
	}
	got := collectTeamsChannels(body)
	if len(got) != 2 || teamsObjectID(got[0]) != "channel-a" || teamsObjectID(got[1]) != "channel-b" {
		t.Fatalf("unexpected channels: %#v", got)
	}
}

func TestTeamsAudienceImportDoesNotUndoLocalLogout(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	helper := filepath.Join(t.TempDir(), "m365-session")
	script := []byte("#!/bin/sh\nprintf '%s\\n' '{\"m365_session\":1,\"service\":\"teams\",\"ok\":true}'\n")
	if err := os.WriteFile(helper, script, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEAMS_WEB_SESSION_HELPER", helper)
	if err := setTeamsSessionDisabled(true); err != nil {
		t.Fatal(err)
	}
	root := newRootCmd(&rootFlags{})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"auth", "import-audience", "abc-1234abcd", "123", "--execute", "--json", "--no-learn"})
	if err := root.Execute(); err != nil {
		t.Fatalf("audience import: %v %s", err, out.String())
	}
	if disabled, err := teamsSessionDisabled(); err != nil || !disabled {
		t.Fatal("importing an audience unexpectedly reenabled a logged-out session")
	}
}

func TestTeamsRecoverInvokesHelperAndRejectsShortDeadline(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	dir := t.TempDir()
	helper, receipt := filepath.Join(dir, "m365-session"), filepath.Join(dir, "args")
	script := []byte("#!/bin/sh\nprintf '%s %s' \"$1\" \"$2\" > \"$TEAMS_TEST_RECOVERY_ARGS\"\nprintf '%s\\n' '{\"m365_session\":1,\"service\":\"teams\",\"ok\":true}'\n")
	if err := os.WriteFile(helper, script, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEAMS_WEB_SESSION_HELPER", helper)
	t.Setenv("TEAMS_TEST_RECOVERY_ARGS", receipt)
	run := func(args ...string) error {
		t.Helper()
		root := newRootCmd(&rootFlags{})
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		return root.Execute()
	}
	if err := run("auth", "recover", "--execute", "--json", "--no-learn"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(receipt)
	if err != nil || string(got) != "recover teams" {
		t.Fatalf("unexpected recovery helper invocation: %q %v", got, err)
	}
	if err := run("auth", "recover", "--execute", "--timeout", "5s", "--json", "--no-learn"); err == nil {
		t.Fatal("short deadline could interrupt browser cleanup")
	}
	if err := setTeamsSessionDisabled(true); err != nil {
		t.Fatal(err)
	}
	if err := run("auth", "recover-files", "--execute", "--json", "--no-learn"); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(receipt)
	if err != nil || string(got) != "recover-files teams" {
		t.Fatalf("unexpected file recovery helper invocation: %q %v", got, err)
	}
	if disabled, err := teamsSessionDisabled(); err != nil || !disabled {
		t.Fatal("recovering an audience unexpectedly reenabled a logged-out session")
	}
}

func TestTeamsReadDryRunDoesNotNeedSession(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	t.Setenv("TEAMS_WEB_SESSION_DIR", t.TempDir())
	flags := &rootFlags{}
	root := newRootCmd(flags)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"teams", "list", "--dry-run", "--json", "--no-learn"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if json.Unmarshal(output.Bytes(), &out) != nil || out["dry_run"] != true {
		t.Fatalf("unexpected dry run: %s", output.String())
	}
}

func TestTeamsLocalLogoutCanBeReenabled(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	if err := setTeamsSessionDisabled(true); err != nil {
		t.Fatal(err)
	}
	if disabled, err := teamsSessionDisabled(); err != nil || !disabled {
		t.Fatal("session was not disabled")
	}
	if err := setTeamsSessionDisabled(false); err != nil {
		t.Fatal(err)
	}
	if disabled, err := teamsSessionDisabled(); err != nil || disabled {
		t.Fatal("session was not reenabled")
	}
}
