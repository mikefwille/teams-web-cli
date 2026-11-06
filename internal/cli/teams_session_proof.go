// Verify the dedicated web session before recording a bounded local proof.
package cli

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
	"github.com/mikefwille/teams-web-cli/internal/config"
)

const teamsSessionValidationPath = "/api/csa/amer/api/v3/teams/users/me"

func teamsBootstrapQuery() map[string]string {
	return map[string]string{
		"isPrefetch": "false", "enableMembershipSummary": "true", "supportsAdditionalSystemGeneratedFolders": "true", "supportsSliceItems": "true", "enableEngageCommunities": "false",
	}
}

func teamsBootstrapProofShape(data []byte) bool {
	var body map[string]json.RawMessage
	if json.Unmarshal(data, &body) != nil || body == nil {
		return false
	}
	for _, key := range []string{"teams", "chats", "channels", "privateFeeds"} {
		raw, ok := body[key]
		if !ok || string(raw) == "null" {
			continue
		}
		var rows []json.RawMessage
		if json.Unmarshal(raw, &rows) == nil && rows != nil {
			return true
		}
	}
	return false
}

func teamsProofFromCheckedSession(result map[string]any, pointer teamsSessionPointer, session *teamsSessionExport, now time.Time) (doctorBrowserSessionProof, error) {
	if result["ok"] != true || result["status"] != "service_read_ok" || result["identity_verified"] != true || result["http_status"] != float64(200) {
		return doctorBrowserSessionProof{}, errors.New("the session helper did not verify an authenticated read and identity")
	}
	exportID, ok := result["export_id"].(string)
	if !ok || pointer.Version != 1 || pointer.Service != "teams" || !teamsExportID.MatchString(exportID) || pointer.ExportID != exportID || session == nil {
		return doctorBrowserSessionProof{}, errors.New("the checked session no longer matches the active session")
	}
	header := session.Headers["authorization"]
	expires, known := accessTokenExpiry(header)
	if _, ok := teamsPrincipal(header); !ok || !known || !expires.After(now.Add(30*time.Second)) {
		return doctorBrowserSessionProof{}, errors.New("the checked session identity or expiry is unavailable")
	}
	return doctorBrowserSessionProof{
		APIName:               "teams-web",
		CookieDomain:          "",
		ValidationMethod:      "GET",
		ValidationPath:        teamsSessionValidationPath,
		StatusCode:            200,
		AuthSource:            "web-session",
		CredentialFingerprint: doctorFingerprintCredential(header),
		VerifiedAt:            now.UTC().Format(time.RFC3339),
	}, nil
}

// verifyTeamsBrowserSession checks the selected export through the shared
// helper, independently reads the declared validation endpoint through the
// CLI's guarded transport, then writes proof only if the active export is stable.
func verifyTeamsBrowserSession(cmd *cobra.Command, flags *rootFlags) (doctorBrowserSessionProof, error) {
	disabled, err := teamsSessionDisabled()
	if err != nil || disabled {
		return doctorBrowserSessionProof{}, errors.New("the Teams web session is locally disabled or unavailable")
	}
	dir, err := teamsSessionDir()
	if err != nil {
		return doctorBrowserSessionProof{}, errors.New("the Teams session directory is unavailable")
	}
	var before teamsSessionPointer
	if err := readPrivateSessionFile(filepath.Join(dir, "current-teams.json"), &before); err != nil {
		return doctorBrowserSessionProof{}, errors.New("the active Teams session is unavailable")
	}
	session, err := loadTeamsWebSession(dir)
	if err != nil {
		return doctorBrowserSessionProof{}, errors.New("the active Teams session is unavailable or expired")
	}
	result, err := runTeamsSessionHelper(cmd, flags, "check")
	if err != nil {
		return doctorBrowserSessionProof{}, errors.New("the shared session check failed")
	}
	proof, err := teamsProofFromCheckedSession(result, before, session, time.Now())
	if err != nil {
		return doctorBrowserSessionProof{}, err
	}
	c, err := flags.newClient()
	if err != nil || c == nil || c.Config == nil || c.Config.AuthSource != "web-session" || c.Config.AuthHeader() != session.Headers["authorization"] {
		return doctorBrowserSessionProof{}, errors.New("the CLI is not using the checked Teams web session")
	}
	ctx, cancel := boundCtx(cmd.Context(), flags)
	defer cancel()
	data, err := c.GetNoCache(ctx, teamsSessionValidationPath, teamsBootstrapQuery())
	if err != nil || !teamsBootstrapProofShape(data) {
		return doctorBrowserSessionProof{}, errors.New("the Teams validation endpoint did not return an authenticated bootstrap")
	}
	var after teamsSessionPointer
	if err := readPrivateSessionFile(filepath.Join(dir, "current-teams.json"), &after); err != nil || after != before {
		return doctorBrowserSessionProof{}, errors.New("the active Teams session changed during verification")
	}
	current, err := loadTeamsWebSession(dir)
	if err != nil || current.Headers["authorization"] != session.Headers["authorization"] {
		return doctorBrowserSessionProof{}, errors.New("the Teams credential changed during verification")
	}
	if err := writeTeamsBrowserSessionProof(proof); err != nil {
		return doctorBrowserSessionProof{}, errors.New("could not save the private browser-session proof")
	}
	return proof, nil
}

func applyTeamsDoctorSessionProof(report map[string]any, cfg *config.Config, header string, body []byte, readErr error) {
	if readErr != nil || !teamsBootstrapProofShape(body) {
		report["credentials"] = "invalid (current Teams validation read failed)"
		report["browser_session_proof_detail"] = "the current Teams validation read did not succeed"
		return
	}
	if ok, detail := browserSessionProofStatus(cfg, header); ok {
		report["credentials"] = "valid"
		report["credentials_detail"] = detail
		report["browser_session_proof"] = "valid"
		report["browser_session_proof_detail"] = detail
	} else {
		report["credentials"] = "invalid (browser-session proof missing or stale)"
		report["credentials_detail"] = detail
		report["browser_session_proof_detail"] = detail
	}
}

func newTeamsAuthVerifyCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{Use: "verify", Short: "Verify the active Teams session and save a private proof", Args: cobra.NoArgs, Annotations: map[string]string{"mcp:hidden": "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dryRunOK(flags) {
				return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "verify browser session"})
			}
			if cliutil.IsAnyHarness() {
				return writeHarnessRefusal(cmd.OutOrStdout(), flags, "verify browser session")
			}
			proof, err := verifyTeamsBrowserSession(cmd, flags)
			if err != nil {
				return authErr(err)
			}
			return flags.printJSON(cmd, map[string]any{"browser_session_proof": "valid", "validation_method": proof.ValidationMethod, "validation_path": proof.ValidationPath, "verified_at": proof.VerifiedAt, "jwt_signature_verified_locally": false})
		}}
}
