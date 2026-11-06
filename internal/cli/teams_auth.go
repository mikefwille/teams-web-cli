// Auth commands for the dedicated Microsoft 365 web-session adapter.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
)

func teamsDisabledFile() (string, error) {
	dir, err := cliutil.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "web-session.disabled"), nil
}

func teamsSessionDisabled() (bool, error) {
	name, err := teamsDisabledFile()
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("invalid web-session disable marker")
	}
	return true, nil
}

func setTeamsSessionDisabled(disabled bool) error {
	name, err := teamsDisabledFile()
	if err != nil {
		return err
	}
	if !disabled {
		info, statErr := os.Lstat(name)
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		if statErr != nil {
			return statErr
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("invalid web-session disable marker")
		}
		return os.Remove(name)
	}
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		_, err = teamsSessionDisabled()
		return err
	}
	if err != nil {
		return err
	}
	return f.Close()
}

func teamsSessionHelper() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("TEAMS_WEB_SESSION_HELPER")); configured != "" {
		return configured, nil
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "m365-session")
		if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() {
			return candidate, nil
		}
	}
	if found, err := exec.LookPath("m365-session"); err == nil {
		return found, nil
	}
	return "", errors.New("session helper not found; set TEAMS_WEB_SESSION_HELPER to the m365-session executable")
}

func runTeamsSessionHelper(cmd *cobra.Command, flags *rootFlags, action string, args ...string) (map[string]any, error) {
	helper, err := teamsSessionHelper()
	if err != nil {
		return nil, err
	}
	ctx, cancel := boundCtx(cmd.Context(), flags)
	if action == "recover" || action == "recover-files" {
		cancel()
		changed := cmd.Flags().Changed("timeout")
		if changed && flags.timeout > 0 && flags.timeout < 110*time.Second {
			return nil, usageErr(errors.New("auth recovery needs --timeout of at least 110s"))
		}
		duration := flags.timeout
		if !changed {
			duration = 120 * time.Second
		}
		if duration > 0 {
			ctx, cancel = context.WithTimeout(cmd.Context(), duration)
		} else {
			ctx, cancel = cmd.Context(), func() {}
		}
	}
	defer cancel()
	arguments := append([]string{action, "teams"}, args...)
	process := exec.CommandContext(ctx, helper, arguments...)
	var stdout bytes.Buffer
	process.Stdout = &stdout
	process.Stderr = ioDiscard{}
	runErr := process.Run()
	var result map[string]any
	if json.Unmarshal(stdout.Bytes(), &result) != nil || result["m365_session"] != float64(1) || result["service"] != "teams" {
		return nil, errors.New("session helper returned no valid result")
	}
	if runErr != nil || result["ok"] != true {
		reason, _ := result["reason"].(string)
		if reason == "" {
			reason = "session_helper_failed"
		}
		return result, fmt.Errorf("Teams session helper: %s", reason)
	}
	return result, nil
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

func newTeamsAuthStatusCmd(flags *rootFlags) *cobra.Command {
	var verify bool
	cmd := &cobra.Command{Use: "status", Short: "Show the dedicated Teams web-session status", Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			disabled, err := teamsSessionDisabled()
			if err != nil {
				return err
			}
			if disabled {
				return flags.printJSON(cmd, map[string]any{"authenticated": false, "source": "web-session", "reason": "logged_out_locally"})
			}
			action := "status"
			if verify {
				action = "check"
			}
			result, err := runTeamsSessionHelper(cmd, flags, action)
			if err != nil {
				return authErr(err)
			}
			return flags.printJSON(cmd, result)
		}}
	cmd.Flags().BoolVar(&verify, "verify", false, "Perform an authenticated read before reporting status")
	return cmd
}

