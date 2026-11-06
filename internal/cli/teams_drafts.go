// Local drafts never call the Teams message service.
package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
)

var teamsDraftID = regexp.MustCompile(`^[a-f0-9]{32}$`)

type teamsDraft struct {
	ID           string `json:"id"`
	Conversation string `json:"conversation"`
	Text         string `json:"text"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	Storage      string `json:"storage"`
}

func teamsDraftDir() (string, error) {
	disabled, err := teamsSessionDisabled()
	if err != nil {
		return "", err
	}
	if disabled {
		return "", errors.New("Teams web session is locally disabled")
	}
	dir, err := teamsSessionDir()
	if err != nil {
		return "", err
	}
	session, err := loadTeamsWebSession(dir)
	if err != nil {
		return "", fmt.Errorf("loading account for local drafts: %w", err)
	}
	principal, ok := teamsPrincipal(session.Headers["authorization"])
	if !ok {
		return "", errors.New("Teams account identity is unavailable")
	}
	base, err := cliutil.DataDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(principal.Tenant + "\x00" + principal.Object))
	return filepath.Join(base, "teams-drafts", hex.EncodeToString(sum[:])), nil
}

func teamsDraftPath(dir, id string) (string, error) {
	if !teamsDraftID.MatchString(id) {
		return "", usageErr(errors.New("invalid draft ID"))
	}
	return filepath.Join(dir, id+".json"), nil
}

func readTeamsDraft(dir, id string) (teamsDraft, error) {
	name, err := teamsDraftPath(dir, id)
	if err != nil {
		return teamsDraft{}, err
	}
	var d teamsDraft
	if err := readPrivateSessionFile(name, &d); err != nil {
		return teamsDraft{}, err
	}
	if d.ID != id || d.Storage != "local" || !validTeamsMessagePathID(d.Conversation) {
		return teamsDraft{}, errors.New("invalid local draft")
	}
	return d, nil
}

func writeTeamsDraft(dir string, d teamsDraft) error {
	name, err := teamsDraftPath(dir, d.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for _, p := range []string{filepath.Dir(dir), dir} {
		s, err := os.Lstat(p)
		if err != nil || !s.IsDir() || s.Mode()&os.ModeSymlink != 0 || s.Mode().Perm()&0077 != 0 {
			return errors.New("local draft directory is not private")
		}
	}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return cliutil.AtomicWritePrivateFile(name, append(data, '\n'), 0600, 0700)
}

func listTeamsDrafts(dir string) ([]teamsDraft, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []teamsDraft{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]teamsDraft, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if !teamsDraftID.MatchString(id) {
			continue
		}
		d, err := readTeamsDraft(dir, id)
		if err != nil {
			return nil, fmt.Errorf("reading local draft: %w", err)
		}
		result = append(result, d)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt > result[j].UpdatedAt })
	return result, nil
}

func newTeamsDraftListCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{Use: "list", Short: "List drafts stored only on this computer", Annotations: map[string]string{"mcp:read-only": "true"}, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dryRunOK(flags) {
				return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "drafts list"})
			}
			dir, err := teamsDraftDir()
			if err != nil {
				return authErr(err)
			}
			drafts, err := listTeamsDrafts(dir)
			if err != nil {
				return err
			}
			return flags.printJSON(cmd, map[string]any{"results": drafts, "count": len(drafts), "storage": "local"})
		}}
}

func newTeamsDraftGetCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{Use: "get [draft-id]", Short: "Read one local draft", Annotations: map[string]string{"mcp:read-only": "true"}, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "drafts get"})
			}
			dir, err := teamsDraftDir()
			if err != nil {
				return authErr(err)
			}
			draft, err := readTeamsDraft(dir, args[0])
			if err != nil {
				return err
			}
			return flags.printJSON(cmd, map[string]any{"result": draft, "storage": "local"})
		}}
}

func configureTeamsDraftChange(cmd *cobra.Command, flags *rootFlags, action string) {
	var conversation, text string
	var execute, confirm bool
	cmd.Short = "Preview or apply a local draft " + action
	cmd.Annotations = map[string]string{"mcp:read-only-unless-flags": "execute,confirm"}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if action == "create" {
			if len(args) != 0 || !validTeamsMessagePathID(conversation) || strings.TrimSpace(text) == "" {
				return usageErr(errors.New("create requires --conversation and --text"))
			}
		} else if len(args) != 1 || !teamsDraftID.MatchString(args[0]) {
			return usageErr(errors.New(action + " requires one draft ID"))
		}
		if action == "update" && strings.TrimSpace(text) == "" {
			return usageErr(errors.New("update requires --text"))
		}
		preview := map[string]any{"executed": false, "action": action, "storage": "local", "sends_message": false}
		var dir string
		var existing teamsDraft
		if action == "create" {
			preview["conversation"] = conversation
			preview["text"] = text
		} else {
			var err error
			dir, err = teamsDraftDir()
			if err != nil {
				return authErr(err)
			}
			existing, err = readTeamsDraft(dir, args[0])
			if err != nil {
				return err
			}
			preview["draft_id"] = args[0]
			preview["current"] = existing
			if action == "update" {
				preview["text"] = text
			}
		}
		if flags.dryRun || !execute {
			preview["note"] = "pass --execute --confirm to apply locally"
			return flags.printJSON(cmd, preview)
		}
		if !confirm {
			return usageErr(errors.New("--execute requires --confirm"))
		}
		if cliutil.IsAnyHarness() {
			return errors.New("draft changes are disabled in verification and dogfood harnesses")
		}
		if action == "create" {
			dir, err := teamsDraftDir()
			if err != nil {
				return authErr(err)
			}
			id := make([]byte, 16)
			if _, err := rand.Read(id); err != nil {
				return err
			}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			d := teamsDraft{ID: hex.EncodeToString(id), Conversation: conversation, Text: text, CreatedAt: now, UpdatedAt: now, Storage: "local"}
			if err := writeTeamsDraft(dir, d); err != nil {
				return err
			}
			return flags.printJSON(cmd, map[string]any{"executed": true, "result": d, "sends_message": false})
		}
		d := existing
		if action == "update" {
			d.Text = text
			d.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			if err := writeTeamsDraft(dir, d); err != nil {
				return err
			}
			return flags.printJSON(cmd, map[string]any{"executed": true, "result": d, "sends_message": false})
		}
		name, err := teamsDraftPath(dir, d.ID)
		if err != nil {
			return err
		}
		if err := os.Remove(name); err != nil {
			return err
		}
		return flags.printJSON(cmd, map[string]any{"executed": true, "draft_id": d.ID, "storage": "local", "sends_message": false})
	}
	cmd.Flags().BoolVar(&execute, "execute", false, "Apply the local change")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm the local change")
	if action == "create" {
		cmd.Flags().StringVar(&conversation, "conversation", "", "Opaque conversation ID")
	}
	if action == "create" || action == "update" {
		cmd.Flags().StringVar(&text, "text", "", "Draft text")
	}
}

func newTeamsDraftCreateCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "create"}
	configureTeamsDraftChange(cmd, flags, "create")
	cmd.Example = `teams-web-pp-cli drafts create --conversation "$CONVERSATION_ID" --text 'Draft text' --json --no-learn`
	return cmd
}
func newTeamsDraftUpdateCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "update [draft-id]"}
	configureTeamsDraftChange(cmd, flags, "update")
	return cmd
}
func newTeamsDraftDeleteCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "delete [draft-id]"}
	configureTeamsDraftChange(cmd, flags, "delete")
	return cmd
}
func newTeamsDraftsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "drafts", Short: "Manage local-only message drafts", Annotations: map[string]string{"pp:parent-group": "true"}, RunE: parentNoSubcommandRunE(flags)}
	cmd.AddCommand(newTeamsDraftListCmd(flags), newTeamsDraftGetCmd(flags), newTeamsDraftCreateCmd(flags), newTeamsDraftUpdateCmd(flags), newTeamsDraftDeleteCmd(flags))
	return cmd
}
func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) { addNovelCommandIfAbsent(root, newTeamsDraftsCmd(flags)) })
}
