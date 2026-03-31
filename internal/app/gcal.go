package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const calendarAPIBase = "https://www.googleapis.com/calendar/v3"

// Calendar represents a Google Calendar from the CalendarList API.
type Calendar struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Primary    bool   `json:"primary"`
	AccessRole string `json:"accessRole"`
}

// GCalEvent represents a Google Calendar event with the fields we read and write.
type GCalEvent struct {
	ID                 string             `json:"id,omitempty"`
	Summary            string             `json:"summary,omitempty"`
	Description        string             `json:"description,omitempty"`
	Location           string             `json:"location,omitempty"`
	Start              EventTime          `json:"start"`
	End                EventTime          `json:"end"`
	Status             string             `json:"status,omitempty"`
	Transparency       string             `json:"transparency,omitempty"` // "opaque" (default/empty) or "transparent" (free)
	ConferenceData     json.RawMessage    `json:"conferenceData,omitempty"`
	Attachments        json.RawMessage    `json:"attachments,omitempty"`
	Attendees          []Attendee         `json:"attendees,omitempty"`
	Reminders          *Reminders         `json:"reminders,omitempty"`
	ExtendedProperties *ExtendedProperties `json:"extendedProperties,omitempty"`
	ColorID            string             `json:"colorId,omitempty"`
	Updated            string             `json:"updated,omitempty"`
	RecurringEventId   string             `json:"recurringEventId,omitempty"`
	EventType          string             `json:"eventType,omitempty"` // default, workingLocation, outOfOffice, focusTime
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

// eventsListResponse is the Google Calendar events.list response.
type eventsListResponse struct {
	Items         []GCalEvent `json:"items"`
	NextPageToken string      `json:"nextPageToken"`
	NextSyncToken string      `json:"nextSyncToken"`
}

// calendarListResponse is the Google Calendar calendarList.list response.
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

// --- Calendar list ---

// ListCalendars fetches the user's calendar list from Google Calendar API.
func ListCalendars(ctx context.Context, token string) ([]Calendar, error) {
	var all []Calendar
	pageToken := ""

	for {
		u := calendarAPIBase + "/users/me/calendarList?maxResults=100"
		if pageToken != "" {
			u += "&pageToken=" + pageToken
		}

		body, err := doGCalRequest(ctx, token, "GET", u, nil)
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
			all = append(all, Calendar{
				ID:         item.ID,
				Name:       name,
				Primary:    item.Primary,
				AccessRole: item.AccessRole,
			})
		}

		if list.NextPageToken == "" {
			break
		}
		pageToken = list.NextPageToken
	}

	return all, nil
}

// --- Event operations ---

// ListEventsResult holds events and the sync token from a list call.
type ListEventsResult struct {
	Events    []GCalEvent
	SyncToken string
}

// ErrSyncTokenExpired is returned when the sync token is no longer valid (410 Gone).
var ErrSyncTokenExpired = fmt.Errorf("sync token expired (410 Gone)")

// ListEvents fetches events from a calendar within the given time window.
// Returns events and a sync token for future incremental fetches.
func ListEvents(ctx context.Context, token, calendarID string, timeMin, timeMax time.Time) (*ListEventsResult, error) {
	var result ListEventsResult
	pageToken := ""

	for {
		params := url.Values{}
		params.Set("singleEvents", "true")
		params.Set("orderBy", "startTime")
		params.Set("maxResults", "2500")
		params.Set("timeMin", timeMin.Format(time.RFC3339))
		params.Set("timeMax", timeMax.Format(time.RFC3339))
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}

		u := calendarAPIBase + "/calendars/" + url.PathEscape(calendarID) + "/events?" + params.Encode()
		body, err := doGCalRequest(ctx, token, "GET", u, nil)
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
func ListEventsIncremental(ctx context.Context, token, calendarID, syncToken string) (*ListEventsResult, error) {
	var result ListEventsResult
	pageToken := ""

	for {
		params := url.Values{}
		params.Set("syncToken", syncToken)
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}

		u := calendarAPIBase + "/calendars/" + url.PathEscape(calendarID) + "/events?" + params.Encode()
		body, statusCode, err := doGCalRequestRaw(ctx, token, "GET", u, nil)
		if statusCode == http.StatusGone {
			return nil, ErrSyncTokenExpired
		}
		if err != nil {
			return nil, fmt.Errorf("incremental sync: %w", err)
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

// ListPlaceholders fetches placeholder events on a calendar for a specific source.
func ListPlaceholders(ctx context.Context, token, calendarID, sourceCalendarID string) ([]GCalEvent, error) {
	var all []GCalEvent
	pageToken := ""

	for {
		params := url.Values{}
		params.Add("privateExtendedProperty", "calendarSyncMarker=v1")
		params.Add("privateExtendedProperty", "sourceCalendarId="+sourceCalendarID)
		params.Set("singleEvents", "true")
		params.Set("maxResults", "2500")
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}

		u := calendarAPIBase + "/calendars/" + url.PathEscape(calendarID) + "/events?" + params.Encode()
		body, err := doGCalRequest(ctx, token, "GET", u, nil)
		if err != nil {
			return nil, fmt.Errorf("listing placeholders: %w", err)
		}

		var resp eventsListResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("decoding placeholders: %w", err)
		}

		all = append(all, resp.Items...)

		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}

	return all, nil
}

// ListPlaceholdersInRange fetches sync-engine placeholders within a time window.
func ListPlaceholdersInRange(ctx context.Context, token, calendarID string, timeMin, timeMax time.Time) ([]GCalEvent, error) {
	var all []GCalEvent
	pageToken := ""

	for {
		params := url.Values{}
		params.Set("privateExtendedProperty", "calendarSyncMarker=v1")
		params.Set("singleEvents", "true")
		params.Set("maxResults", "2500")
		params.Set("timeMin", timeMin.Format(time.RFC3339))
		params.Set("timeMax", timeMax.Format(time.RFC3339))
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}

		u := calendarAPIBase + "/calendars/" + url.PathEscape(calendarID) + "/events?" + params.Encode()
		body, err := doGCalRequest(ctx, token, "GET", u, nil)
		if err != nil {
			return nil, fmt.Errorf("listing placeholders in range: %w", err)
		}

		var resp eventsListResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("decoding placeholders: %w", err)
		}

		all = append(all, resp.Items...)

		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}

	return all, nil
}

