# Calendar Sync - Design Document

## Architecture Overview

Calendar Sync is a Go web application built on appbase. It uses Google Calendar API v3 to read events from source calendars and create/update/delete placeholder events on a hub calendar (and in M4, back out to source calendars).

```
┌─────────────────────────────────────────────────────┐
│                    calendar-sync                     │
│                                                      │
│  ┌──────────┐  ┌───────────┐  ┌──────────────────┐  │
│  │  Web UI   │  │  REST API  │  │       CLI        │  │
│  │ (HTML/CSS)│  │  (chi)     │  │     (cobra)      │  │
│  └─────┬─────┘  └─────┬─────┘  └────────┬─────────┘  │
│        │              │                  │             │
│        └──────────────┼──────────────────┘             │
│                       │                                │
│              ┌────────┴────────┐                       │
│              │   Sync Engine   │                       │
│              └────────┬────────┘                       │
│                       │                                │
│         ┌─────────────┼─────────────┐                  │
│         │             │             │                  │
│  ┌──────┴──────┐ ┌────┴────┐ ┌─────┴─────┐           │
│  │  Google Cal  │ │  Store  │ │  appbase  │           │
│  │  API Client  │ │ (SQLite)│ │  (auth)   │           │
│  └─────────────┘ └─────────┘ └───────────┘           │
└─────────────────────────────────────────────────────┘
           │
           ▼
   Google Calendar API v3
```

## Google Calendar API Usage

### OAuth Scope

```
https://www.googleapis.com/auth/calendar
```

