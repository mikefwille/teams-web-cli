// Own presence changes based on the observed Teams web-session requests.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/mikefwille/teams-web-cli/internal/client"
	"github.com/mikefwille/teams-web-cli/internal/cliutil"
)

const teamsForceAvailabilityPath = "/ups/noam/v1/me/forceavailability"
const teamsPublishNotePath = "/ups/noam/v1/me/publishnote"

var teamsPresenceWriteLimiter = cliutil.NewAdaptiveLimiterAuto(1)

func teamsStatusMessagePayload(text string, now time.Time) (map[string]any, time.Time, error) {
	if !utf8.ValidString(text) || strings.TrimSpace(text) == "" || len(text) > 1024 || strings.ContainsAny(text, "\r\n\x00") {
		return nil, time.Time{}, errors.New("status text must be a nonempty single line of at most 1024 bytes")
	}
	expires := now.UTC().Add(time.Hour).Truncate(time.Millisecond)
	return map[string]any{"expiry": expires.Format("2006-01-02T15:04:05.000Z"), "message": "<p>" + html.EscapeString(text) + "</p>"}, expires, nil
}

// This sends one exact observed PUT. An ambiguous result is never retried.
func performTeamsPresenceWrite(ctx context.Context, c *client.Client, path string, body any) error {
	if c == nil || c.HTTPClient == nil || c.Config == nil || c.Config.AuthSource != "web-session" || !teamsOrigin(c.BaseURL) {
		return authErr(errors.New("a verified-origin Teams web session is required"))
	}
	if _, ok := c.HTTPClient.Transport.(teamsOriginTransport); !ok {
		return errors.New("Teams session origin guard is unavailable")
	}
	if path != teamsForceAvailabilityPath && path != teamsPublishNotePath {
		return errors.New("unsupported presence write route")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://teams.cloud.microsoft")
	if err := teamsPresenceWriteLimiter.Wait(ctx); err != nil {
		return err
	}
	hc := *c.HTTPClient
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return apiErr(errors.New("outcome_unknown: presence request did not complete; inspect your status before trying again"))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(data) > 65536 {
		return apiErr(errors.New("outcome_unknown: presence response could not be read; inspect your status before trying again"))
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		teamsPresenceWriteLimiter.OnRateLimit()
		return rateLimitErr(&cliutil.RateLimitError{URL: "Teams presence service", RetryAfter: cliutil.RetryAfter(resp)})
	}
	if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500 {
		return apiErr(fmt.Errorf("outcome_unknown: Teams returned HTTP %d; inspect your status before trying again", resp.StatusCode))
	}
	if resp.StatusCode != http.StatusOK {
		return classifyAPIErrorOnly(fmt.Errorf("Teams presence request returned HTTP %d", resp.StatusCode))
	}
	if len(bytes.TrimSpace(data)) != 0 {
		return apiErr(errors.New("outcome_unknown: Teams returned an unexpected presence response; inspect your status before trying again"))
	}
	teamsPresenceWriteLimiter.OnSuccess()
	return nil
}

func configureTeamsPresenceChange(cmd *cobra.Command, flags *rootFlags, kind string) {
	var availability, text string
	var execute, confirm bool
	cmd.Args = cobra.NoArgs
	cmd.Annotations = map[string]string{"mcp:read-only-unless-flags": "execute,confirm"}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		path := teamsForceAvailabilityPath
		var body any
		preview := map[string]any{"executed": false, "action": kind}
		var expires time.Time
		switch kind {
		case "set":
			if !strings.EqualFold(availability, "busy") {
				return usageErr(errors.New("the observed availability value is Busy"))
			}
			body = map[string]string{"availability": "Busy"}
			preview["availability"] = "Busy"
		case "reset":
		case "set-message":
			path = teamsPublishNotePath
			var err error
			body, expires, err = teamsStatusMessagePayload(text, time.Now())
			if err != nil {
				return usageErr(err)
			}
			preview["text"] = text
			preview["expires_in"] = "1h"
		case "clear-message":
			path = teamsPublishNotePath
			body = map[string]string{"message": ""}
		default:
			return errors.New("unsupported presence action")
		}
		if flags.dryRun || !execute {
			return flags.printJSON(cmd, preview)
		}
		if !confirm {
			return usageErr(errors.New("--execute requires --confirm"))
		}
		if cliutil.IsAnyHarness() {
			return errors.New("presence writes are disabled in verification and dogfood harnesses")
		}
		c, err := flags.newClient()
		if err != nil {
			return err
		}
		ctx, cancel := boundCtx(cmd.Context(), flags)
		defer cancel()
		if err := performTeamsPresenceWrite(ctx, c, path, body); err != nil {
			return err
		}
		result := map[string]any{"executed": true, "status": "accepted", "action": kind, "verification": "not_checked"}
		if !expires.IsZero() {
			result["expires_at"] = expires.Format(time.RFC3339Nano)
		}
		return flags.printJSON(cmd, result)
	}
	if kind == "set" {
		cmd.Short = "Preview or set your availability to Busy"
		cmd.Flags().StringVar(&availability, "availability", "", "Observed availability value: Busy")
	}
	if kind == "reset" {
		cmd.Short = "Preview or reset your availability"
	}
	if kind == "set-message" {
		cmd.Short = "Preview or set a one-hour status message"
		cmd.Flags().StringVar(&text, "text", "", "Single-line status message")
	}
	if kind == "clear-message" {
		cmd.Short = "Preview or clear your status message"
	}
	cmd.Flags().BoolVar(&execute, "execute", false, "Execute the previewed change")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm the change")
}

func newTeamsPresenceSetCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "set"}
	configureTeamsPresenceChange(cmd, flags, "set")
	return cmd
}
func newTeamsPresenceResetCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "reset"}
	configureTeamsPresenceChange(cmd, flags, "reset")
	return cmd
}
func newTeamsPresenceMessageSetCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "set"}
	configureTeamsPresenceChange(cmd, flags, "set-message")
	return cmd
}
func newTeamsPresenceMessageClearCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "clear"}
	configureTeamsPresenceChange(cmd, flags, "clear-message")
	return cmd
}
func newTeamsPresenceMessageCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "message", Short: "Manage your status message", Annotations: map[string]string{"pp:parent-group": "true"}, RunE: parentNoSubcommandRunE(flags)}
	cmd.AddCommand(newTeamsPresenceMessageSetCmd(flags))
	cmd.AddCommand(newTeamsPresenceMessageClearCmd(flags))
	return cmd
}
