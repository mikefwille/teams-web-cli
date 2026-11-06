// Teams read workflows backed by the observed current-user bootstrap request.
package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/platform"
)

type teamsBootstrap struct {
	Teams               []map[string]any `json:"teams"`
	Chats               []map[string]any `json:"chats"`
	Channels            []map[string]any `json:"channels"`
	PrivateFeeds        []map[string]any `json:"privateFeeds"`
	ConversationFolders json.RawMessage  `json:"conversationFolders"`
	Metadata            map[string]any   `json:"metadata"`
}

// pp:data-source live
func fetchTeamsBootstrap(cmd *cobra.Command, flags *rootFlags) (teamsBootstrap, error) {
	c, err := flags.newClient()
	if err != nil {
		return teamsBootstrap{}, err
	}
	return fetchTeamsBootstrapWithClient(cmd, flags, c)
}

func fetchTeamsBootstrapWithClient(cmd *cobra.Command, flags *rootFlags, c *client.Client) (teamsBootstrap, error) {
	ctx, cancel := boundCtx(cmd.Context(), flags)
	defer cancel()
	data, err := c.GetNoCache(ctx, teamsSessionValidationPath, teamsBootstrapQuery())
	if err != nil {
		return teamsBootstrap{}, classifyAPIErrorOnly(err)
	}
	var body teamsBootstrap
	if err := json.Unmarshal(data, &body); err != nil {
		return teamsBootstrap{}, errors.New("Teams returned an invalid bootstrap response")
	}
	if body.Teams == nil {
		body.Teams = []map[string]any{}
	}
	if body.Chats == nil {
		body.Chats = []map[string]any{}
	}
	if body.Channels == nil {
		body.Channels = []map[string]any{}
	}
	return body, nil
}

func teamsObjectID(item map[string]any) string {
	for _, key := range []string{"id", "teamId", "threadId"} {
		if s, ok := item[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func collectTeamsChannels(body teamsBootstrap) []map[string]any {
	out := make([]map[string]any, 0)
	seen := map[string]bool{}
	add := func(item map[string]any) {
		id := teamsObjectID(item)
		if id != "" && seen[id] {
			return
		}
		if id != "" {
			seen[id] = true
		}
		out = append(out, item)
	}
	for _, item := range body.Channels {
		add(item)
	}
	for _, team := range body.Teams {
		if nested, ok := team["channels"].([]any); ok {
			for _, value := range nested {
				if item, ok := value.(map[string]any); ok {
					add(item)
				}
			}
		}
	}
	return out
}

func teamsCollectionIsPartial(resource string, metadata map[string]any) bool {
	// The bootstrap response can say no more chats while omitting a known
	// self-chat. Its pagination flags do not establish complete coverage.
	if resource == "chats" {
		return true
	}
	partial, _ := metadata["isPartialData"].(bool)
	return partial
}

func teamsCollectionCommand(flags *rootFlags, resource, mode string) *cobra.Command {
	var limit int
	var parentTeam string
	use := mode
	if mode == "get" {
		use = "get [id]"
	}
	cmd := &cobra.Command{Use: use, Short: fmt.Sprintf("%s %s from the current Teams account", mode, resource),
		Annotations: map[string]string{"mcp:read-only": "true", "pp:data-source": "live"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return flags.printJSON(cmd, map[string]any{"dry_run": true, "resource": resource, "action": mode})
			}
			if mode == "get" && (len(args) != 1 || strings.TrimSpace(args[0]) == "") {
				return usageErr(errors.New("one resource ID is required"))
			}
			if mode == "list" && len(args) != 0 {
				return usageErr(errors.New("list takes no positional arguments"))
			}
			if limit < 1 || limit > 1000 {
				return usageErr(errors.New("--limit must be between 1 and 1000"))
			}
			body, err := fetchTeamsBootstrap(cmd, flags)
			if err != nil {
				return err
			}
			items := body.Teams
			if resource == "chats" {
				items = collectTeamsChats(body)
			}
			if resource == "channels" {
				items = collectTeamsChannels(body)
			}
			out := make([]map[string]any, 0)
			matched := 0
			for _, item := range items {
				if parentTeam != "" {
					p, _ := item["parentTeamId"].(string)
					if p != parentTeam {
						continue
					}
				}
				if mode == "get" && teamsObjectID(item) != args[0] {
					continue
				}
				matched++
				if len(out) < limit {
					out = append(out, item)
				}
			}
			if mode == "get" && len(out) == 0 {
				return fmt.Errorf("%s ID not found in the current bootstrap response", resource)
			}
			if mode == "get" {
				return flags.printJSON(cmd, map[string]any{"result": out[0], "source": "live_bootstrap"})
			}
			result := map[string]any{"results": out, "count": len(out), "source": "live_bootstrap", "partial": teamsCollectionIsPartial(resource, body.Metadata), "truncated": matched > len(out)}
			if resource == "chats" {
				result["coverage"] = "bootstrap chats and recognized private-feed/section references only; other conversations may be absent"
			}
			return flags.printJSON(cmd, result)
		}}
	cmd.Flags().IntVar(&limit, "limit", 100, "Maximum number of returned objects")
	if resource == "channels" {
		cmd.Flags().StringVar(&parentTeam, "team", "", "Filter by the exact parent team ID")
		if mode == "get" {
			cmd.Example = `teams-web-pp-cli channels get "$CHANNEL_ID" --json --no-learn`
		}
	}
	return cmd
}

func newTeamsMembersCommand(flags *rootFlags) *cobra.Command {
	var pageSize int
	cmd := &cobra.Command{Use: "members [thread-id]", Short: "Read members of an observed Teams thread or channel", Annotations: map[string]string{"mcp:read-only": "true", "pp:data-source": "live"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "members"})
			}
			if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
				return usageErr(errors.New("one thread ID is required"))
			}
			if pageSize < 1 || pageSize > 100 {
				return usageErr(errors.New("--page-size must be between 1 and 100"))
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			path := "/api/chatsvc/amer/v1/threads/" + url.PathEscape(args[0]) + "/members"
			data, err := c.GetNoCache(ctx, path, map[string]string{"view": "msnp24Equivalent", "selectMemberRoles": "User|Admin|Guest", "pageSize": fmt.Sprint(pageSize)})
			if err != nil {
				return classifyAPIErrorOnly(err)
			}
			var result any
			if json.Unmarshal(data, &result) != nil {
				return errors.New("Teams returned invalid members JSON")
			}
			return flags.printJSON(cmd, map[string]any{"result": result, "source": "live", "pagination": "server response may include a continuation token"})
		}}
	cmd.Flags().IntVar(&pageSize, "page-size", 30, "Maximum members requested in one page")
	return cmd
}

