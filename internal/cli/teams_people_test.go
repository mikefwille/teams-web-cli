package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type teamsPeopleRoundTripFunc func(*http.Request) (*http.Response, error)

func (f teamsPeopleRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestTeamsPeoplePayloadUsesBoundedObservedPersonShape(t *testing.T) {
	body, err := teamsPeoplePayload("  Synthetic  Person ")
	if err != nil {
		t.Fatal(err)
	}
	requests, ok := body["EntityRequests"].([]any)
	if !ok || len(requests) != 1 {
		t.Fatalf("expected only one people entity: %#v", body)
	}
	people := requests[0].(map[string]any)
	query := people["Query"].(map[string]string)
	if people["EntityType"] != "People" || people["Size"] != 5 || query["QueryString"] != "Synthetic Person" || query["NormalizedQueryString"] != query["QueryString"] || body["AppName"] != "Microsoft Teams" || body["dataSource"] != "personScoped" {
		t.Fatalf("unexpected people request shape: %#v", body)
	}
	for _, invalid := range []string{"", "   ", "person\nname", "person:other", "person*", strings.Repeat("x", 129)} {
		if _, err := teamsPeoplePayload(invalid); err == nil {
			t.Fatalf("accepted an invalid literal query %q", invalid)
		}
	}
	if _, err := teamsPeoplePayload("synthetic.user@example.test"); err != nil {
		t.Fatalf("address rejected: %v", err)
	}
}

func TestTeamsPeopleDecodeSeparatesErrorsAndOtherGroups(t *testing.T) {
	data := []byte(`{"Groups":[{"Type":"People","Suggestions":[{"DisplayName":"Synthetic Person","MRI":"8:orgid:synthetic","ExternalDirectoryObjectId":"synthetic","PeopleSubtype":"OrganizationUser","TrackingData":"not returned"}]},{"Type":"Text","Suggestions":[{"Text":"ignored"}]}]}`)
	results, err := decodeTeamsPeople(data)
	if err != nil || len(results) != 1 || results[0]["DisplayName"] != "Synthetic Person" || results[0]["MRI"] != "8:orgid:synthetic" {
		t.Fatalf("unexpected suggestions: %#v %v", results, err)
	}
	if _, ok := results[0]["TrackingData"]; ok {
		t.Fatal("returned unrelated service metadata")
	}
	for _, input := range []string{`{}`, `{"Groups":null}`, `{"Groups":[],"error":{"code":"failed"}}`, `{"Groups":[{"Type":"People","Suggestions":null}]}`, `{"Groups":[{"Type":"People","Suggestions":[null]}]}`} {
		if _, err := decodeTeamsPeople([]byte(input)); err == nil {
			t.Fatalf("accepted an invalid response: %s", input)
		}
	}
	if empty, err := decodeTeamsPeople([]byte(`{"Groups":[]}`)); err != nil || len(empty) != 0 {
		t.Fatalf("empty suggestions: %#v %v", empty, err)
	}
}

func TestTeamsPeopleTransportGuardsAudienceAndRequest(t *testing.T) {
	calls := 0
	anchor, err := teamsPeopleAnchor(teamsTokenPrincipal{Object: "11111111-1111-4111-8111-111111111111", Tenant: "22222222-2222-4222-8222-222222222222"})
	if err != nil {
		t.Fatal(err)
	}
	transport := teamsPeopleTransport{anchor: anchor, flight: "SyntheticFlightA,SyntheticFlightB", headers: map[string]string{"authorization": "Bearer synthetic", "x-safe": "ok", "x-invalid": "bad\r\nvalue", "x-anchormailbox": "wrong-captured-anchor", "x-client-flights": "wrong-flight"}, next: teamsPeopleRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("authorization") != "Bearer synthetic" || req.Header.Get("x-safe") != "ok" || req.Header.Get("x-invalid") != "" || req.Header.Get("Content-Type") != "application/json" || req.Header.Get("X-AnchorMailbox") != anchor || req.Header.Get("X-Client-Flights") != "SyntheticFlightA,SyntheticFlightB" {
			t.Fatal("credential or safe-header handling was incorrect")
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || len(body["EntityRequests"].([]any)) != 1 {
			t.Fatalf("unexpected request body: %v", err)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"Groups":[]}`)), Request: req}, nil
	})}
	body, err := teamsPeoplePayload("Synthetic Person")
	if err != nil {
		t.Fatal(err)
	}
	data, err := runTeamsPeopleSuggestions(context.Background(), &http.Client{Transport: transport}, body)
	if err != nil || string(data) != `{"Groups":[]}` || calls != 1 {
		t.Fatalf("request failed: calls=%d err=%v", calls, err)
	}
	for _, raw := range []string{
		"https://evil.invalid" + teamsPeoplePath + "?scenario=powerbar&setflight=" + teamsPeopleFlights,
		teamsSearchOrigin + "/unexpected?scenario=powerbar&setflight=" + teamsPeopleFlights,
		teamsSearchOrigin + teamsPeoplePath + "?scenario=powerbar&setflight=" + teamsPeopleFlights + "&other=1",
		teamsSearchOrigin + teamsPeoplePath + "?scenario=powerbar&scenario=powerbar&setflight=" + teamsPeopleFlights,
	} {
		req, err := http.NewRequest(http.MethodPost, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transport.RoundTrip(req); err == nil {
			t.Fatal("accepted an unobserved credential destination")
		}
	}
	if calls != 1 {
		t.Fatal("invalid requests reached the downstream transport")
	}
}

func TestTeamsPeopleAnchorRequiresAccountUUIDs(t *testing.T) {
	for _, principal := range []teamsTokenPrincipal{
		{Object: "", Tenant: "22222222-2222-4222-8222-222222222222"},
		{Object: "11111111-1111-4111-8111-111111111111\r\nX: bad", Tenant: "22222222-2222-4222-8222-222222222222"},
		{Object: "11111111-1111-4111-8111-111111111111", Tenant: "not-a-uuid"},
	} {
		if _, err := teamsPeopleAnchor(principal); err == nil {
			t.Fatal("accepted an invalid account anchor")
		}
	}
}

func TestTeamsPeopleCommandIsReadOnlyAndDryRunNeedsNoSession(t *testing.T) {
	t.Setenv("TEAMS_WEB_HOME", t.TempDir())
	t.Setenv("TEAMS_WEB_SESSION_DIR", t.TempDir())
	root := newRootCmd(&rootFlags{})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"people", "search", "--query", "Synthetic Person", "--dry-run", "--json", "--no-learn"})
	if err := root.Execute(); err != nil {
		t.Fatalf("dry-run failed: %v %s", err, out.String())
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result["dry_run"] != true || result["source"] != "teams_native_typeahead" || result["partial"] != true {
		t.Fatalf("unexpected dry-run output: %s %v", out.String(), err)
	}
	cmd := newTeamsPeopleSearchCmd(&rootFlags{})
	if cmd.Annotations["mcp:read-only"] != "true" || cmd.Annotations["pp:data-source"] != "live" {
		t.Fatal("people search is not marked as a live, read-only command")
	}
}
