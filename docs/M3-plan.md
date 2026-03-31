# M3 Implementation Plan

## Goal

One-way sync from each source calendar to the hub calendar. Sync handles creation, update, and deletion of placeholder events.

## Use Cases Covered

UC-0020 through UC-0033.

## Data Model Additions

### SyncedEvent

Tracks each source event → placeholder mapping.

```go
type SyncedEvent struct {
    ID               string `json:"id"               store:"id,pk"`
    UserID           string `json:"userId"            store:"user_id,index"`
    SourceCalendarID string `json:"sourceCalendarId"  store:"source_calendar_id,index"`
    SourceEventID    string `json:"sourceEventId"     store:"source_event_id"`
    TargetCalendarID string `json:"targetCalendarId"  store:"target_calendar_id"`
    TargetEventID    string `json:"targetEventId"     store:"target_event_id"`
    SourceUpdated    string `json:"sourceUpdated"     store:"source_updated"`
    CreatedAt        string `json:"createdAt"         store:"created_at"`
    UpdatedAt        string `json:"updatedAt"         store:"updated_at"`
}
```

### SyncLog

Audit trail per sync pass.

```go
type SyncLog struct {
    ID          string `json:"id"          store:"id,pk"`
    UserID      string `json:"userId"      store:"user_id,index"`
    StartedAt   string `json:"startedAt"   store:"started_at"`
    CompletedAt string `json:"completedAt" store:"completed_at"`
    Created     int    `json:"created"     store:"created"`
    Updated     int    `json:"updated"     store:"updated"`
    Deleted     int    `json:"deleted"     store:"deleted"`
    Errors      int    `json:"errors"      store:"errors"`
    Status      string `json:"status"      store:"status"`
    ErrorMsg    string `json:"errorMsg"    store:"error_msg"`
}
```

## Google Calendar API Operations

New functions in `gcal.go`:

### ListEvents

```go
func ListEvents(ctx context.Context, token, calendarID string, timeMin, timeMax time.Time) ([]GCalEvent, error)
```

- `GET /calendars/{id}/events`
- `singleEvents=true` (expand recurring)
- `orderBy=startTime`
- `timeMin` / `timeMax` scoped to sync window
- `maxResults=2500`
- Paginates via `nextPageToken`

### ListPlaceholders

```go
func ListPlaceholders(ctx context.Context, token, calendarID, sourceCalendarID string) ([]GCalEvent, error)
```

- `GET /calendars/{id}/events`
- `privateExtendedProperty=calendarSyncMarker=v1`
- `privateExtendedProperty=sourceCalendarId={sourceCalendarID}`
- Returns only our placeholder events for a specific source

### CreateEvent

```go
func CreateEvent(ctx context.Context, token, calendarID string, event *GCalEvent) (*GCalEvent, error)
```

- `POST /calendars/{id}/events?conferenceDataVersion=1&supportsAttachments=true&sendUpdates=none`

### UpdateEvent

```go
func UpdateEvent(ctx context.Context, token, calendarID, eventID string, event *GCalEvent) (*GCalEvent, error)
```

- `PATCH /calendars/{id}/events/{eventId}?conferenceDataVersion=1&supportsAttachments=true&sendUpdates=none`

### DeleteEvent

```go
func DeleteEvent(ctx context.Context, token, calendarID, eventID string) error
```

- `DELETE /calendars/{id}/events/{eventId}?sendUpdates=none`

### GCalEvent struct

Represents the Google Calendar event fields we read and write:

```go
type GCalEvent struct {
    ID               string
    Summary          string
    Description      string
    Location         string
    Start            EventTime     // dateTime or date (all-day)
    End              EventTime
    Status           string        // confirmed, tentative, cancelled
    Transparency     string        // opaque, transparent
    ConferenceData   json.RawMessage
    Attachments      json.RawMessage
    Attendees        []Attendee
    Reminders        Reminders
    ExtendedProperties ExtendedProperties
    Updated          string        // RFC3339 timestamp
    RecurringEventId string
}
```

## Sync Engine

New file `internal/app/sync.go`:

### Core Algorithm

