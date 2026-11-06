package cli

import "testing"

func TestTeamsChatBootstrapIsAlwaysPartial(t *testing.T) {
	metadata := map[string]any{"isPartialData": false, "hasMoreChats": false}
	if !teamsCollectionIsPartial("chats", metadata) {
		t.Fatal("a bootstrap response cannot prove complete chat discovery")
	}
	if teamsCollectionIsPartial("teams", metadata) {
		t.Fatal("the chat coverage restriction affected teams")
	}
	metadata["isPartialData"] = true
	if !teamsCollectionIsPartial("teams", metadata) {
		t.Fatal("the server partial flag was ignored")
	}
}
