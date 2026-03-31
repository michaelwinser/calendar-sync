# M2 Implementation Plan

## Goal

User can select multiple calendars for synchronization and designate a hub calendar. Configuration is persisted and viewable.

## Use Cases Covered

UC-0010 through UC-0016.

## Architecture Review Changes

- `store:"user_id,unique"` → `store:"user_id,index"` (appbase store doesn't support `unique` tag; enforce one-per-user in application layer)
- Use `getAccessToken(r, google)` pattern for token refresh instead of raw `appbase.AccessToken(r)`
- Application-layer uniqueness check on `(user_id, calendar_id)` for SourceCalendar, with comment noting the invariant
- Timestamps use `time.Now().UTC().Format(time.RFC3339)` consistently
- `/api/calendars` returns proper HTTP errors on failure; frontend handles error state
- Single-resource config API: one `PUT /api/config` replaces separate hub/sources endpoints

## Data Model

Two entities using `store.Collection`:

### SyncConfig (one per user)

```go
type SyncConfig struct {
    ID              string `json:"id"              store:"id,pk"`
    UserID          string `json:"userId"          store:"user_id,index"`
    HubCalendarID   string `json:"hubCalendarId"   store:"hub_calendar_id"`
    HubCalendarName string `json:"hubCalendarName" store:"hub_calendar_name"`
    SyncWindowWeeks int    `json:"syncWindowWeeks" store:"sync_window_weeks"`
    CreatedAt       string `json:"createdAt"       store:"created_at"`
    UpdatedAt       string `json:"updatedAt"       store:"updated_at"`
}
```

One-per-user enforced at application layer (query-then-create/update).

### SourceCalendar (many per user)

```go
type SourceCalendar struct {
    ID           string `json:"id"           store:"id,pk"`
    UserID       string `json:"userId"       store:"user_id,index"`
    CalendarID   string `json:"calendarId"   store:"calendar_id"`
    CalendarName string `json:"calendarName" store:"calendar_name"`
    SyncToken    string `json:"-"            store:"sync_token"`
    CreatedAt    string `json:"createdAt"    store:"created_at"`
}
```

Uniqueness of `(user_id, calendar_id)` enforced at application layer.

## API Endpoints

### GET /api/calendars (UC-0010)

Fetches the user's Google Calendar list via the Calendar API. Uses `getAccessToken(r, google)` for token with refresh attempt.

Returns calendars where the user has at least `reader` access:

```json
[
  {"id": "primary@gmail.com", "name": "Personal", "primary": true, "accessRole": "owner"},
  {"id": "work@company.com", "name": "Work", "primary": false, "accessRole": "owner"},
  {"id": "team@group.calendar.google.com", "name": "Team", "primary": false, "accessRole": "reader"}
]
```

On failure (network error, revoked scopes): returns HTTP 502 or 403 with JSON error body.

### GET /api/config (UC-0015)

Returns the user's sync configuration.

```json
{
  "hubCalendarId": "hub@gmail.com",
  "hubCalendarName": "Hub Calendar",
  "syncWindowWeeks": 8,
  "sources": [
    {"calendarId": "work@company.com", "calendarName": "Work"},
    {"calendarId": "personal@gmail.com", "calendarName": "Personal"}
  ]
}
```

Returns `{"hubCalendarId": "", "hubCalendarName": "", "syncWindowWeeks": 8, "sources": []}` if not configured.

### PUT /api/config (UC-0011, UC-0012, UC-0013, UC-0014, UC-0016)

Accepts the full desired configuration state. The backend reconciles the diff against existing SourceCalendar rows: adds new ones, removes missing ones, preserves `sync_token` for sources that remain.

```json
{
  "hubCalendarId": "hub@gmail.com",
  "hubCalendarName": "Hub Calendar",
  "syncWindowWeeks": 8,
  "sources": [
    {"calendarId": "work@company.com", "calendarName": "Work"},
    {"calendarId": "personal@gmail.com", "calendarName": "Personal"}
  ]
}
```

Validation (UC-0016):
- Hub calendar cannot also be a source
- No duplicate source calendar IDs
- `syncWindowWeeks` defaults to 8 if omitted or zero

If no SyncConfig exists for the user, create one; otherwise update the existing row.

## Google Calendar API Client

New file `internal/app/gcal.go` with:

```go
func listCalendars(ctx context.Context, token string) ([]Calendar, error)
```

Calls `GET https://www.googleapis.com/calendar/v3/users/me/calendarList` with Bearer token. Paginates if needed. Returns id, summary (name), primary flag, and accessRole.

This is the only Google API call needed for M2. Event operations come in M3.

## Frontend

Update the home page to show:

1. **Hub section**: Current hub calendar (or "not set"). Dropdown to pick from available calendars. "Set as hub" button.
2. **Sources section**: Checkboxes for each available calendar. Checked = source. Hub calendar disabled in the checkbox list. Changes save via PUT /api/config.
3. **Status section**: Placeholder for sync controls (M3).

On page load, fetch `/api/calendars` and `/api/config` in parallel. Populate dropdowns/checkboxes from calendars, mark current selections from config.

Error states: if `/api/calendars` fails, show an error message instead of empty controls.

## File Structure

```
internal/
  app/
    store.go        # SyncConfig and SourceCalendar entities + store constructors
    gcal.go         # Google Calendar API client (listCalendars for now)
    server.go       # API handler implementations
main.go             # Updated to wire everything together
```

## E2E Tests

Config CRUD can be tested without Google API calls (no token needed for store operations):

| Test | Use Cases | What it validates |
|------|-----------|-------------------|
| `e2e/03-config.sh` | UC-0011–UC-0016 | Set hub, add/remove sources, view config, validation rules (hub≠source, no duplicates) |

UC-0010 (list calendars) requires a real Google token and is validated manually.

## Build Order

1. Create `internal/app/store.go` — entity definitions and store constructors
2. Create `internal/app/gcal.go` — listCalendars function
3. Create `internal/app/server.go` — API handlers (GET/PUT config, GET calendars)
4. Update `main.go` — wire stores and handlers, update home page HTML
5. Write `e2e/03-config.sh`
6. Run `./dev ci`
