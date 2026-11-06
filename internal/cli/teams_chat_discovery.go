// Bounded chat references from observed Teams bootstrap response shapes.
package cli

import (
	"encoding/json"
	"unicode"
	"unicode/utf8"
)

// IDs are opaque provider values. Do not restrict them to test fixture markers.
func validTeamsOrganizationID(id string) bool {
	if id == "" || len(id) > 4096 || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// collectTeamsChats preserves existing chat rows, and adds only private-feed
// and section-item shapes that were actually observed as a notes conversation.
// Last-message content is deliberately not copied from those extra sources.
func collectTeamsChats(body teamsBootstrap) []map[string]any {
	out := make([]map[string]any, 0, len(body.Chats)+len(body.PrivateFeeds))
	seen := make(map[string]bool)
	add := func(row map[string]any) {
		id := teamsObjectID(row)
		if !validTeamsOrganizationID(id) || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, row)
	}
	for _, row := range body.Chats {
		add(row)
	}
	for _, row := range body.PrivateFeeds {
		if row["type"] != "Conversation" || row["threadType"] != "streamofnotes" || row["streamType"] != "streamofnotes" {
			continue
		}
		id, _ := row["id"].(string)
		add(map[string]any{"id": id, "type": "Conversation", "threadType": "streamofnotes", "streamType": "streamofnotes", "discovered_from": "privateFeeds"})
	}
	// The section envelope may be absent in a partial bootstrap. Do not infer
	// an empty inventory from it, or classify unknown item types as chats.
	var snapshot teamsFolderSnapshot
	if json.Unmarshal(body.ConversationFolders, &snapshot) == nil {
		for _, folder := range snapshot.Folders {
			if folder.IsDeleted == nil || *folder.IsDeleted || folder.Items == nil {
				continue
			}
			for _, item := range *folder.Items {
				if item.ThreadType == "streamofnotes" {
					add(map[string]any{"id": item.ConversationID, "threadType": item.ThreadType, "discovered_from": "conversationFolders"})
				}
			}
		}
	}
	return out
}
