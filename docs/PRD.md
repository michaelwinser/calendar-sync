# Calendar Sync - Product Requirements Document

## Overview

Calendar Sync keeps a user's free/busy status consistent across multiple Google Calendars by synchronizing events through a central hub calendar. See `docs/MVSR.md` for full context.

## Definitions

- **Hub calendar**: A designated Google Calendar that acts as the central aggregation point for all synced events.
- **Source calendar**: A Google Calendar the user has selected for synchronization. Events flow from source calendars to the hub.
- **Placeholder event**: A copy of a source event created on another calendar to reflect the user's busy status. Carries the same title, description, location, video links, attachments, and free/busy status as the original. Reminders are always off.
- **Sync pass**: A single execution of the synchronization logic — can be triggered manually or via API.

## Use Cases

### M1 — Authentication and Setup

| ID | Use Case | Description |
|----|----------|-------------|
| UC-0001 | App starts and serves UI | The app starts, listens on the configured port, and serves the web UI. The health endpoint returns OK. |
| UC-0002 | Unauthenticated user sees login | A user who is not logged in sees a login page when accessing the app. |
| UC-0003 | User logs in via Google | A user authenticates via Google OAuth. The app requests calendar read/write scopes. After login, the user is redirected to the main UI. |
| UC-0004 | User views auth status | An authenticated user can check their login status (email, scopes granted). |
| UC-0005 | User logs out | An authenticated user can log out. After logout, they see the login page. |

### M2 — Calendar Selection and Hub Management

| ID | Use Case | Description |
|----|----------|-------------|
| UC-0010 | List available calendars | An authenticated user can retrieve a list of their Google Calendars (id, name, primary flag). |
| UC-0011 | Designate hub calendar | The user selects an existing Google Calendar to serve as the hub. The selection is persisted. Only one hub calendar can be active at a time. |
| UC-0012 | Change hub calendar | The user can change which calendar is the hub. Changing the hub does not delete previously synced placeholder events (cleanup is a separate concern). |
| UC-0013 | Add source calendar | The user selects a Google Calendar to sync to the hub. The calendar is added to the sync configuration. The hub calendar cannot also be a source calendar. |
| UC-0014 | Remove source calendar | The user removes a calendar from the sync configuration. Previously synced placeholder events from that calendar remain on the hub until the next sync pass cleans them up. |
| UC-0015 | View sync configuration | The user can view their current configuration: which calendar is the hub and which calendars are selected as sources. |
| UC-0016 | Reject invalid configuration | The app prevents selecting the hub calendar as a source calendar. The app prevents designating a source calendar as the hub without first removing it as a source. |

### M3 — One-Way Sync (Sources → Hub)

