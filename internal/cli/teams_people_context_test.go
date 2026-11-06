package cli

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func teamsPeopleSyntheticToken(t *testing.T, principal teamsTokenPrincipal, audience string, expiry int64) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"tid": principal.Tenant, "oid": principal.Object, "aud": audience, "exp": expiry})
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer aa." + base64.RawURLEncoding.EncodeToString(claims) + ".bb"
}

func TestTeamsPeopleFlightContextRequiresMatchingPositiveCapture(t *testing.T) {
	primary := teamsTokenPrincipal{Object: "11111111-1111-4111-8111-111111111111", Tenant: "22222222-2222-4222-8222-222222222222"}
	other := teamsTokenPrincipal{Object: "33333333-3333-4333-8333-333333333333", Tenant: primary.Tenant}
	future := time.Now().Add(time.Hour).Unix()
	for _, tc := range []struct {
		name, host, audience, anchor, flight, responseID string
		captured                                         teamsTokenPrincipal
		want                                             bool
	}{
		{"matching self", "substrate.office.com", "synthetic-audience", "self", "FlightAlpha,FlightBeta", primary.Object, primary, true},
		{"other account", "substrate.office.com", "synthetic-audience", "self", "FlightAlpha", primary.Object, other, false},
		{"other audience", "substrate.office.com", "other-audience", "self", "FlightAlpha", primary.Object, primary, false},
		{"other anchor", "substrate.office.com", "synthetic-audience", "other", "FlightAlpha", primary.Object, primary, false},
		{"missing self result", "substrate.office.com", "synthetic-audience", "self", "FlightAlpha", other.Object, primary, false},
		{"other origin", "evil.invalid", "synthetic-audience", "self", "FlightAlpha", primary.Object, primary, false},
		{"invalid flight", "substrate.office.com", "synthetic-audience", "self", "bad flight", primary.Object, primary, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			name := "evidence-teams-synthetic-a1b2c3d4-123.json"
			responsePath := filepath.Join(dir, strings.TrimSuffix(name, ".json")+".network-response")
			response, _ := json.Marshal(map[string]any{"Groups": []any{map[string]any{"Type": "People", "Suggestions": []any{map[string]any{"ExternalDirectoryObjectId": tc.responseID, "PeopleType": "Person"}}}}})
			if err := os.WriteFile(responsePath, response, 0600); err != nil {
				t.Fatal(err)
			}
			captured := teamsPeopleSyntheticToken(t, tc.captured, tc.audience, future)
			anchor, _ := teamsPeopleAnchor(primary)
			if tc.anchor == "other" {
				anchor, _ = teamsPeopleAnchor(other)
			}
			q := url.Values{"scenario": {"powerbar"}, "setflight": {teamsPeopleFlights}}
			detail := "reqid=1 POST https://" + tc.host + teamsPeoplePath + "?" + q.Encode() + "\nStatus: 200\nRequest Headers:\n- authorization: " + captured + "\n- x-anchormailbox: " + anchor + "\n- x-client-flights: " + tc.flight + "\nRequest Body:\n{}\nResponse Headers:\n"
			meta, _ := json.Marshal(teamsPeopleEvidence{Method: "POST", Detail: detail, ResponsePath: responsePath})
			if err := os.WriteFile(filepath.Join(dir, name), meta, 0600); err != nil {
				t.Fatal(err)
			}
			active := map[string]string{"authorization": teamsPeopleSyntheticToken(t, primary, "synthetic-audience", future)}
			flight, err := loadTeamsPeopleFlight(dir, primary, active)
			if tc.want && (err != nil || flight != tc.flight) {
				t.Fatalf("matching capture was rejected: %v", err)
			}
			if !tc.want && err == nil {
				t.Fatal("unmatched capture was accepted")
			}
		})
	}
	t.Run("expired active audience", func(t *testing.T) {
		active := map[string]string{"authorization": teamsPeopleSyntheticToken(t, primary, "synthetic-audience", time.Now().Add(-time.Hour).Unix())}
		if _, err := loadTeamsPeopleFlight(t.TempDir(), primary, active); err == nil {
			t.Fatal("expired active audience was accepted")
		}
	})
}

func TestTeamsPeopleEvidenceHeaderParserRejectsDuplicates(t *testing.T) {
	detail := "Status: 200\nRequest Headers:\n- x-client-flights: FlightAlpha\n- x-client-flights: FlightBeta\nRequest Body:\n{}"
	if _, ok := teamsPeopleEvidenceHeaders(detail); ok {
		t.Fatal("duplicate context headers were accepted")
	}
}
