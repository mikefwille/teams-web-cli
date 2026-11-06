// Read the noncredential client-flight context from a private, observed Teams
// people-suggestion capture for the active account and search audience.
package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var teamsPeopleEvidenceName = regexp.MustCompile(`^evidence-teams-[a-z0-9]+-[a-f0-9]{8}-[0-9]+\.json$`)
var teamsPeopleFlightValue = regexp.MustCompile(`^[A-Za-z0-9_.-]+(,[A-Za-z0-9_.-]+)*$`)
var teamsPeopleEvidenceURL = regexp.MustCompile(`https://[^\s]+`)

type teamsPeopleEvidence struct {
	Method       string `json:"method"`
	Detail       string `json:"detail_text"`
	ResponsePath string `json:"full_response_path"`
}

func teamsPeopleTokenAudience(auth string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(auth), " ")
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	jwt := strings.Split(parts[1], ".")
	if len(jwt) != 3 {
		return "", false
	}
	data, err := base64.RawURLEncoding.DecodeString(jwt[1])
	if err != nil {
		return "", false
	}
	var claims struct {
		Audience string `json:"aud"`
	}
	if json.Unmarshal(data, &claims) != nil || claims.Audience == "" {
		return "", false
	}
	return claims.Audience, true
}

func teamsPeopleEvidenceHeaders(detail string) (map[string]string, bool) {
	lines := strings.Split(detail, "\n")
	started, ended, status := false, false, false
	headers := map[string]string{}
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], "Status:") && fields[1] == "200" {
			status = true
		}
		heading := strings.Trim(strings.TrimSpace(line), "#* -:")
		if strings.EqualFold(heading, "Request Headers") {
			started = true
			continue
		}
		if started && (strings.EqualFold(heading, "Request Body") || strings.EqualFold(heading, "Response Headers")) {
			ended = true
			break
		}
		if !started || !strings.HasPrefix(line, "-") {
			continue
		}
		name, value, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "-")), ":")
		name, value = strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(value)
		if !ok || (name != "authorization" && name != "x-anchormailbox" && name != "x-client-flights") {
			continue
		}
		if _, exists := headers[name]; exists || strings.ContainsAny(value, "\r\n") {
			return nil, false
		}
		headers[name] = value
	}
	return headers, status && started && ended
}

func teamsPeopleCapturedContext(dir, name string, primary teamsTokenPrincipal, activeAudience string) (string, bool) {
	var evidence teamsPeopleEvidence
	if !teamsPeopleEvidenceName.MatchString(name) || readPrivateSessionFile(filepath.Join(dir, name), &evidence) != nil || evidence.Method != "POST" {
		return "", false
	}
	match := teamsPeopleEvidenceURL.FindString(evidence.Detail)
	u, err := url.Parse(strings.TrimRight(match, "]"))
	if err != nil || !teamsPeopleURLAllowed(u) {
		return "", false
	}
	headers, ok := teamsPeopleEvidenceHeaders(evidence.Detail)
	if !ok {
		return "", false
	}
	capturedPrincipal, ok := teamsPrincipal(headers["authorization"])
	capturedAudience, audienceOK := teamsPeopleTokenAudience(headers["authorization"])
	anchor, anchorErr := teamsPeopleAnchor(primary)
	flight := headers["x-client-flights"]
	if !ok || !audienceOK || anchorErr != nil || capturedPrincipal.Object != primary.Object || capturedPrincipal.Tenant != primary.Tenant || capturedAudience != activeAudience || !strings.EqualFold(headers["x-anchormailbox"], anchor) || len(flight) > 2048 || !teamsPeopleFlightValue.MatchString(flight) {
		return "", false
	}
	expected := filepath.Join(dir, strings.TrimSuffix(name, ".json")+".network-response")
	if evidence.ResponsePath != expected {
		return "", false
	}
	var response json.RawMessage
	if readPrivateSessionFile(expected, &response) != nil {
		return "", false
	}
	people, err := decodeTeamsPeople(response)
	if err != nil {
		return "", false
	}
	for _, person := range people {
		id, _ := person["ExternalDirectoryObjectId"].(string)
		mri, _ := person["MRI"].(string)
		if strings.EqualFold(id, primary.Object) || strings.EqualFold(mri, "8:orgid:"+primary.Object) {
			return flight, true
		}
	}
	return "", false
}

func loadTeamsPeopleFlight(dir string, primary teamsTokenPrincipal, activeHeaders map[string]string) (string, error) {
	activePrincipal, ok := teamsPrincipal(activeHeaders["authorization"])
	audience, audienceOK := teamsPeopleTokenAudience(activeHeaders["authorization"])
	if !ok || !audienceOK || activePrincipal.Object != primary.Object || activePrincipal.Tenant != primary.Tenant {
		return "", errors.New("Teams people-search audience does not match the current account")
	}
	if expiry, known := accessTokenExpiry(activeHeaders["authorization"]); !known || time.Now().After(expiry) {
		return "", errors.New("Teams people-search audience is expired or has no usable expiry")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", errors.New("Teams people-search capture directory is unavailable")
	}
	checked := 0
	for i := len(entries) - 1; i >= 0 && checked < 512; i-- {
		entry := entries[i]
		if entry.IsDir() || !teamsPeopleEvidenceName.MatchString(entry.Name()) {
			continue
		}
		checked++
		if flight, ok := teamsPeopleCapturedContext(dir, entry.Name(), primary, audience); ok {
			return flight, nil
		}
	}
	return "", errors.New("no matching positive Teams people-suggestion capture; capture a self suggestion with the browser owner")
}