// ListAllPlaceholders fetches all placeholder events created by this app on a calendar.
func ListAllPlaceholders(ctx context.Context, token, calendarID string) ([]GCalEvent, error) {
	var all []GCalEvent
	pageToken := ""

	for {
		params := url.Values{}
		params.Set("privateExtendedProperty", "calendarSyncMarker=v1")
		params.Set("singleEvents", "true")
		params.Set("maxResults", "2500")
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}

		u := calendarAPIBase + "/calendars/" + url.PathEscape(calendarID) + "/events?" + params.Encode()
		body, err := doGCalRequest(ctx, token, "GET", u, nil)
		if err != nil {
			return nil, fmt.Errorf("listing all placeholders: %w", err)
		}

		var resp eventsListResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("decoding placeholders: %w", err)
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
func CreateEvent(ctx context.Context, token, calendarID string, event *GCalEvent) (*GCalEvent, error) {
	u := calendarAPIBase + "/calendars/" + url.PathEscape(calendarID) +
		"/events?conferenceDataVersion=1&supportsAttachments=true&sendUpdates=none"

	jsonBody, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}

	body, err := doGCalRequest(ctx, token, "POST", u, jsonBody)
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
func UpdateEvent(ctx context.Context, token, calendarID, eventID string, event *GCalEvent) (*GCalEvent, error) {
	u := calendarAPIBase + "/calendars/" + url.PathEscape(calendarID) +
		"/events/" + url.PathEscape(eventID) +
		"?conferenceDataVersion=1&supportsAttachments=true&sendUpdates=none"

	jsonBody, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}

	body, err := doGCalRequest(ctx, token, "PATCH", u, jsonBody)
	if err != nil {
		return nil, fmt.Errorf("updating event: %w", err)
	}

	var updated GCalEvent
	if err := json.Unmarshal(body, &updated); err != nil {
		return nil, fmt.Errorf("decoding updated event: %w", err)
	}
	return &updated, nil
}

// DeleteEvent deletes an event from the specified calendar.
func DeleteEvent(ctx context.Context, token, calendarID, eventID string) error {
	u := calendarAPIBase + "/calendars/" + url.PathEscape(calendarID) +
		"/events/" + url.PathEscape(eventID) + "?sendUpdates=none"

	_, err := doGCalRequest(ctx, token, "DELETE", u, nil)
	return err
}

// BatchDeleteEvents deletes multiple events using Google's batch API.
// Processes up to 50 events per batch request. Returns counts of deleted and errored.
func BatchDeleteEvents(ctx context.Context, token, calendarID string, eventIDs []string) (deleted, errors int) {
	const batchSize = 50

	for i := 0; i < len(eventIDs); i += batchSize {
		end := i + batchSize
		if end > len(eventIDs) {
			end = len(eventIDs)
		}
		chunk := eventIDs[i:end]

		d, e := doBatchDelete(ctx, token, calendarID, chunk)
		deleted += d
		errors += e
	}
	return
}

