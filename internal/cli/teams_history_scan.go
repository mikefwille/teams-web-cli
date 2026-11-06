package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
)

// An explicit plan is a sequence of known request selectors, not an inferred
// continuation protocol. The first request is supplied by the regular flags.
type teamsHistoryPoint struct {
	StartTime    int64  `json:"start_time"`
	SyncState    string `json:"sync_state"`
	HasStartTime bool   `json:"-"`
	View         string `json:"-"`
}

type teamsHistoryPlan struct {
	Pages []teamsHistoryPoint `json:"pages"`
}

type teamsHistoryScan struct {
	Messages          []map[string]any
	Metadata          []map[string]any
	FirstPageIDs      map[string]bool
	PagesScanned      int
	OverlapCount      int
	ConflictingCopies int
	Unidentified      int
	StopReason        string
}

type teamsHistoryRequestError struct{ cause error }

func (e teamsHistoryRequestError) Error() string {
	return "Teams message history request failed; the response or cursor may be invalid"
}
func (e teamsHistoryRequestError) Unwrap() error { return e.cause }

// Validate the server-provided link, then reconstruct the request on the
// already guarded Teams origin instead of following the URL directly.
func teamsBackwardPoint(link, conversation string, pageSize int) (teamsHistoryPoint, error) {
	invalid := func() (teamsHistoryPoint, error) {
		return teamsHistoryPoint{}, errors.New("invalid or untrusted Teams backward continuation")
	}
	if len(link) > 70000 || !validTeamsMessagePathID(conversation) || pageSize < 1 || pageSize > 200 {
		return invalid()
	}
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "teams.cloud.microsoft") || u.User != nil || u.Opaque != "" || u.Fragment != "" || u.ForceQuery {
		return invalid()
	}
	prefix := "/api/chatsvc/amer/v1/users/"
	if !strings.HasPrefix(u.Path, prefix) {
		return invalid()
	}
	rest := strings.TrimPrefix(u.Path, prefix)
	segments := strings.Split(rest, "/")
	if len(segments) != 4 || !strings.EqualFold(segments[0], "me") || segments[1] != "conversations" || segments[2] != conversation || segments[3] != "messages" {
		return invalid()
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(query) != 4 {
		return invalid()
	}
	for _, key := range []string{"startTime", "syncState", "pageSize", "view"} {
		if len(query[key]) != 1 {
			return invalid()
		}
	}
	start, err := strconv.ParseInt(query.Get("startTime"), 10, 64)
	if err != nil || start < 0 {
		return invalid()
	}
	requestedSize, err := strconv.Atoi(query.Get("pageSize"))
	if err != nil || requestedSize != pageSize {
		return invalid()
	}
	state, view := query.Get("syncState"), query.Get("view")
	if state == "" || len(state) > 65536 || (view != "msnp24Equivalent" && view != "msnp24Equivalent|supportsMessageProperties") {
		return invalid()
	}
	return teamsHistoryPoint{StartTime: start, SyncState: state, HasStartTime: true, View: view}, nil
}