func newTeamsWhoami(flags *rootFlags) *cobra.Command {
	return &cobra.Command{Use: "whoami", Short: "Show the account identity associated with the accepted web session", Annotations: map[string]string{"mcp:read-only": "true", "pp:data-source": "live"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dryRunOK(flags) {
				return flags.printJSON(cmd, map[string]any{"dry_run": true, "action": "whoami"})
			}
			if flags.platformSession != nil {
				if flags.platformSession.GateOutcome != platform.GateVerified {
					return errors.New("verified client profile session is required")
				}
				return flags.printJSON(cmd, platformIdentityReport(flags.platformSession))
			}
			if _, err := fetchTeamsBootstrap(cmd, flags); err != nil {
				return err
			}
			dir, err := teamsSessionDir()
			if err != nil {
				return err
			}
			session, err := loadTeamsWebSession(dir)
			if err != nil {
				return authErr(err)
			}
			parts := strings.Split(strings.TrimSpace(strings.TrimPrefix(session.Headers["authorization"], "Bearer ")), ".")
			if len(parts) != 3 {
				return errors.New("session identity claim is unavailable")
			}
			payload, err := base64.RawURLEncoding.DecodeString(parts[1])
			if err != nil {
				return errors.New("session identity claim is unavailable")
			}
			var claims map[string]any
			if json.Unmarshal(payload, &claims) != nil {
				return errors.New("session identity claim is unavailable")
			}
			identity := map[string]any{"source": "accepted_access_token_claim", "jwt_signature_verified_locally": false}
			for _, key := range []string{"upn", "unique_name", "name", "tid", "oid"} {
				if value, ok := claims[key].(string); ok && value != "" {
					identity[key] = value
				}
			}
			return flags.printJSON(cmd, identity)
		}}
}

func newTeamsChatsCmd(flags *rootFlags) *cobra.Command {
	group := &cobra.Command{Use: "chats", Short: "Browse Teams chats", Annotations: map[string]string{"pp:parent-group": "true"}, RunE: parentNoSubcommandRunE(flags)}
	addNovelCommandIfAbsent(group, teamsCollectionCommand(flags, "chats", "list"))
	addNovelCommandIfAbsent(group, teamsCollectionCommand(flags, "chats", "get"))
	group.AddCommand(newTeamsFavoriteStatusCmd(flags))
	group.AddCommand(newTeamsFavoriteCmd(flags))
	group.AddCommand(newTeamsUnfavoriteCmd(flags))
	group.AddCommand(newTeamsChatHideHistoryCmd(flags))
	group.AddCommand(newTeamsChatShowHistoryCmd(flags))
	group.AddCommand(newTeamsChatHideCmd(flags))
	group.AddCommand(newTeamsChatUnhideCmd(flags))
	return group
}

func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		for _, name := range []string{"teams", "chats", "channels"} {
			var group *cobra.Command
			for _, existing := range root.Commands() {
				if existing.Name() == name {
					group = existing
					break
				}
			}
			if group == nil {
				group = &cobra.Command{Use: name, Short: "Browse Teams " + name, RunE: parentNoSubcommandRunE(flags)}
				root.AddCommand(group)
			}
			addNovelCommandIfAbsent(group, teamsCollectionCommand(flags, name, "list"))
			addNovelCommandIfAbsent(group, teamsCollectionCommand(flags, name, "get"))
			if name == "chats" {
				addNovelCommandIfAbsent(group, newTeamsFavoriteStatusCmd(flags))
				addNovelCommandIfAbsent(group, newTeamsFavoriteCmd(flags))
				addNovelCommandIfAbsent(group, newTeamsUnfavoriteCmd(flags))
			}
		}
		addNovelCommandIfAbsent(root, newTeamsMembersCommand(flags))
		addNovelCommandIfAbsent(root, newTeamsWhoami(flags))
	})
}
