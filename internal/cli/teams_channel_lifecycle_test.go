package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const syntheticChannel = "19:synthetic-channel@thread.tacv2"
const syntheticGroup = "11111111-2222-4333-8444-555555555555"

func channelFixture(t *testing.T) struct {
	Request      teamsChannelCreateBody `json:"request"`
	Response     json.RawMessage        `json:"response"`
	Conversation json.RawMessage        `json:"conversation"`
	Delete       teamsChannelDeleteBody `json:"delete_observed_subset"`
} {
	t.Helper()
	data, err := os.ReadFile("testdata/teams_channel_sanitized.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Request      teamsChannelCreateBody `json:"request"`
		Response     json.RawMessage        `json:"response"`
		Conversation json.RawMessage        `json:"conversation"`
		Delete       teamsChannelDeleteBody `json:"delete_observed_subset"`
	}
	if json.Unmarshal(data, &f) != nil {
		t.Fatal("bad fixture")
	}
	return f
}
func TestTeamsChannelObservedShapeAndParentValidation(t *testing.T) {
	f := channelFixture(t)
	if !reflect.DeepEqual(f.Request, teamsChannelCreatePayload("Synthetic Channel", syntheticGroup)) {
		t.Fatal("create request differs from capture")
	}
	if !reflect.DeepEqual(f.Delete, teamsChannelDeletePayload(syntheticChannel, "Synthetic Channel")) {
		t.Fatal("delete subset differs from capture")
	}
	if group, err := validateTeamsChannelThread(f.Conversation, syntheticOwnedTeam, syntheticChannel, "Synthetic Channel", syntheticGroup); err != nil || group != syntheticGroup {
		t.Fatalf("captured channel rejected: %v", err)
	}
	for _, bad := range []string{strings.Replace(string(f.Conversation), syntheticOwnedTeam, "19:other@thread.tacv2", 1), strings.Replace(string(f.Conversation), "TeamsStandardChannel", "TeamsPrivateChannel", 1), strings.Replace(string(f.Conversation), `"privacy": "public"`, `"privacy": "private"`, 1), strings.Replace(string(f.Conversation), syntheticGroup, "11111111-2222-4333-8444-555555555556", 1)} {
		if _, err := validateTeamsChannelThread([]byte(bad), syntheticOwnedTeam, syntheticChannel, "Synthetic Channel", syntheticGroup); err == nil {
			t.Fatal("unsafe channel accepted")
		}
	}
	if teamsChannelName("General") || teamsChannelName(" general ") || teamsChannelName("") {
		t.Fatal("unsafe channel name accepted")
	}
}
func TestTeamsChannelWriteUsesOneObservedRouteAndNoRetry(t *testing.T) {
	old := teamsLifecycleLimiter
	teamsLifecycleLimiter = nil
	defer func() { teamsLifecycleLimiter = old }()
	f := channelFixture(t)
	tr := &lifecycleTestTransport{status: 201, body: string(f.Response)}
	id, err := performTeamsChannelWrite(context.Background(), lifecycleTestClient(tr), "create", syntheticOwnedTeam, "", "Synthetic Channel", syntheticGroup)
	if err != nil || id != syntheticChannel || tr.calls != 1 || tr.method != http.MethodPost || !strings.HasSuffix(tr.path, "/channels") {
		t.Fatalf("create: %v calls %d", err, tr.calls)
	}
	var sent teamsChannelCreateBody
	if json.Unmarshal(tr.sent, &sent) != nil || !reflect.DeepEqual(sent, f.Request) {
		t.Fatal("create body mismatch")
	}
	tr = &lifecycleTestTransport{status: 200, body: `true`}
	id, err = performTeamsChannelWrite(context.Background(), lifecycleTestClient(tr), "delete", syntheticOwnedTeam, syntheticChannel, "Synthetic Channel", syntheticGroup)
	if err != nil || id != syntheticChannel || tr.calls != 1 || tr.method != http.MethodDelete || !strings.Contains(tr.path, "/channels/") {
		t.Fatalf("delete: %v", err)
	}
	var sub teamsChannelDeleteBody
	if json.Unmarshal(tr.sent, &sub) != nil || !reflect.DeepEqual(sub, f.Delete) {
		t.Fatal("delete subset mismatch")
	}
	for _, tr = range []*lifecycleTestTransport{{err: errors.New("private")}, {status: 500, body: "private"}, {status: 201, body: `{}`}} {
		_, err = performTeamsChannelWrite(context.Background(), lifecycleTestClient(tr), "create", syntheticOwnedTeam, "", "Synthetic Channel", syntheticGroup)
		if err == nil || !strings.Contains(err.Error(), "outcome_unknown") || tr.calls != 1 || strings.Contains(err.Error(), "private") {
			t.Fatal("ambiguous write was retried or leaked")
		}
	}
	tr = &lifecycleTestTransport{status: 200, body: `false`}
	_, err = performTeamsChannelWrite(context.Background(), lifecycleTestClient(tr), "delete", syntheticOwnedTeam, syntheticChannel, "Synthetic Channel", syntheticGroup)
	if err == nil || !strings.Contains(err.Error(), "outcome_unknown") || tr.calls != 1 {
		t.Fatal("unrecognized delete accepted")
	}
}
func TestTeamsChannelReceiptsAreAccountScopedAndExact(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	one := teamsTokenPrincipal{Tenant: "tenant", Object: "one"}
	two := teamsTokenPrincipal{Tenant: "tenant", Object: "two"}
	dir, err := teamsChannelReceiptDir(one, true)
	if err != nil {
		t.Fatal(err)
	}
	other, err := teamsChannelReceiptDir(two, true)
	if err != nil {
		t.Fatal(err)
	}
	if dir == other {
		t.Fatal("accounts shared receipt directory")
	}
	rec := teamsChannelReceipt{Version: 1, State: "active", TeamID: syntheticOwnedTeam, ChannelID: syntheticChannel, Name: "Synthetic Channel", GroupID: syntheticGroup}
	if err = writeTeamsChannelReceipt(dir, rec); err != nil {
		t.Fatal(err)
	}
	if err = writeTeamsChannelReceipt(dir, rec); err == nil {
		t.Fatal("receipt overwritten")
	}
	if got, err := readTeamsChannelReceipt(dir, rec.TeamID, rec.ChannelID, rec.Name); err != nil || got != rec {
		t.Fatalf("receipt: %v", err)
	}
	if _, err = readTeamsChannelReceipt(other, rec.TeamID, rec.ChannelID, rec.Name); err == nil {
		t.Fatal("cross-account receipt accepted")
	}
	if _, err = readTeamsChannelReceipt(dir, rec.TeamID, rec.ChannelID, "Other"); err == nil {
		t.Fatal("renamed channel accepted")
	}
	if err = markTeamsChannelReceipt(dir, rec, "delete_in_progress"); err != nil {
		t.Fatal(err)
	}
	if _, err = readTeamsChannelReceipt(dir, rec.TeamID, rec.ChannelID, rec.Name); err == nil {
		t.Fatal("in-progress delete could be repeated")
	}
}
func TestTeamsChannelPreviewConfirmationAndGeneralGuard(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	t.Setenv("TEAMS_WEB_SESSION_DIR", t.TempDir())
	for _, tc := range []struct {
		args    []string
		wantErr bool
	}{
		{[]string{"channels", "create", "--team", syntheticOwnedTeam, "--if-team-name", "Synthetic Team", "--anchor-channel", syntheticChannel, "--name", "Synthetic Channel", "--json", "--no-learn"}, false},
		{[]string{"channels", "create", "--team", syntheticOwnedTeam, "--if-team-name", "Synthetic Team", "--anchor-channel", syntheticOwnedTeam, "--name", "Synthetic Channel", "--json", "--no-learn"}, false},
		{[]string{"channels", "delete", syntheticOwnedTeam, "--team", syntheticOwnedTeam, "--if-team-name", "Synthetic Team", "--if-name", "Synthetic Channel", "--json", "--no-learn"}, true},
		{[]string{"channels", "delete", syntheticChannel, "--team", syntheticOwnedTeam, "--if-team-name", "Synthetic Team", "--if-name", "Synthetic Channel", "--json", "--no-learn"}, false},
		{[]string{"channels", "create", "--team", syntheticOwnedTeam, "--if-team-name", "Synthetic Team", "--anchor-channel", syntheticChannel, "--name", "Synthetic Channel", "--execute", "--json", "--no-learn"}, true},
		{[]string{"channels", "delete", syntheticChannel, "--team", syntheticOwnedTeam, "--if-team-name", "Synthetic Team", "--if-name", "General", "--json", "--no-learn"}, true},
	} {
		root := newRootCmd(&rootFlags{})
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(tc.args)
		err := root.Execute()
		if (err != nil) != tc.wantErr {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if err == nil {
			var result map[string]any
			if json.Unmarshal(out.Bytes(), &result) != nil || result["executed"] != false {
				t.Fatalf("not preview: %s", out.String())
			}
		}
	}
}

func TestTeamsChannelDeleteClaimPreventsConcurrentAndLaterRepeats(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	dir, err := teamsChannelReceiptDir(teamsTokenPrincipal{Tenant: "tenant", Object: "one"}, true)
	if err != nil {
		t.Fatal(err)
	}
	rec := teamsChannelReceipt{Version: 1, State: "active", TeamID: syntheticOwnedTeam, ChannelID: syntheticChannel, Name: "Synthetic Channel", GroupID: syntheticGroup}
	if err = writeTeamsChannelReceipt(dir, rec); err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var writes atomic.Int32
	preflight := func(teamsChannelReceipt) error { return nil }
	first := func(teamsChannelReceipt) (string, error) {
		writes.Add(1)
		close(entered)
		<-release
		return syntheticChannel, nil
	}
	go func() {
		_, updated, err := claimTeamsChannelDelete(dir, rec.TeamID, rec.ChannelID, rec.Name, preflight, first)
		if err == nil && !updated {
			err = errors.New("receipt not updated")
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first delete did not enter")
	}
	second := func(teamsChannelReceipt) (string, error) { writes.Add(1); return syntheticChannel, nil }
	if _, _, err = claimTeamsChannelDelete(dir, rec.TeamID, rec.ChannelID, rec.Name, preflight, second); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("concurrent claim: %v", err)
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if _, _, err = claimTeamsChannelDelete(dir, rec.TeamID, rec.ChannelID, rec.Name, preflight, second); err == nil {
		t.Fatal("completed delete retried")
	}
	if writes.Load() != 1 {
		t.Fatalf("provider calls = %d", writes.Load())
	}
}
func TestTeamsChannelDeleteClaimRetainsUnknownOutcome(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	dir, err := teamsChannelReceiptDir(teamsTokenPrincipal{Tenant: "tenant", Object: "one"}, true)
	if err != nil {
		t.Fatal(err)
	}
	rec := teamsChannelReceipt{Version: 1, State: "active", TeamID: syntheticOwnedTeam, ChannelID: syntheticChannel, Name: "Synthetic Channel", GroupID: syntheticGroup}
	if err = writeTeamsChannelReceipt(dir, rec); err != nil {
		t.Fatal(err)
	}
	writes := 0
	pre := func(teamsChannelReceipt) error { return nil }
	write := func(teamsChannelReceipt) (string, error) { writes++; return "", errors.New("outcome_unknown") }
	if _, _, err = claimTeamsChannelDelete(dir, rec.TeamID, rec.ChannelID, rec.Name, pre, write); err == nil || !strings.Contains(err.Error(), "outcome_unknown") {
		t.Fatal("unknown outcome was not surfaced")
	}
	if _, _, err = claimTeamsChannelDelete(dir, rec.TeamID, rec.ChannelID, rec.Name, pre, write); err == nil || writes != 1 {
		t.Fatal("unknown delete was retried")
	}
}

func TestTeamsChannelDeleteClaimAcrossProcesses(t *testing.T) {
	if os.Getenv("TEAMS_CHANNEL_LOCK_CHILD") == "1" {
		dir := os.Getenv("TEAMS_CHANNEL_LOCK_DIR")
		ready := filepath.Join(dir, "child-ready")
		release := filepath.Join(dir, "child-release")
		_, _, err := claimTeamsChannelDelete(dir, syntheticOwnedTeam, syntheticChannel, "Synthetic Channel", func(teamsChannelReceipt) error { return nil }, func(teamsChannelReceipt) (string, error) {
			if err := os.WriteFile(ready, []byte("ready"), 0600); err != nil {
				return "", err
			}
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(release); err == nil {
					return syntheticChannel, nil
				}
				time.Sleep(10 * time.Millisecond)
			}
			return "", errors.New("release timed out")
		})
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	dir, err := teamsChannelReceiptDir(teamsTokenPrincipal{Tenant: "tenant", Object: "one"}, true)
	if err != nil {
		t.Fatal(err)
	}
	rec := teamsChannelReceipt{Version: 1, State: "active", TeamID: syntheticOwnedTeam, ChannelID: syntheticChannel, Name: "Synthetic Channel", GroupID: syntheticGroup}
	if err = writeTeamsChannelReceipt(dir, rec); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTeamsChannelDeleteClaimAcrossProcesses$", "-test.count=1")
	cmd.Env = append(os.Environ(), "TEAMS_CHANNEL_LOCK_CHILD=1", "TEAMS_CHANNEL_LOCK_DIR="+dir)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	ready := filepath.Join(dir, "child-ready")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err = os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = os.Stat(ready); err != nil {
		t.Fatalf("child did not claim: %s", output.String())
	}
	calls := 0
	_, _, err = claimTeamsChannelDelete(dir, rec.TeamID, rec.ChannelID, rec.Name, func(teamsChannelReceipt) error { return nil }, func(teamsChannelReceipt) (string, error) { calls++; return syntheticChannel, nil })
	if err == nil || !strings.Contains(err.Error(), "already in progress") || calls != 0 {
		t.Fatal("cross-process duplicate delete was not blocked")
	}
	if err = os.WriteFile(filepath.Join(dir, "child-release"), []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatalf("child: %v %s", err, output.String())
	}
	if _, err = readTeamsChannelReceipt(dir, rec.TeamID, rec.ChannelID, rec.Name); err == nil {
		t.Fatal("completed receipt was still active")
	}
}

func TestTeamsGeneralAnchorRequiresMatchingTeamSpaceAndBootstrap(t *testing.T) {
	thread := `{"id":"` + syntheticOwnedTeam + `","threadProperties":{"threadType":"space","spaceThreadTopic":"Synthetic Team","groupId":"` + syntheticGroup + `","isCreator":true},"memberProperties":{"role":"Admin"}}`
	row := func() map[string]any {
		return map[string]any{"id": syntheticOwnedTeam, "parentTeamId": syntheticOwnedTeam, "groupId": syntheticGroup, "isGeneral": true, "isDeleted": false}
	}
	body := teamsBootstrap{Channels: []map[string]any{row()}}
	if group, err := validateTeamsGeneralAnchor([]byte(thread), body, syntheticOwnedTeam, "Synthetic Team"); err != nil || group != syntheticGroup {
		t.Fatalf("observed General shape rejected: %v", err)
	}
	for _, bad := range []string{strings.Replace(thread, `"isCreator":true`, `"isCreator":false`, 1), strings.Replace(thread, `"role":"Admin"`, `"role":"User"`, 1), strings.Replace(thread, `"threadType":"space"`, `"threadType":"topic"`, 1), strings.Replace(thread, syntheticGroup, "11111111-2222-4333-8444-555555555556", 1)} {
		if _, err := validateTeamsGeneralAnchor([]byte(bad), body, syntheticOwnedTeam, "Synthetic Team"); err == nil {
			t.Fatal("unsafe team space accepted")
		}
	}
	for _, change := range []func(map[string]any){func(r map[string]any) { r["isGeneral"] = false }, func(r map[string]any) { r["isDeleted"] = true }, func(r map[string]any) { r["parentTeamId"] = "19:other@thread.tacv2" }, func(r map[string]any) { r["groupId"] = "11111111-2222-4333-8444-555555555556" }} {
		r := row()
		change(r)
		if _, err := validateTeamsGeneralAnchor([]byte(thread), teamsBootstrap{Channels: []map[string]any{r}}, syntheticOwnedTeam, "Synthetic Team"); err == nil {
			t.Fatal("unsafe General row accepted")
		}
	}
}