| ID | Use Case | Description |
|----|----------|-------------|
| UC-0020 | Sync creates placeholders | A sync pass reads events from each source calendar and creates placeholder events on the hub for any events not already synced. |
| UC-0021 | Placeholder carries event fields | Placeholder events include: title, description, location, video conference link, attachments, start/end time, and all-day flag. |
| UC-0022 | Placeholder reflects free/busy | The placeholder's transparency (free/busy) matches the source event. If the source shows as "free", the placeholder also shows as "free". |
| UC-0023 | Placeholder has no reminders | All placeholder events are created with reminders disabled, regardless of the source event's reminder settings. |
| UC-0024 | Sync updates placeholders | When a source event is modified (time, title, location, etc.), the next sync pass updates the corresponding placeholder on the hub. |
| UC-0025 | Sync deletes orphaned placeholders | When a source event is deleted or cancelled, the next sync pass removes the corresponding placeholder from the hub. |
| UC-0026 | Sync is idempotent | Running a sync pass multiple times with no source changes produces no modifications to the hub. |
| UC-0027 | Sync handles recurring events | Recurring events are expanded into individual instances by the Google Calendar API. Each instance gets its own placeholder. Recurrence rules are not copied — placeholders are always single events. |
| UC-0028 | Trigger sync via CLI | A user can trigger a sync pass via the CLI command: `calendar-sync sync`. |
| UC-0029 | Trigger sync via API | A sync pass can be triggered via POST to the sync endpoint. This supports cron or webhook invocation. |
| UC-0030 | Sync reports results | After a sync pass, the app reports counts: created, updated, deleted, errors. |
| UC-0031 | Sync scopes to time window | A sync pass operates on events within a configurable time window (default: now to 8 weeks ahead). The window size is user-configurable. Events outside this window are not synced. |
| UC-0032 | Sync skips declined events | Events the user has declined are not synced to the hub. |
| UC-0033 | Sync skips placeholder events | The sync engine recognizes its own placeholder events and never treats them as source events. This prevents duplicate or chained placeholders. |
| UC-0034 | Sync skips working-location events | Events with `eventType=workingLocation` (Google's "working from" feature) are not synced. These are account-specific and cannot be created as regular events on all calendar types. Future: create matching working-location events on target calendars where supported. |

### M4 — Two-Way Sync (Hub → Calendars)

| ID | Use Case | Description |
|----|----------|-------------|
| UC-0040 | Hub syncs out to source calendars | After syncing sources → hub, the sync pass creates placeholders on each source calendar for events that originated from *other* source calendars. |
| UC-0041 | No self-sync | An event originating from calendar A does not get a placeholder back on calendar A. It only appears on the hub and on calendars B, C, etc. |
| UC-0042 | No placeholder chains | The sync engine distinguishes original events from placeholders. A placeholder is never treated as a source event — this prevents infinite sync loops. |
| UC-0043 | Outbound update propagation | When a source event is updated, the change propagates through the hub to placeholders on all other source calendars. |
| UC-0044 | Outbound delete propagation | When a source event is deleted, the placeholder on the hub and all outbound placeholders on other source calendars are removed. |
| UC-0045 | Source calendar removal cleans up | When a source calendar is removed from the sync configuration, the next sync pass removes all placeholder events that originated from that calendar (on the hub and on other source calendars). |
| UC-0046 | New source calendar backfills | When a new source calendar is added, the next sync pass creates placeholders for its existing events on the hub and on other source calendars within the sync time window. |
| UC-0047 | Read-only calendars sync inbound only | Source calendars where the user has read-only access sync events to the hub (and via the hub to other calendars), but the outbound sync silently skips writing placeholders back to them. No error is reported. |
| UC-0048 | Deleted placeholder is recreated | If a user manually deletes a placeholder event from any calendar, the next sync pass recreates it. The sync engine owns placeholders and keeps them consistent with the source. |
| UC-0049 | Source removal cleans up all placeholders | When a source calendar is unchecked/removed from the config, the next sync pass deletes all placeholder events that originated from that calendar — on the hub and on all other source calendars. |

### M5 — Polish and Automation

| ID | Use Case | Description |
|----|----------|-------------|
| UC-0050 | Sync window parameter | `POST /api/sync` accepts a `days` query parameter to override the configured sync window. Defaults to `syncWindowWeeks * 7`. |
| UC-0051 | Nudge endpoint for automated sync | `POST /api/sync/nudge` triggers sync for all users who are due based on their last sync time and configured interval. Auth: OIDC on Cloud Run, deployment key (`X-Nudge-Key`) on TrueNAS/localhost. |
| UC-0052 | Stored refresh token for background sync | The user's Google refresh token is captured on login and stored in SyncConfig. The nudge endpoint uses it to get fresh access tokens without a browser session. |
| UC-0053 | Past event auto-cleanup | At the end of each sync pass, placeholder events whose end date is before today are automatically deleted from the hub and all sync calendars. No configuration required. |
| UC-0054 | Batch sync writes | Sync create/update/delete operations use Google's batch API (up to 50 per request) for improved performance. |
| UC-0055 | Placeholder emoji prefix | Each sync calendar can have an optional emoji prefix that is prepended to placeholder event titles (e.g. "🔄 Team Standup"). Configured per-calendar in the UI. |
| UC-0056 | Placeholder color | Each sync calendar can have a color assigned to its placeholder events using Google Calendar's predefined colorId values (1–11), or "default". Configured per-calendar in the UI. |
| UC-0057 | Sync window control in UI | The sync window (in weeks) and sync interval (in minutes) are configurable via the UI. |
| UC-0058 | Dry-run sync | The CLI `sync` command supports a `--dry-run` flag that reports what would change without making API writes. |
| UC-0059 | Per-calendar sync log | Sync logs include per-calendar breakdowns of created/updated/deleted counts, shown in the UI. |

## Future Considerations

The following are not in the current roadmap but are anticipated extensions:

- **Per-source sync direction override**: Allow the user to mark a read-write calendar as "inbound only" (one-way sync to the hub, no placeholders written back). Use case: a shared team calendar the user wants visibility of but should not write to.
- **Per-calendar field control**: Allow the user to configure which fields are copied to placeholders on a per-calendar basis (e.g., hide titles from personal calendar when syncing to work hub).
- **Webhook-driven sync**: Use Google Calendar push notifications (`Events.watch`) instead of polling, so sync happens in near real-time when events change.

## Constraints

- **No local mode**: Appbase supports a "local" mode without OAuth login. This app requires Google OAuth for calendar access, so local mode is not supported. The app always requires authentication.
- **Devcontainer for non-Go tooling**: All development tooling other than Go itself runs inside the devcontainer (e.g., oapi-codegen, Node if needed). If additional tools are required, they must be explicitly added to the devcontainer configuration.

## Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | Sync pass completes within 60 seconds for up to 5 source calendars with 200 events each. |
| NFR-02 | The app runs on localhost, TrueNAS (Docker), and Cloud Run without code changes — only configuration differs. |
| NFR-03 | All state is stored in SQLite (local/TrueNAS) or Firestore (Cloud Run). No other persistence dependencies. |
| NFR-04 | The app handles Google Calendar API rate limits gracefully (exponential backoff, no data loss). |
| NFR-05 | OAuth token refresh is handled transparently — a sync pass does not fail due to an expired access token if a valid refresh token exists. |

## Use Case Status

Track implementation status here. E2e tests for all "done" use cases must pass in `./dev ci`.

| Milestone | Use Cases | Status |
|-----------|-----------|--------|
| M1 | UC-0001 – UC-0005 | done (UC-0003, UC-0005 manual only) |
| M2 | UC-0010 – UC-0016 | done (UC-0010 manual only) |
| M3 | UC-0020 – UC-0034 | done (UC-0020–UC-0028, UC-0031–UC-0034 manual only) |
| M4 | UC-0040 – UC-0049 | done (UC-0040–UC-0049 manual only) |
| M5 | UC-0050 – UC-0059 | not started |
