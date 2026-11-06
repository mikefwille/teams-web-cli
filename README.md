# Teams Web CLI

Read and work with a selected Microsoft Teams web-session account from a CLI or
an MCP agent. The CLI also supports local drafts and selected operations on
the account's own OneDrive files.

Created by [@mikefwille](https://github.com/mikefwille).

## Install

Go 1.26.10 or newer is required. After the repository is published, install the
two binaries with:

```bash
go install github.com/mikefwille/teams-web-cli/cmd/teams-web-pp-cli@latest
go install github.com/mikefwille/teams-web-cli/cmd/teams-web-pp-mcp@latest
```

Or build them from a checkout with `make build-all`; the binaries are written
to `bin/`. Add that directory or your Go install directory to `PATH`. Agent
instructions are in [SKILL.md](SKILL.md).

## Authentication

This CLI uses a private, existing Microsoft 365 web session. **This repository
does not include the `m365-session` browser helper needed to create or renew
that session.** A separately obtained compatible helper is required for initial
sign-in, session status, import, refresh, and recovery. The helper used during
development is shared with an Outlook web CLI, but has not been packaged or
verified for general installation. There is currently no self-contained public
sign-in setup in this repository.

If you already have a compatible `m365-session` helper, put it on `PATH` or
set `TEAMS_WEB_SESSION_HELPER` to its executable. Keep its private session
exports outside this checkout. The CLI reads them from
`~/.local/share/outlook-web-dev/private` by default; set `M365_WEB_HOME` to an
absolute shared Microsoft 365 home to use its `private` subdirectory, or set
`TEAMS_WEB_SESSION_DIR` to the export directory directly. The CLI and helper
must refer to the same private session. Some commands require an additional,
separately captured service audience.

```bash
teams-web-pp-cli auth status --json --no-learn
teams-web-pp-cli whoami --json --no-learn
```

Confirm the selected account before working. `whoami` reports claims from the
service-accepted session; it does not independently verify JWT signatures.
`auth logout --execute` disables this CLI's local use of the session; it does
not sign out Microsoft or delete the shared exports. Never put tokens, cookies,
browser profiles, exports, or private messages in this repository.

## Use

```bash
teams-web-pp-cli chats list --json --no-learn
teams-web-pp-cli teams list --json --no-learn
teams-web-pp-cli messages list --conversation "$CHAT_ID" --max-pages 3 --json --no-learn
teams-web-pp-cli search messages --query "$QUERY" --conversation "$CHAT_ID" --json --no-learn
teams-web-pp-cli messages send --conversation "$CHAT_ID" --text "$TEXT" --json --no-learn
```

Message changes preview by default; add `--execute --confirm` to apply an
explicitly requested change. If a write has an uncertain result, check the
destination before trying again. Drafts are local and do not sync to Teams.
Reads and search are bounded and may be incomplete. Run
`teams-web-pp-cli --help` for commands and flags.

For owned OneDrive file commands, set `M365_ONEDRIVE_ORIGIN` to the account's
origin, for example `https://example-my.sharepoint.com`, and import compatible
account-specific OneDrive evidence through the external session helper. The
origin must be HTTPS and have the form `<tenant>-my.sharepoint.com`. Commands
are limited to observed owned-file routes; attachment sharing is not supported.

## MCP

Configure your MCP agent to launch `teams-web-pp-mcp` over stdio. For example:

```json
{
  "mcpServers": {
    "teams-web": {
      "command": "/absolute/path/to/teams-web-pp-mcp"
    }
  }
}
```

Keep `teams-web-pp-cli` beside the MCP binary or available on `PATH` (or set
`TEAMS_WEB_CLI_PATH`). The server inherits the local session and environment;
it does not expose an HTTP listener.

## Status and limits

This v1 was exercised on macOS with one account; live message-write checks used
that account's self-chat. Other tenants, destinations, policies, and the public
installation path have not been verified. Complete history, complete mention
coverage, reconciliation of older message changes, broader administration,
channel-thread replies, and attachment sharing are outside v1.

## Development

```bash
make test
make build-all
```

The test fixtures are synthetic. Never add real credentials or session captures
to test data. This project is licensed under the Apache License 2.0; see
[LICENSE](LICENSE) and [NOTICE](NOTICE).