The full `calendar` scope is required because `calendar.events` does not grant access to `calendarList.list` (needed to discover the user's calendars). This scope provides:
- Read the calendar list
- Read events from all calendars the user has access to
- Create, update, and delete events on calendars with write access

### Endpoints Used

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/users/me/calendarList` | GET | List user's calendars |
| `/calendars/{id}/events` | GET | List events (full or incremental via syncToken) |
| `/calendars/{id}/events` | POST | Create placeholder event |
| `/calendars/{id}/events/{eventId}` | PATCH | Update placeholder event |
| `/calendars/{id}/events/{eventId}` | DELETE | Delete placeholder event |

### Query Parameters

When creating or updating events, always pass:
- `conferenceDataVersion=1` — required to write conferenceData
- `supportsAttachments=true` — required to write attachments
- `sendUpdates=none` — placeholders should not send email notifications

When listing events for sync:
- `singleEvents=true` — expands recurring events into instances
- `timeMin` / `timeMax` — scoped to the sync window
- `orderBy=startTime` — deterministic ordering (requires singleEvents=true)
- `maxResults=2500` — maximum page size

When listing placeholder events specifically:
- `privateExtendedProperty=calendarSyncMarker=v1` — finds only our placeholders

## Placeholder Identification

The sync engine tags every placeholder event it creates using **private extended properties**. These are key-value pairs visible only on the specific calendar's copy of the event.

```json
{
  "extendedProperties": {
    "private": {
      "calendarSyncMarker": "v1",
      "sourceCalendarId": "work@example.com",
      "sourceEventId": "abc123xyz"
    }
  }
}
```

| Property | Purpose |
|----------|---------|
| `calendarSyncMarker` | Identifies this event as a sync-engine placeholder. Value is a version string for future schema changes. |
| `sourceCalendarId` | The calendar ID where the original event lives. Used to avoid self-sync (UC-0041) and for cleanup on source removal (UC-0045). |
| `sourceEventId` | The event ID on the source calendar. Used to correlate placeholders with source events for updates and deletes. |

**Why private extended properties?**
- They are invisible to other attendees (privacy)
- They can be used as a filter parameter on `Events.list` — the sync engine can efficiently query for only its own placeholders without fetching all events
- They survive event edits by the user in the Google Calendar UI

## Data Model

### SyncConfig

Stores the user's sync configuration. One row per user.

```go
type SyncConfig struct {
    ID              string `store:"id,pk"`
    UserID          string `store:"user_id,unique"`
    HubCalendarID   string `store:"hub_calendar_id"`
    HubCalendarName string `store:"hub_calendar_name"`
    SyncWindowWeeks int    `store:"sync_window_weeks"` // default: 8
    CreatedAt       string `store:"created_at"`
    UpdatedAt       string `store:"updated_at"`
}
```

### SourceCalendar

Stores which calendars are selected as sources. Many-to-one with SyncConfig.

```go
type SourceCalendar struct {
    ID           string `store:"id,pk"`
    UserID       string `store:"user_id,index"`
    CalendarID   string `store:"calendar_id"`
    CalendarName string `store:"calendar_name"`
    SyncToken    string `store:"sync_token"`    // for incremental sync
    CreatedAt    string `store:"created_at"`
}
```

### SyncedEvent

Tracks the mapping between source events and placeholder events. This is the core bookkeeping table that makes update and delete propagation possible.

```go
type SyncedEvent struct {
    ID               string `store:"id,pk"`
    UserID           string `store:"user_id,index"`
    SourceCalendarID string `store:"source_calendar_id,index"`
    SourceEventID    string `store:"source_event_id"`
    TargetCalendarID string `store:"target_calendar_id"`
    TargetEventID    string `store:"target_event_id"`
    SourceUpdated    string `store:"source_updated"` // event's `updated` timestamp
    CreatedAt        string `store:"created_at"`
    UpdatedAt        string `store:"updated_at"`
}
```

Compound index on `(source_calendar_id, source_event_id, target_calendar_id)` for fast lookup during sync.

### SyncLog

Audit trail for sync pass executions.

```go
type SyncLog struct {
    ID          string `store:"id,pk"`
    UserID      string `store:"user_id,index"`
    StartedAt   string `store:"started_at"`
    CompletedAt string `store:"completed_at"`
    Created     int    `store:"created"`
    Updated     int    `store:"updated"`
    Deleted     int    `store:"deleted"`
    Errors      int    `store:"errors"`
    Status      string `store:"status"` // "running", "completed", "failed"
    ErrorMsg    string `store:"error_msg"`
}
```

### Entity Relationships

```
SyncConfig (1 per user)
    │
    ├── HubCalendarID (one designated hub)
    │
    └── SourceCalendar (0..n per user)
            │
            └── SyncedEvent (0..n per source calendar)
                    │
                    ├── sourceEventId → event on source calendar
                    └── targetEventId → placeholder on target calendar
```

## Sync Algorithm

### M3: One-Way Sync (Sources → Hub)

For each enabled source calendar:

```
1. FETCH source events
   - If syncToken exists for this source: incremental sync
     - GET /events?syncToken=X
     - On 410 Gone: clear syncToken, fall through to full sync
   - Else: full sync
     - GET /events?timeMin=NOW&timeMax=NOW+window&singleEvents=true

2. FETCH existing placeholders on hub for this source
   - GET /events?privateExtendedProperty=sourceCalendarId={sourceCalId}
     &privateExtendedProperty=calendarSyncMarker=v1

3. BUILD lookup maps
   - sourceEvents:  map[sourceEventId] → event
   - placeholders:  map[sourceEventId] → placeholder event
   - syncedEvents:  map[sourceEventId] → SyncedEvent record

4. FOR EACH source event:
   - Skip if status == "cancelled"
   - Skip if user declined (attendees[self=true].responseStatus == "declined")
   - Skip if it's a placeholder (has calendarSyncMarker extended property)

   a. No existing placeholder → CREATE
      - Build placeholder from source (see Field Mapping below)
      - POST /calendars/{hubId}/events
      - Insert SyncedEvent record

   b. Existing placeholder AND source.updated > syncedEvent.sourceUpdated → UPDATE
      - PATCH /calendars/{hubId}/events/{placeholderId}
      - Update SyncedEvent record

   c. Existing placeholder AND source unchanged → SKIP

5. FOR EACH existing placeholder with NO matching source event → DELETE
   - DELETE /calendars/{hubId}/events/{placeholderId}
   - Delete SyncedEvent record

6. PERSIST new syncToken (from last page of Events.list response)

7. LOG results to SyncLog
```

### M4: Two-Way Sync (Hub → Source Calendars)

After the inbound sync (sources → hub) completes:

```
For each source calendar S:
  For each event on the hub that did NOT originate from S:
    - Create/update/delete placeholder on S
    - Use sourceCalendarId extended property to determine origin
    - SyncedEvent records track hub→S mappings separately
```

The `targetCalendarID` field in SyncedEvent distinguishes inbound mappings (target = hub) from outbound mappings (target = source calendar).

### Field Mapping: Source → Placeholder

```go
placeholder := &CalendarEvent{
    Summary:     source.Summary,
    Description: buildDescription(source), // original description + attendee list
    Location:    source.Location,
    Start:       source.Start,       // dateTime or date (all-day)
    End:         source.End,
    Transparency: source.Transparency, // "opaque" or "transparent"
    ConferenceData: source.ConferenceData,
    Attachments:    source.Attachments,
    Reminders: Reminders{
        UseDefault: false,
        Overrides:  []Reminder{},     // always empty — no reminders
    },
    ExtendedProperties: ExtendedProperties{
        Private: map[string]string{
            "calendarSyncMarker": "v1",
            "sourceCalendarId":  source.CalendarID,
            "sourceEventId":     source.ID,
        },
    },
}
```

**Attendees in description**: Attendees are not added as event attendees (that would trigger invitations and cause confusion). Instead, the attendee list is appended as text to the placeholder's description:

```
[original description]

---
Attendees: Alice <alice@example.com>, Bob <bob@example.com> (organizer)
```

Fields intentionally **not** copied:
- `attendees` — not as attendees; included as text in description instead (see above)
- `recurrence` — placeholders are always single instances
- `reminders` — always disabled
- `organizer` / `creator` — set automatically by the API
- `id` — generated by the API

## API Design

### Endpoints

| Method | Path | Description | Milestone |
|--------|------|-------------|-----------|
| GET | `/health` | Health check | M1 |
| GET | `/api/auth/status` | Auth status | M1 |
| GET | `/api/calendars` | List user's Google Calendars | M2 |
| GET | `/api/config` | Get sync configuration | M2 |
| PUT | `/api/config/hub` | Set hub calendar | M2 |
| POST | `/api/config/sources` | Add source calendar | M2 |
| DELETE | `/api/config/sources/{id}` | Remove source calendar | M2 |
| POST | `/api/sync` | Trigger sync pass | M3 |
| GET | `/api/sync/logs` | List recent sync logs | M3 |
| GET | `/api/sync/events` | List synced event mappings | M3 |

Auth endpoints (`/api/auth/login`, `/api/auth/callback`, `/api/auth/logout`) are provided by appbase.

### Response Format

All API responses use the appbase `server.RespondJSON` / `server.RespondError` helpers:

```json
// Success
{"hubCalendar": {"id": "...", "name": "..."}, "sources": [...]}

// Error
{"error": "message"}
```

### Sync Trigger Response

```json
{
  "created": 5,
  "updated": 2,
  "deleted": 1,
  "errors": 0,
  "message": "Sync completed: 5 created, 2 updated, 1 deleted"
}
```

## Frontend

Plain HTML served by the Go backend with minimal JavaScript for API calls. No SPA framework, no build step.

### Pages

**Login page**: Provided by appbase's `LoginPage()` handler.

**Main page** (`/`): Shows:
- Current sync configuration (hub calendar, source calendars)
- Calendar picker (dropdown populated from `/api/calendars`)
- "Set as hub" / "Add as source" / "Remove" actions
- "Sync now" button
- Recent sync log (last 10 runs with counts)

### Approach

- HTML templates rendered server-side (Go `html/template`)
- CSS for layout and styling (no framework)
- Vanilla JavaScript for:
  - Fetch calls to the API endpoints
  - Updating the DOM after actions (add/remove source, trigger sync)
  - No routing, no state management, no bundler

## Incremental Sync and Token Management

### syncToken Lifecycle

Each source calendar has its own `syncToken` stored in the `SourceCalendar` record.

```
First sync:
  1. Full fetch with timeMin/timeMax → process events
  2. Store nextSyncToken from last page

Subsequent syncs:
  1. Fetch with syncToken → get only changes since last sync
  2. Process changes (created, updated, cancelled)
  3. Store new nextSyncToken

Token expired (410 Gone):
  1. Clear syncToken
  2. Perform full sync (fetch all events in the time window)
  3. Reconcile against existing SyncedEvent records
     - Existing records are NOT cleared — they serve as the
       ground truth for which placeholders already exist
     - The full sync creates/updates/deletes as normal,
       using SyncedEvent records to match source→placeholder
```

### syncToken vs privateExtendedProperty

These two mechanisms serve different purposes and **cannot be combined** in a single API call:

- **syncToken**: Used when fetching source events to get incremental changes efficiently
- **privateExtendedProperty filter**: Used when fetching placeholder events on the hub/target calendar to find only our events

The sync engine makes two separate API calls per source calendar per sync pass:
1. Source events (with syncToken when available)
2. Hub placeholders for that source (with privateExtendedProperty filter)

## Rate Limiting

The Google Calendar API has per-user and per-project quotas (exact numbers vary by project, check Cloud Console).

### Strategy

- Exponential backoff with jitter on 403/429 responses
- Maximum 5 retries per request
- Use syncToken to minimize API calls (only fetch changes)
- Process source calendars sequentially (not in parallel) to stay within per-user quota
- Log rate limit hits for visibility

## Deployment

Three deployment targets, differing only in configuration:

| Target | Store | Auth | Invocation |
|--------|-------|------|------------|
| localhost | SQLite (`data/app.db`) | Google OAuth (localhost redirect) | `go run . serve` |
| TrueNAS | SQLite (Docker volume) | Google OAuth | Docker Compose |
| Cloud Run | Firestore | Google OAuth (Cloud Run URL) | `appbase deploy` |

Sync can be triggered by:
- Manual click in the UI
- CLI: `calendar-sync sync`
- HTTP: `POST /api/sync` (for cron jobs or webhooks)