func configureTeamsAuthChange(cmd *cobra.Command, flags *rootFlags, action string) {
	var execute bool
	short := "Refresh the dedicated Teams web session"
	if action == "recover" {
		short = "Recover the session using the dedicated headless profile"
	}
	if action == "recover-files" {
		short = "Recover the OneDrive audience using the dedicated headless profile"
	}
	if action == "import" {
		short = "Activate a captured Teams session export"
	}
	if action == "import-audience" {
		short = "Import an observed service-specific Teams credential"
	}
	if action == "logout" {
		short = "Disable this CLI's local use of the shared session"
	}
	cmd.Short = short
	cmd.Annotations = map[string]string{"mcp:hidden": "true"}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if action != "import" && action != "import-audience" && len(args) != 0 {
			return usageErr(errors.New("this command takes no positional arguments"))
		}
		if dryRunOK(flags) || !execute {
			return flags.printJSON(cmd, map[string]any{"executed": false, "action": action, "note": "pass --execute to apply"})
		}
		if cliutil.IsAnyHarness() {
			return writeHarnessRefusal(cmd.OutOrStdout(), flags, "change Teams authentication")
		}
		if action == "logout" {
			if err := setTeamsSessionDisabled(true); err != nil {
				return err
			}
			return flags.printJSON(cmd, map[string]any{"executed": true, "logged_out_locally": true, "remote_sign_out": false})
		}
		var extra []string
		helperAction := "refresh"
		if action == "recover" || action == "recover-files" {
			helperAction = action
		}
		if action == "import" {
			if len(args) != 1 || !teamsExportID.MatchString(args[0]) {
				return usageErr(errors.New("an export ID is required"))
			}
			helperAction = "activate"
			extra = args
		}
		if action == "import-audience" {
			if len(args) != 2 || !teamsExportID.MatchString(args[0]) || !teamsRequestID.MatchString(args[1]) {
				return usageErr(errors.New("a capture batch ID and numeric request ID are required"))
			}
			helperAction = "import-evidence"
			extra = args
		}
		result, err := runTeamsSessionHelper(cmd, flags, helperAction, extra...)
		if err != nil {
			return authErr(err)
		}
		if action != "import-audience" && action != "recover-files" {
			if err := setTeamsSessionDisabled(false); err != nil {
				return err
			}
		}
		return flags.printJSON(cmd, result)
	}
	cmd.Flags().BoolVar(&execute, "execute", false, "Apply the requested local authentication change")
}

func newTeamsAuthRefreshCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "refresh"}
	configureTeamsAuthChange(cmd, flags, "refresh")
	return cmd
}
func newTeamsAuthRecoverCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "recover"}
	configureTeamsAuthChange(cmd, flags, "recover")
	return cmd
}
func newTeamsAuthRecoverFilesCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "recover-files"}
	configureTeamsAuthChange(cmd, flags, "recover-files")
	return cmd
}
func newTeamsAuthLoginCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "login"}
	configureTeamsAuthChange(cmd, flags, "login")
	cmd.Flags().Bool("chrome", false, "Use the dedicated existing Chrome owner; requires --execute")
	return cmd
}
func newTeamsAuthImportCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "import [export-id]"}
	configureTeamsAuthChange(cmd, flags, "import")
	return cmd
}
func newTeamsAuthImportAudienceCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "import-audience [batch-id] [request-id]"}
	configureTeamsAuthChange(cmd, flags, "import-audience")
	return cmd
}

func newTeamsAuthAudiencesCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{Use: "audiences", Short: "List captured service-specific credential metadata", Annotations: map[string]string{"mcp:hidden": "true"}, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := runTeamsSessionHelper(cmd, flags, "catalog")
			if err != nil {
				return authErr(err)
			}
			return flags.printJSON(cmd, result)
		}}
}
func newTeamsAuthLogoutCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "logout"}
	configureTeamsAuthChange(cmd, flags, "logout")
	return cmd
}

func newTeamsAuthSetupCmd() *cobra.Command {
	return &cobra.Command{Use: "setup", Short: "Show how to reuse the dedicated Teams web session", Annotations: map[string]string{"mcp:hidden": "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "To recover the dedicated headless profile, run: teams-web-pp-cli auth recover --execute")
			return err
		}}
}

func init() {
	registerNovelCommand(func(rootCmd *cobra.Command, flags *rootFlags) {
		if parent, _, err := rootCmd.Find([]string{"auth"}); err == nil && parent != rootCmd {
			for _, existing := range parent.Commands() {
				if existing.Name() == "status" || existing.Name() == "logout" || existing.Name() == "setup" {
					parent.RemoveCommand(existing)
				}
			}
			parent.AddCommand(newTeamsAuthSetupCmd(), newTeamsAuthStatusCmd(flags), newTeamsAuthVerifyCmd(flags), newTeamsAuthRefreshCmd(flags), newTeamsAuthRecoverCmd(flags), newTeamsAuthRecoverFilesCmd(flags), newTeamsAuthLoginCmd(flags), newTeamsAuthImportCmd(flags), newTeamsAuthImportAudienceCmd(flags), newTeamsAuthAudiencesCmd(flags), newTeamsAuthLogoutCmd(flags))
		}
	})
}