```
RunSync(ctx, token, store, config) → SyncResult

1. Create SyncLog (status=running)

2. For each source calendar:
   a. Fetch source events (within sync window)
   b. Fetch existing placeholders on hub for this source
   c. Load SyncedEvent records for this source → hub
   d. Build lookup maps:
      - sourceEvents: map[eventID] → GCalEvent
      - placeholders: map[sourceEventID] → GCalEvent (from extendedProperties)
      - syncedEvents: map[sourceEventID] → SyncedEvent

   e. For each source event:
      - Skip if status == "cancelled"
      - Skip if user declined (attendee with self=true, responseStatus=declined)
      - Skip if it's a placeholder (has calendarSyncMarker)

      i.   No existing placeholder → CREATE
           - Build placeholder (see field mapping)
           - CreateEvent on hub
           - Insert SyncedEvent record

      ii.  Existing placeholder AND source.updated > syncedEvent.sourceUpdated → UPDATE
           - Build placeholder
           - UpdateEvent on hub
           - Update SyncedEvent record

      iii. Existing placeholder AND unchanged → SKIP

   f. For each existing placeholder with NO matching source event → DELETE
      - DeleteEvent on hub
      - Delete SyncedEvent record

3. Update SyncLog (status=completed, counts)
4. Return SyncResult
```

### Field Mapping: Source → Placeholder

```go
func buildPlaceholder(source GCalEvent, sourceCalID string) GCalEvent {
    desc := source.Description
    // Append attendee list as text
    if len(source.Attendees) > 0 {
        desc += "\n\n---\nAttendees: " + formatAttendees(source.Attendees)
    }

    return GCalEvent{
        Summary:      source.Summary,
        Description:  desc,
        Location:     source.Location,
        Start:        source.Start,
        End:          source.End,
        Transparency: source.Transparency,
        ConferenceData: source.ConferenceData,
        Attachments:    source.Attachments,
        Reminders:    Reminders{UseDefault: false},
        ExtendedProperties: ExtendedProperties{
            Private: map[string]string{
                "calendarSyncMarker": "v1",
                "sourceCalendarId":   sourceCalID,
                "sourceEventId":      source.ID,
            },
        },
    }
}
```

### Declined Detection

```go
func isDeclined(event GCalEvent) bool {
    for _, a := range event.Attendees {
        if a.Self && a.ResponseStatus == "declined" {
            return true
        }
    }
    return false
}
```

## API Endpoints

### POST /api/sync (UC-0029)

Triggers a full sync pass for the authenticated user. Returns the result.

```json
{
  "created": 5,
  "updated": 2,
  "deleted": 1,
  "errors": 0,
  "message": "Sync completed: 5 created, 2 updated, 1 deleted"
}
```

Requires valid config (hub + at least one source). Returns 400 if not configured.

### GET /api/sync/logs (UC-0030)

Returns the most recent sync logs (last 20).

```json
[
  {
    "id": "...",
    "startedAt": "2026-03-30T...",
    "completedAt": "2026-03-30T...",
    "created": 5, "updated": 2, "deleted": 1, "errors": 0,
    "status": "completed"
  }
]
```

## CLI Command

### `calendar-sync sync` (UC-0028)

Triggers a sync pass via the API (following the todo-api CLI pattern):

```go
syncCmd := &cobra.Command{
    Use:   "sync",
    Short: "Run a calendar sync pass",
    RunE: func(cmd *cobra.Command, args []string) error {
        // Use appcli.ClientForCommand for HTTP client + auth
        // POST /api/sync
        // Print results
    },
}
```

## Frontend Updates

Add to the Sync section:
- "Sync now" button that POSTs to `/api/sync` and shows the result
- Sync log table showing recent runs (fetched from `/api/sync/logs`)
- Loading state while sync is running

## E2E Tests

The sync engine requires real Google Calendar API access. For e2e without real calendars, we test the supporting pieces:

| Test | Use Cases | What it validates |
|------|-----------|-------------------|
| `e2e/04-sync-no-config.sh` | UC-0029 | POST /api/sync returns 400 when no config exists |
| `e2e/05-sync-logs.sh` | UC-0030 | GET /api/sync/logs returns empty list, then has entries after sync attempt |

Full sync integration (UC-0020–UC-0028, UC-0031–UC-0033) is validated manually against real Google Calendars.

## Rate Limiting

Implement exponential backoff with jitter for 403/429 responses:

```go
func doWithRetry(ctx context.Context, fn func() (*http.Response, error)) (*http.Response, error) {
    // Max 5 retries, base delay 500ms, max delay 30s, jitter ±25%
}
```

Applied to all Google Calendar API calls.

## Build Order

1. Add SyncedEvent and SyncLog to `store.go`, add store methods
2. Add event operations to `gcal.go` (GCalEvent struct, ListEvents, ListPlaceholders, CreateEvent, UpdateEvent, DeleteEvent, retry logic)
3. Create `sync.go` with RunSync algorithm
4. Add POST /api/sync and GET /api/sync/logs to `server.go`
5. Add `sync` CLI command to `main.go`
6. Update frontend with sync button and log display
7. Write e2e tests
8. Run `./dev ci`
9. Manual integration test against real calendars
