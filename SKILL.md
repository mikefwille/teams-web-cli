---
name: pp-teams-web
description: Read and search Teams or perform an explicitly requested message action using the Teams web-session CLI.
---

# Teams web-session CLI

Use `teams-web-pp-cli`; its stdio MCP server is `teams-web-pp-mcp`. Installation
and the external `m365-session` helper prerequisite are described in the
repository README. This repository does not provide the helper required to
create or renew a session.

## Session

```bash
teams-web-pp-cli --version --no-learn
teams-web-pp-cli auth status --json --no-learn
teams-web-pp-cli whoami --json --no-learn
```

Confirm the intended account before working. `whoami` uses claims from the
service-accepted session; JWT signatures are not independently verified
locally. Normal reads use the saved session without starting a browser.
Session refresh and recovery require a compatible external helper. Coordinate
with the browser owner before starting either operation. `auth logout
--execute` disables local use in that CLI home; it does not sign out Microsoft
or delete shared session exports.

## Read and search

```bash
teams-web-pp-cli chats list --json --no-learn
teams-web-pp-cli teams list --json --no-learn
teams-web-pp-cli messages list --conversation "$CHAT_ID" --max-pages 3 --json --no-learn
teams-web-pp-cli search messages --query "$QUERY" --conversation "$CHAT_ID" --json --no-learn
teams-web-pp-cli presence get --json --no-learn
```

Use IDs returned by Teams as opaque values. Reads and search are bounded; say
when results are partial rather than treating absence as proof. Some operations
require a separately captured service audience. If it is unavailable or
expired, report the specific error.

## Messages and drafts

Message changes preview by default. Send only when the user has requested it;
add both `--execute --confirm` to apply a previewed change. Do not retry an
uncertain write until its result has been checked.

```bash
teams-web-pp-cli messages send --conversation "$CHAT_ID" --text "$TEXT" --json --no-learn
teams-web-pp-cli messages reply "$MESSAGE_ID" --conversation "$CHAT_ID" --text "$TEXT" --json --no-learn
teams-web-pp-cli messages edit "$MESSAGE_ID" --conversation "$CHAT_ID" --text "$TEXT" --json --no-learn
teams-web-pp-cli messages delete "$MESSAGE_ID" --conversation "$CHAT_ID" --json --no-learn
teams-web-pp-cli drafts create --conversation "$CHAT_ID" --text "$TEXT" --json --no-learn
teams-web-pp-cli drafts list --json --no-learn
```

Edits and deletes are for owned messages. Reply is a quote reply in a confirmed
chat; channel thread routing is not supported. Drafts are local only and are
not sent or synced to Teams. Their changes also require `--execute --confirm`.
Owned OneDrive commands require the configured origin and captured file
audience described in the README.

## V1 limits

Live write checks used the current account's self-chat; other destinations and
policies may behave differently. Complete history, positive mention coverage,
incremental reconciliation of older message changes, broader administration,
and attachment sharing are outside this v1. Keep credentials, tokens, cookies,
private message content, and sync cursors out of diagnostic output.
