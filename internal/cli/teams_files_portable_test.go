package cli

import "testing"

// All file tests use a synthetic tenant origin; no real tenant is embedded.
func init() { teamsFilesOrigin = "https://fixture-my.sharepoint.com" }

func TestConfiguredTeamsFilesOrigin(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"https://example-my.sharepoint.com", "https://example-my.sharepoint.com"},
		{"https://EXAMPLE-my.sharepoint.com:443/", "https://example-my.sharepoint.com"},
		{"", ""}, {"http://example-my.sharepoint.com", ""},
		{"https://example-my.sharepoint.com.evil.invalid", ""},
		{"https://example-my.sharepoint.com/path", ""},
		{"https://user@example-my.sharepoint.com", ""},
	} {
		if got := configuredTeamsFilesOrigin(tc.input); got != tc.want {
			t.Fatalf("configured origin validation returned %q, want %q", got, tc.want)
		}
	}
}
