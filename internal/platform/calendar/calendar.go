// Package calendar is a generic Google Calendar API client shared by the app's
// modules. It knows nothing about sync placeholders or any single feature — those
// live in the module that owns them. The API base URLs are fields on Client so tests
// can point it at a local fake.
package calendar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Default Google Calendar API endpoints.
const (
	DefaultBaseURL  = "https://www.googleapis.com/calendar/v3"
	DefaultBatchURL = "https://www.googleapis.com/batch/calendar/v3"
)

// Client talks to the Google Calendar API. BaseURL/BatchURL are configurable so a
// test can substitute a local fake; HTTP defaults to http.DefaultClient.
type Client struct {
	BaseURL  string
	BatchURL string
	HTTP     *http.Client
}

// New returns a Client. An empty baseURL uses the real Google endpoints.
func New(baseURL string) *Client {
	if baseURL == "" {
		return &Client{BaseURL: DefaultBaseURL, BatchURL: DefaultBatchURL, HTTP: http.DefaultClient}
	}
	// A custom base (e.g. a test fake) also redirects batch requests to it, so the
	// hook doesn't fake reads while batch deletes still hit real Google.
	return &Client{BaseURL: baseURL, BatchURL: baseURL + "/batch", HTTP: http.DefaultClient}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// --- Types ---

// Calendar represents a Google Calendar from the CalendarList API.
type Calendar struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Primary    bool   `json:"primary"`
	AccessRole string `json:"accessRole"`
}

// GCalEvent represents a Google Calendar event with the fields we read and write.
type GCalEvent struct {
	ID                 string              `json:"id,omitempty"`
	Summary            string              `json:"summary,omitempty"`
	Description        string              `json:"description,omitempty"`
	Location           string              `json:"location,omitempty"`
	Start              EventTime           `json:"start"`
	End                EventTime           `json:"end"`
	Status             string              `json:"status,omitempty"`
	Transparency       string              `json:"transparency,omitempty"` // "opaque" (default/empty) or "transparent" (free)
	ConferenceData     json.RawMessage     `json:"conferenceData,omitempty"`
	Attachments        json.RawMessage     `json:"attachments,omitempty"`
	Attendees          []Attendee          `json:"attendees,omitempty"`
	Reminders          *Reminders          `json:"reminders,omitempty"`
	ExtendedProperties *ExtendedProperties `json:"extendedProperties,omitempty"`
	ColorID            string              `json:"colorId,omitempty"`
	Updated            string              `json:"updated,omitempty"`
	RecurringEventId   string              `json:"recurringEventId,omitempty"`
	EventType          string              `json:"eventType,omitempty"` // default, workingLocation, outOfOffice, focusTime
}