func readTeamsHistoryPlan(file string, first teamsHistoryPoint, pageSize int) ([]teamsHistoryPoint, error) {
	if _, err := teamsHistoryParams(first.SyncState, first.StartTime, pageSize); err != nil {
		return nil, err
	}
	points := []teamsHistoryPoint{first}
	if file == "" {
		return points, nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("reading page plan: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 600001))
	if err != nil || len(data) > 600000 {
		return nil, errors.New("page plan could not be read within the size limit")
	}
	var plan teamsHistoryPlan
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil || plan.Pages == nil {
		return nil, usageErr(errors.New("page plan must be an object with a pages array"))
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, usageErr(errors.New("page plan must contain only one JSON object"))
	}
	if len(plan.Pages) > 9 {
		return nil, usageErr(errors.New("page plan may contain at most nine additional pages"))
	}
	seen := map[teamsHistoryPoint]bool{first: true}
	for _, point := range plan.Pages {
		if point.StartTime == 0 && point.SyncState == "" {
			return nil, usageErr(errors.New("each additional page needs start_time or sync_state"))
		}
		if _, err := teamsHistoryParams(point.SyncState, point.StartTime, pageSize); err != nil {
			return nil, err
		}
		if seen[point] {
			return nil, usageErr(errors.New("page plan contains a repeated request selector"))
		}
		seen[point] = true
		points = append(points, point)
	}
	return points, nil
}

func teamsMessageDeleted(message map[string]any) bool {
	properties, _ := message["properties"].(map[string]any)
	for key, value := range properties {
		if !strings.EqualFold(key, "deletetime") {
			continue
		}
		switch v := value.(type) {
		case nil:
			return false
		case string:
			return v != "" && v != "0"
		case float64:
			return v != 0
		default:
			return true
		}
	}
	return false
}

func scanTeamsHistory(points []teamsHistoryPoint, fetch func(teamsHistoryPoint) (teamsMessagePage, error)) (teamsHistoryScan, error) {
	out := teamsHistoryScan{Messages: []map[string]any{}, Metadata: []map[string]any{}, FirstPageIDs: map[string]bool{}, StopReason: "explicit_selection"}
	positions := map[string]int{}
	rowsRead := 0
	for i, point := range points {
		page, err := fetch(point)
		if err != nil {
			return teamsHistoryScan{}, fmt.Errorf("history page %d failed; the scan is incomplete: %w", i+1, err)
		}
		rowsRead += len(page.Messages)
		if rowsRead > 2000 {
			return teamsHistoryScan{}, errors.New("history scan exceeded the 2000-message limit")
		}
		out.PagesScanned++
		out.Metadata = append(out.Metadata, page.Metadata)
		for _, message := range page.Messages {
			id, ok := message["id"].(string)
			if !ok || !validTeamsMessagePathID(id) {
				out.Unidentified++
				out.Messages = append(out.Messages, message)
				continue
			}
			if i == 0 {
				out.FirstPageIDs[id] = true
			}
			index, found := positions[id]
			if !found {
				positions[id] = len(out.Messages)
				out.Messages = append(out.Messages, message)
				continue
			}
			out.OverlapCount++
			old, _ := json.Marshal(out.Messages[index])
			newer, _ := json.Marshal(message)
			if !bytes.Equal(old, newer) {
				out.ConflictingCopies++
			}
			// Do not resurrect content if one observed copy is a tombstone.
			if !teamsMessageDeleted(out.Messages[index]) && teamsMessageDeleted(message) {
				out.Messages[index] = message
			}
		}
	}
	return out, nil
}

func followTeamsHistoryBackward(first teamsHistoryPoint, conversation string, pageSize, maxPages int, fetch func(teamsHistoryPoint) (teamsMessagePage, error)) (teamsHistoryScan, error) {
	points := []teamsHistoryPoint{first}
	pages := make([]teamsMessagePage, 0, maxPages)
	seenPoints := map[teamsHistoryPoint]bool{first: true}
	seenIDs := map[string]bool{}
	rowsRead := 0
	stop := "page_limit"
	for i := 0; i < maxPages; i++ {
		page, err := fetch(points[len(points)-1])
		if err != nil {
			return teamsHistoryScan{}, fmt.Errorf("history page %d failed; the scan is incomplete: %w", i+1, err)
		}
		rowsRead += len(page.Messages)
		if rowsRead > 2000 {
			return teamsHistoryScan{}, errors.New("history scan exceeded the 2000-message limit")
		}
		newIDs := 0
		for _, message := range page.Messages {
			id, ok := message["id"].(string)
			if ok && validTeamsMessagePathID(id) && !seenIDs[id] {
				seenIDs[id] = true
				newIDs++
			}
		}
		if i > 0 && newIDs == 0 {
			return teamsHistoryScan{}, errors.New("history continuation returned no distinct messages; the scan is incomplete")
		}
		pages = append(pages, page)
		raw, exists := page.Metadata["backwardLink"]
		if !exists || raw == nil || raw == "" {
			stop = "no_backward_link"
			break
		}
		if i+1 == maxPages {
			break
		}
		link, ok := raw.(string)
		if !ok {
			return teamsHistoryScan{}, errors.New("invalid Teams backward continuation metadata")
		}
		next, err := teamsBackwardPoint(link, conversation, pageSize)
		if err != nil {
			return teamsHistoryScan{}, err
		}
		if seenPoints[next] {
			return teamsHistoryScan{}, errors.New("history continuation repeated a request; the scan is incomplete")
		}
		seenPoints[next] = true
		points = append(points, next)
	}
	index := 0
	out, err := scanTeamsHistory(points, func(teamsHistoryPoint) (teamsMessagePage, error) {
		page := pages[index]
		index++
		return page, nil
	})
	if err == nil {
		out.StopReason = stop
	}
	return out, err
}

func fetchTeamsHistoryScan(cmd *cobra.Command, flags *rootFlags, conversation string, points []teamsHistoryPoint, pageSize, maxPages int) (teamsHistoryScan, error) {
	if !validTeamsMessagePathID(conversation) {
		return teamsHistoryScan{}, usageErr(errors.New("--conversation must be a valid opaque conversation ID"))
	}
	c, err := flags.newClient()
	if err != nil {
		return teamsHistoryScan{}, err
	}
	return fetchTeamsHistoryScanWithClient(cmd, flags, c, conversation, points, pageSize, maxPages)
}

func fetchTeamsHistoryScanWithClient(cmd *cobra.Command, flags *rootFlags, c *client.Client, conversation string, points []teamsHistoryPoint, pageSize, maxPages int) (teamsHistoryScan, error) {
	if !validTeamsMessagePathID(conversation) {
		return teamsHistoryScan{}, usageErr(errors.New("--conversation must be a valid opaque conversation ID"))
	}
	if len(points) == 0 || maxPages < 1 || maxPages > 10 || (len(points) > 1 && maxPages != 1) {
		return teamsHistoryScan{}, usageErr(errors.New("invalid history page selection"))
	}
	ctx, cancel := boundCtx(cmd.Context(), flags)
	defer cancel()
	bounded := &cobra.Command{}
	bounded.SetContext(ctx)
	fetch := func(point teamsHistoryPoint) (teamsMessagePage, error) {
		return fetchTeamsMessagePoint(bounded, flags, c, conversation, point, pageSize)
	}
	if cliutil.IsDogfoodEnv() && (maxPages > 1 || len(points) > 1) {
		result, err := scanTeamsHistory(points[:1], fetch)
		result.StopReason = "dogfood_page_cap"
		return result, err
	}
	if len(points) > 1 {
		return scanTeamsHistory(points, fetch)
	}
	return followTeamsHistoryBackward(points[0], conversation, pageSize, maxPages, fetch)
}