func doBatchDelete(ctx context.Context, token, calendarID string, eventIDs []string) (deleted, errors int) {
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

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://www.googleapis.com/batch/calendar/v3", &body)
	if err != nil {
		return 0, len(eventIDs)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "multipart/mixed; boundary="+boundary)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, len(eventIDs)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, len(eventIDs)
	}

	// Parse multipart response to count successes/failures.
	// Each part has an HTTP status line like "HTTP/1.1 204 No Content"
	respStr := string(respBody)
	for _, id := range eventIDs {
		_ = id
		// Count HTTP 204 (success) and HTTP 200 (success) responses
	}
	// Simple approach: count "HTTP/1.1 204" and "HTTP/1.1 200" occurrences
	for _, line := range strings.Split(respStr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "HTTP/1.1 2") {
			deleted++
		} else if strings.HasPrefix(line, "HTTP/1.1 4") || strings.HasPrefix(line, "HTTP/1.1 5") {
			errors++
		}
	}

	return
}

// --- HTTP helpers with retry ---

// doGCalRequest makes an API request with exponential backoff on 403/429.
func doGCalRequest(ctx context.Context, token, method, url string, body []byte) ([]byte, error) {
	respBody, statusCode, err := doGCalRequestRaw(ctx, token, method, url, body)
	if err != nil {
		return nil, err
	}
	if statusCode >= 400 {
		return nil, fmt.Errorf("API status %d: %s", statusCode, string(respBody))
	}
	return respBody, nil
}

// doGCalRequestRaw makes an API request with retry, returning the body and status code.
// Caller is responsible for checking status code for specific handling (e.g., 410 Gone).
func doGCalRequestRaw(ctx context.Context, token, method, rawURL string, reqBody []byte) ([]byte, int, error) {
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

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, 0, fmt.Errorf("request failed: %w", err)
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("reading response: %w", err)
		}

		// Retry on rate limit or server error
		if (resp.StatusCode == 403 || resp.StatusCode == 429 || resp.StatusCode >= 500) && attempt < maxRetries-1 {
			delay := baseDelay * time.Duration(1<<attempt)
			if delay > maxDelay {
				delay = maxDelay
			}
			// Jitter ±25%
			jitter := time.Duration(float64(delay) * (0.75 + rand.Float64()*0.5))
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(jitter):
			}
			continue
		}

		// For DELETE, 204 No Content is success
		if method == "DELETE" && resp.StatusCode == http.StatusNoContent {
			return nil, resp.StatusCode, nil
		}

		return respBody, resp.StatusCode, nil
	}

	return nil, 0, fmt.Errorf("max retries exceeded")
}

// --- Helpers ---

// IsPlaceholder returns true if the event was created by the sync engine.
func IsPlaceholder(event GCalEvent) bool {
	if event.ExtendedProperties == nil {
		return false
	}
	return event.ExtendedProperties.Private["calendarSyncMarker"] == "v1"
}

// IsDeclined returns true if the authenticated user has declined this event.
func IsDeclined(event GCalEvent) bool {
	for _, a := range event.Attendees {
		if a.Self && a.ResponseStatus == "declined" {
			return true
		}
	}
	return false
}

// SourceEventID returns the source event ID from a placeholder's extended properties.
func SourceEventID(event GCalEvent) string {
	if event.ExtendedProperties == nil {
		return ""
	}
	return event.ExtendedProperties.Private["sourceEventId"]
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

// PlaceholderOptions configures how a placeholder event looks on the target calendar.
type PlaceholderOptions struct {
	EmojiPrefix string // prepended to title, e.g. "🔄 "
	ColorID     string // Google Calendar colorId (1-11), empty for default
}

// BuildPlaceholder creates a placeholder event from a source event.
func BuildPlaceholder(source GCalEvent, sourceCalID string, opts PlaceholderOptions) GCalEvent {
	desc := source.Description
	if len(source.Attendees) > 0 {
		if desc != "" {
			desc += "\n\n"
		}
		desc += "---\nAttendees: " + FormatAttendees(source.Attendees)
	}

	summary := source.Summary
	if opts.EmojiPrefix != "" {
		summary = opts.EmojiPrefix + " " + summary
	}

	p := GCalEvent{
		Summary:      summary,
		Description:  desc,
		Location:     source.Location,
		Start:        source.Start,
		End:          source.End,
		Transparency: source.Transparency, // empty means "opaque" (busy) — the API default
		ConferenceData: source.ConferenceData,
		Attachments:    source.Attachments,
		Reminders:    &Reminders{UseDefault: false},
		ExtendedProperties: &ExtendedProperties{
			Private: map[string]string{
				"calendarSyncMarker": "v1",
				"sourceCalendarId":   sourceCalID,
				"sourceEventId":      source.ID,
			},
		},
	}

	if opts.ColorID == "source" {
		// Use the source event's color (if it has one)
		p.ColorID = source.ColorID
	} else if opts.ColorID != "" {
		p.ColorID = opts.ColorID
	}

	return p
}
