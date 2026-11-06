package cli

import "testing"

func TestTeamsOwnPresenceRejectsAnotherAccountAndErrors(t *testing.T) {
	data := []byte(`[{"mri":"8:orgid:synthetic","source":"ups","status":20000,"presence":{"availability":"Available"}}]`)
	presence, err := decodeTeamsOwnPresence(data, "8:orgid:synthetic")
	if err != nil || presence["availability"] != "Available" {
		t.Fatalf("presence read: %#v %v", presence, err)
	}
	if _, err := decodeTeamsOwnPresence(data, "8:orgid:other"); err == nil {
		t.Fatal("another account's presence was accepted")
	}
	if _, err := decodeTeamsOwnPresence([]byte(`[{"mri":"8:orgid:synthetic","source":"ups","status":50000}]`), "8:orgid:synthetic"); err == nil {
		t.Fatal("presence error was accepted")
	}
}
