package cli

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mikefwille/teams-web-cli/internal/cliutil"
	"github.com/mikefwille/teams-web-cli/internal/config"
)

func syntheticTeamsProofHeader(exp time.Time) string {
	claims, _ := json.Marshal(map[string]any{"tid": "synthetic-tenant", "oid": "synthetic-object", "exp": exp.Unix()})
	return "Bearer synthetic." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
}

func TestTeamsBrowserProofRequiresCheckedCurrentIdentity(t *testing.T) {
	now := time.Now().UTC()
	pointer := teamsSessionPointer{Version: 1, Service: "teams", ExportID: "sample-1234abcd"}
	session := &teamsSessionExport{Headers: map[string]string{"authorization": syntheticTeamsProofHeader(now.Add(time.Hour))}}
	good := map[string]any{"ok": true, "status": "service_read_ok", "identity_verified": true, "http_status": float64(200), "export_id": pointer.ExportID}
	proof, err := teamsProofFromCheckedSession(good, pointer, session, now)
	if err != nil || proof.AuthSource != "web-session" || proof.ValidationMethod != "GET" || proof.ValidationPath != teamsSessionValidationPath || proof.CredentialFingerprint != doctorFingerprintCredential(session.Headers["authorization"]) {
		t.Fatalf("valid checked session was not represented correctly: %#v %v", proof, err)
	}
	for _, tc := range []struct {
		name   string
		result map[string]any
		ptr    teamsSessionPointer
		sess   *teamsSessionExport
	}{
		{"no identity", map[string]any{"ok": true, "status": "service_read_ok", "http_status": float64(200), "export_id": pointer.ExportID}, pointer, session},
		{"forbidden", map[string]any{"ok": true, "status": "forbidden", "identity_verified": true, "http_status": float64(403), "export_id": pointer.ExportID}, pointer, session},
		{"changed export", good, teamsSessionPointer{Version: 1, Service: "teams", ExportID: "other-1234abcd"}, session},
		{"expired token", good, pointer, &teamsSessionExport{Headers: map[string]string{"authorization": syntheticTeamsProofHeader(now.Add(-time.Second))}}},
		{"unknown expiry", good, pointer, &teamsSessionExport{Headers: map[string]string{"authorization": "Bearer opaque"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := teamsProofFromCheckedSession(tc.result, tc.ptr, tc.sess, now); err == nil {
				t.Fatal("unchecked or stale session produced proof")
			}
		})
	}
}

func TestTeamsBrowserProofRequiresBootstrapShape(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"teams":[]}`, true},
		{`{"privateFeeds":[{"id":"synthetic"}]}`, true},
		{`{}`, false},
		{`{"teams":null}`, false},
		{`{"teams":"not-an-array"}`, false},
		{`<html>sign in</html>`, false},
	} {
		if got := teamsBootstrapProofShape([]byte(tc.body)); got != tc.want {
			t.Errorf("shape %s: got %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestTeamsBrowserProofIsPrivateBoundAndFresh(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	t.Setenv("TEAMS_WEB_DATA_DIR", "")
	restore, err := cliutil.SetHomeOverride("")
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	header := syntheticTeamsProofHeader(time.Now().Add(time.Hour))
	proof := doctorBrowserSessionProof{APIName: "teams-web", ValidationMethod: "GET", ValidationPath: teamsSessionValidationPath, StatusCode: 200, AuthSource: "web-session", CredentialFingerprint: doctorFingerprintCredential(header), VerifiedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := writeTeamsBrowserSessionProof(proof); err != nil {
		t.Fatal(err)
	}
	dir, err := cliutil.DataDir()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "browser-session-proof.json")
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("proof file permissions: %v %v", info, err)
	}
	data, err := os.ReadFile(file)
	if err != nil || strings.Contains(string(data), header) {
		t.Fatal("proof exposed a credential or was unreadable")
	}
	cfg := &config.Config{Path: filepath.Join(t.TempDir(), "config.toml")}
	if ok, detail := browserSessionProofStatus(cfg, header); !ok {
		t.Fatalf("valid proof rejected: %s", detail)
	}
	report := map[string]any{"browser_session_proof": "missing or stale"}
	applyTeamsDoctorSessionProof(report, cfg, header, []byte(`{"teams":[]}`), nil)
	if report["browser_session_proof"] != "valid" || report["credentials"] != "valid" {
		t.Fatalf("valid live read and proof did not reach doctor: %#v", report)
	}
	report = map[string]any{"browser_session_proof": "missing or stale"}
	applyTeamsDoctorSessionProof(report, cfg, header, []byte(`<html>sign in</html>`), nil)
	if report["browser_session_proof"] == "valid" || report["credentials"] == "valid" {
		t.Fatalf("sign-in page was accepted as a live read: %#v", report)
	}
	if ok, _ := browserSessionProofStatus(cfg, header+"changed"); ok {
		t.Fatal("proof accepted another credential")
	}
	proof.VerifiedAt = time.Now().Add(-31 * time.Minute).UTC().Format(time.RFC3339)
	if err := writeTeamsBrowserSessionProof(proof); err != nil {
		t.Fatal(err)
	}
	if ok, _ := browserSessionProofStatus(cfg, header); ok {
		t.Fatal("stale proof accepted")
	}
	proof.VerifiedAt = time.Now().UTC().Format(time.RFC3339)
	proof.AuthSource = "other"
	if err := writeTeamsBrowserSessionProof(proof); err != nil {
		t.Fatal(err)
	}
	if ok, _ := browserSessionProofStatus(cfg, header); ok {
		t.Fatal("proof from another auth source accepted")
	}
	proof.AuthSource = "web-session"
	if err := writeTeamsBrowserSessionProof(proof); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if ok, _ := browserSessionProofStatus(cfg, header); ok {
		t.Fatal("insecure proof permissions accepted")
	}
}