// EventTime represents a Google Calendar event time (either dateTime or date for all-day).
type EventTime struct {
	DateTime string `json:"dateTime,omitempty"`
	Date     string `json:"date,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

// Attendee represents a Google Calendar event attendee.
type Attendee struct {
	Email          string `json:"email"`
	DisplayName    string `json:"displayName,omitempty"`
	Self           bool   `json:"self,omitempty"`
	ResponseStatus string `json:"responseStatus,omitempty"`
	Organizer      bool   `json:"organizer,omitempty"`
}

// Reminders controls event reminder settings.
type Reminders struct {
	UseDefault bool `json:"useDefault"`
}

// ExtendedProperties holds private and shared key-value pairs on an event.
type ExtendedProperties struct {
	Private map[string]string `json:"private,omitempty"`
}

type eventsListResponse struct {
	Items         []GCalEvent `json:"items"`
	NextPageToken string      `json:"nextPageToken"`
	NextSyncToken string      `json:"nextSyncToken"`
}

type calendarListResponse struct {
	Items []struct {
		ID              string `json:"id"`
		Summary         string `json:"summary"`
		SummaryOverride string `json:"summaryOverride"`
		Primary         bool   `json:"primary"`
		AccessRole      string `json:"accessRole"`
	} `json:"items"`
	NextPageToken string `json:"nextPageToken"`
}

// ListEventsResult holds events and the sync token from a list call.
type ListEventsResult struct {
	Events    []GCalEvent
	SyncToken string
}

// ErrSyncTokenExpired is returned when a sync token is no longer valid (410 Gone).
var ErrSyncTokenExpired = fmt.Errorf("sync token expired (410 Gone)")

// EventQuery filters events.list. Private-property filters combine (AND); zero
// TimeMin/TimeMax are omitted. It subsumes the various placeholder queries.
type EventQuery struct {
	PrivateProps map[string]string // privateExtendedProperty key=value filters
	TimeMin      time.Time
	TimeMax      time.Time
	SingleEvents bool
}

// --- Calendar list ---

// ListCalendars fetches the user's calendar list.
func (c *Client) ListCalendars(ctx context.Context, token string) ([]Calendar, error) {
	var all []Calendar
	pageToken := ""
	for {
		u := c.BaseURL + "/users/me/calendarList?maxResults=100"
		if pageToken != "" {
			u += "&pageToken=" + pageToken
		}
		body, err := c.doChecked(ctx, token, "GET", u, nil)
		if err != nil {
			return nil, fmt.Errorf("listing calendars: %w", err)
		}
		var list calendarListResponse
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("decoding calendar list: %w", err)
		}
		for _, item := range list.Items {
			name := item.SummaryOverride
			if name == "" {
				name = item.Summary
			}
			all = append(all, Calendar{ID: item.ID, Name: name, Primary: item.Primary, AccessRole: item.AccessRole})
		}
		if list.NextPageToken == "" {
			break
		}
		pageToken = list.NextPageToken
	}
	return all, nil
}

// --- Event operations ---

// ListEvents fetches events from a calendar within the given time window, plus a
// sync token for future incremental fetches.
func (c *Client) ListEvents(ctx context.Context, token, calendarID string, timeMin, timeMax time.Time) (*ListEventsResult, error) {
	return c.ListEventsFields(ctx, token, calendarID, timeMin, timeMax, "")
}

// ListEventsFields is ListEvents with an optional Google partial-response field mask
// (e.g. "items(summary,start,end),nextPageToken") to shrink the payload. A mask must
// include nextPageToken for paging; omitting nextSyncToken yields an empty SyncToken —
// fine for a one-shot read, but sync's incremental fetch must never use such a mask.
func (c *Client) ListEventsFields(ctx context.Context, token, calendarID string, timeMin, timeMax time.Time, fields string) (*ListEventsResult, error) {
	var result ListEventsResult
	pageToken := ""
	for {
		params := url.Values{}
		params.Set("singleEvents", "true")
		params.Set("orderBy", "startTime")
		params.Set("maxResults", "2500")
		params.Set("timeMin", timeMin.Format(time.RFC3339))
		params.Set("timeMax", timeMax.Format(time.RFC3339))
		if fields != "" {
			params.Set("fields", fields)
		}
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}
		u := c.BaseURL + "/calendars/" + url.PathEscape(calendarID) + "/events?" + params.Encode()
		body, err := c.doChecked(ctx, token, "GET", u, nil)
		if err != nil {
			return nil, fmt.Errorf("listing events: %w", err)
		}
		var resp eventsListResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("decoding events: %w", err)
		}
		result.Events = append(result.Events, resp.Items...)
		if resp.NextPageToken == "" {
			result.SyncToken = resp.NextSyncToken
			break
		}
		pageToken = resp.NextPageToken
	}
	return &result, nil
}

// ListEventsIncremental fetches only events changed since the given sync token.
// Returns ErrSyncTokenExpired if the token is no longer valid.
func (c *Client) ListEventsIncremental(ctx context.Context, token, calendarID, syncToken string) (*ListEventsResult, error) {
	var result ListEventsResult
	pageToken := ""
	for {
		params := url.Values{}
		params.Set("syncToken", syncToken)
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}
		u := c.BaseURL + "/calendars/" + url.PathEscape(calendarID) + "/events?" + params.Encode()
		body, statusCode, err := c.do(ctx, token, "GET", u, nil)
		if statusCode == http.StatusGone {
			return nil, ErrSyncTokenExpired
		}
		if err != nil {
			return nil, err
		}
		if statusCode >= 400 {
			return nil, fmt.Errorf("API status %d: %s", statusCode, string(body))
		}
		var resp eventsListResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("decoding incremental events: %w", err)
		}
		result.Events = append(result.Events, resp.Items...)
		if resp.NextPageToken == "" {
			result.SyncToken = resp.NextSyncToken
			break
		}
		pageToken = resp.NextPageToken
	}
	return &result, nil
}

// ListEventsByProperty fetches events matching a query (private-property filters,
// optional time window). Pages through all results.
func (c *Client) ListEventsByProperty(ctx context.Context, token, calendarID string, q EventQuery) ([]GCalEvent, error) {
	var all []GCalEvent
	pageToken := ""
	for {
		params := url.Values{}
		for k, v := range q.PrivateProps {
			params.Add("privateExtendedProperty", k+"="+v)
		}
		if q.SingleEvents {
			params.Set("singleEvents", "true")
		}
		if !q.TimeMin.IsZero() {
			params.Set("timeMin", q.TimeMin.Format(time.RFC3339))
		}
		if !q.TimeMax.IsZero() {
			params.Set("timeMax", q.TimeMax.Format(time.RFC3339))
		}
		params.Set("maxResults", "2500")
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}
		u := c.BaseURL + "/calendars/" + url.PathEscape(calendarID) + "/events?" + params.Encode()
		body, err := c.doChecked(ctx, token, "GET", u, nil)
		if err != nil {
			return nil, fmt.Errorf("listing events by property: %w", err)
		}
		var resp eventsListResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("decoding events: %w", err)
		}
		all = append(all, resp.Items...)
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return all, nil
}

// CreateEvent creates an event on the specified calendar.
func (c *Client) CreateEvent(ctx context.Context, token, calendarID string, event *GCalEvent) (*GCalEvent, error) {
	u := c.BaseURL + "/calendars/" + url.PathEscape(calendarID) +
		"/events?conferenceDataVersion=1&supportsAttachments=true&sendUpdates=none"
	jsonBody, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	body, err := c.doChecked(ctx, token, "POST", u, jsonBody)
	if err != nil {
		return nil, fmt.Errorf("creating event: %w", err)
	}
	var created GCalEvent
	if err := json.Unmarshal(body, &created); err != nil {
		return nil, fmt.Errorf("decoding created event: %w", err)
	}
	return &created, nil
}

// UpdateEvent patches an event on the specified calendar.
func (c *Client) UpdateEvent(ctx context.Context, token, calendarID, eventID string, event *GCalEvent) (*GCalEvent, error) {
	u := c.BaseURL + "/calendars/" + url.PathEscape(calendarID) +
		"/events/" + url.PathEscape(eventID) +
		"?conferenceDataVersion=1&supportsAttachments=true&sendUpdates=none"
	jsonBody, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	body, err := c.doChecked(ctx, token, "PATCH", u, jsonBody)
	if err != nil {
		return nil, fmt.Errorf("updating event: %w", err)
	}
	var updated GCalEvent
	if err := json.Unmarshal(body, &updated); err != nil {
		return nil, fmt.Errorf("decoding updated event: %w", err)
	}
	return &updated, nil
}

// DeleteEvent deletes an event from the specified calendar. An already-deleted
// event (404/410) is treated as success — deletion is idempotent.
func (c *Client) DeleteEvent(ctx context.Context, token, calendarID, eventID string) error {
	u := c.BaseURL + "/calendars/" + url.PathEscape(calendarID) +
		"/events/" + url.PathEscape(eventID) + "?sendUpdates=none"
	body, status, err := c.do(ctx, token, "DELETE", u, nil)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent || status == http.StatusOK ||
		status == http.StatusGone || status == http.StatusNotFound {
		return nil
	}
	if status >= 400 {
		return fmt.Errorf("API status %d: %s", status, string(body))
	}
	return nil
}

// BatchDeleteEvents deletes multiple events using Google's batch API (≤50 per
// request). Returns counts of deleted and errored.
func (c *Client) BatchDeleteEvents(ctx context.Context, token, calendarID string, eventIDs []string) (deleted, errors int) {
	const batchSize = 50
	for i := 0; i < len(eventIDs); i += batchSize {
		end := i + batchSize
		if end > len(eventIDs) {
			end = len(eventIDs)
		}
		d, e := c.doBatchDelete(ctx, token, calendarID, eventIDs[i:end])
		deleted += d
		errors += e
	}
	return
}

func (c *Client) doBatchDelete(ctx context.Context, token, calendarID string, eventIDs []string) (deleted, errors int) {
	// Boundary must be an RFC 2046 token (no tspecials like '@', ≤70 chars), so it
	// is derived from a timestamp, never from the calendar ID.
	boundary := "batch_calsync_" + fmt.Sprintf("%d", time.Now().UnixNano())
	var body bytes.Buffer
	for i, eventID := range eventIDs {
		body.WriteString("--" + boundary + "\r\n")
		body.WriteString("Content-Type: application/http\r\n")
		body.WriteString(fmt.Sprintf("Content-ID: <item%d>\r\n", i))
		body.WriteString("\r\n")
		path := "/calendar/v3/calendars/" + url.PathEscape(calendarID) +
			"/events/" + url.PathEscape(eventID) + "?sendUpdates=none"
		body.WriteString("DELETE " + path + " HTTP/1.1\r\n")
		body.WriteString("\r\n")
	}
	body.WriteString("--" + boundary + "--\r\n")

	req, err := http.NewRequestWithContext(ctx, "POST", c.BatchURL, &body)
	if err != nil {
		return 0, len(eventIDs)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "multipart/mixed; boundary="+boundary)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, len(eventIDs)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, len(eventIDs)
	}
	// Count per-part HTTP status lines in the multipart response.
	for _, line := range strings.Split(string(respBody), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "HTTP/1.1 2") {
			deleted++
		} else if strings.HasPrefix(line, "HTTP/1.1 4") || strings.HasPrefix(line, "HTTP/1.1 5") {
			errors++
		}
	}
	return
}

// --- HTTP with classified retry ---

// doChecked makes a request and returns an error for any status >= 400.
func (c *Client) doChecked(ctx context.Context, token, method, u string, body []byte) ([]byte, error) {
	respBody, status, err := c.do(ctx, token, method, u, body)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("API status %d: %s", status, string(respBody))
	}
	return respBody, nil
}

// do makes a request with backoff on retryable errors, returning body and status.
// Retries only transient failures (429, 5xx, and rate/quota-reason 403s); other
// 4xx fail fast so a permanent 403 (e.g. read-only calendar) doesn't hang.
func (c *Client) do(ctx context.Context, token, method, rawURL string, reqBody []byte) ([]byte, int, error) {
	const maxRetries = 5
	baseDelay := 500 * time.Millisecond
	maxDelay := 30 * time.Second

	for attempt := range maxRetries {
		var bodyReader io.Reader
		if reqBody != nil {
			bodyReader = bytes.NewReader(reqBody)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if reqBody != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient().Do(req)
		if err != nil {
			return nil, 0, fmt.Errorf("request failed: %w", err)
		}
		respBody, err := io.ReadAll(resp.Body)
		retryAfter := resp.Header.Get("Retry-After")
		resp.Body.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("reading response: %w", err)
		}

		if attempt < maxRetries-1 && isRetryable(resp.StatusCode, errorReason(respBody)) {
			delay := baseDelay * time.Duration(1<<attempt)
			if delay > maxDelay {
				delay = maxDelay
			}
			// Jitter ±25%.
			delay = time.Duration(float64(delay) * (0.75 + rand.Float64()*0.5))
			if ra := parseRetryAfter(retryAfter); ra > 0 {
				delay = min(ra, maxDelay) // honor server hint, but never sleep unbounded
			}
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(delay):
			}
			continue
		}
		return respBody, resp.StatusCode, nil
	}
	return nil, 0, fmt.Errorf("max retries exceeded")
}

// isRetryable reports whether a failed response is a transient error worth retrying.
func isRetryable(status int, reason string) bool {
	if status == http.StatusTooManyRequests || status >= 500 {
		return true
	}
	if status == http.StatusForbidden {
		switch reason {
		case "rateLimitExceeded", "userRateLimitExceeded", "quotaExceeded", "backendError", "RESOURCE_EXHAUSTED":
			return true
		}
	}
	return false
}

// errorReason extracts the first Google API error reason from a response body.
func errorReason(body []byte) string {
	var e struct {
		Error struct {
			Status string `json:"status"`
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	if len(e.Error.Errors) > 0 {
		return e.Error.Errors[0].Reason
	}
	return e.Error.Status
}

// parseRetryAfter parses a Retry-After header given in delta-seconds. Returns 0 if
// absent or in HTTP-date form (we fall back to computed backoff there).
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// --- Stateless helpers ---

// IsDeclined returns true if the authenticated user has declined this event.
func IsDeclined(event GCalEvent) bool {
	for _, a := range event.Attendees {
		if a.Self && a.ResponseStatus == "declined" {
			return true
		}
	}
	return false
}

// FormatAttendees formats an attendee list as a human-readable string.
func FormatAttendees(attendees []Attendee) string {
	var parts []string
	for _, a := range attendees {
		name := a.DisplayName
		if name == "" {
			name = a.Email
		} else {
			name += " <" + a.Email + ">"
		}
		if a.Organizer {
			name += " (organizer)"
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, ", ")
}
