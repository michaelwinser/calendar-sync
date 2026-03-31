# M5 Implementation Plan

## Goal

Polish, performance, and automation. Make the sync reliable enough to run unattended and pleasant enough to forget about.

## Features

### 5.1 Sync window parameter and UI control

Add `days` query parameter to `POST /api/sync`. Default to the configured `syncWindowWeeks * 7`. The UI exposes a sync window selector (merged with 5.7).

### 5.2 Nudge endpoint for automated sync

A single deployment-level endpoint that triggers sync for all users who are due:

```
POST /api/sync/nudge
```

**How it works:**
1. External scheduler (cron, Cloud Scheduler) hits `/api/sync/nudge` on a fixed interval (e.g. every 5 minutes)
2. The endpoint enumerates all user SyncConfigs
3. For each user, checks if a sync is due based on last sync time and schedule config
4. If due: uses the user's stored refresh token to get a fresh access token, runs sync
5. Logs results per user

**Auth by deployment target:**

| Target | Auth mechanism |
|--------|---------------|
| Cloud Run | Google OIDC — Cloud Scheduler invokes with a service account. No keys to manage. |
| TrueNAS | Deployment key stored in appconfig (`SYNC_NUDGE_KEY`). Cron passes `X-Nudge-Key` header. |
| localhost | Same deployment key, or unauthenticated for dev. |

The deployment key is set via `appconfig set SYNC_NUDGE_KEY=<random>` — it's per-deployment, not per-user.

**Schedule config per user (stored in SyncConfig):**
- `SyncIntervalMinutes` — how often to sync (default: 15)
- `SyncWindowDays` — how far ahead to look (default: syncWindowWeeks * 7)

The nudge endpoint compares `lastSyncAt` against `SyncIntervalMinutes` to decide who's due.

### 5.3 Past event auto-cleanup

At the end of each sync pass, delete all placeholder events whose end date is before today (day boundary, not time). Applied to hub and all sync calendars. No configuration — this always happens.

### 5.4 Batch writes for sync performance

Batch create/update/delete operations using Google's batch API (up to 50 per request), same pattern as `BatchDeleteEvents` in the tools. Apply to:
- Inbound sync (creates/updates/deletes on hub)
- Outbound sync (creates/updates/deletes on sync calendars)

### 5.5 Placeholder visual distinction

Per-calendar options in the UI:
- **Emoji prefix**: optional emoji prepended to placeholder event titles (e.g. "🔄", "📅")
- **Color**: Google Calendar colorId (1–11 predefined colors), or "default"

Stored in SourceCalendar entity. Applied during BuildPlaceholder.

### 5.6 Rename "Source Calendars" to "Sync Calendars"

UI label change. The calendars are sources for inbound sync AND targets for outbound sync — "sync calendars" is more accurate.

### 5.8 Fix date-filtered placeholder search in tools

Investigate why `privateExtendedProperty` + `timeMin`/`timeMax` doesn't return results. Fix or document the limitation.

### 5.9 Idempotent sync verification

Run sync twice with no source changes and verify zero API writes on the second pass. Add a `--dry-run` flag to the CLI sync command that reports what would change without making API calls.

### 5.10 Per-calendar sync log breakdown

Extend SyncLog to include per-calendar counts. Show in the UI which calendars had changes.

### 5.11 Docker Compose for TrueNAS

Dockerfile (multi-stage Alpine build) and docker-compose.yml for local/TrueNAS deployment. Uses appbase's deploy patterns.

### 5.12 Cloud Scheduler setup

Extend `./dev provision` to create a Cloud Scheduler job:
- Every 5 minutes: `POST /api/sync/nudge`
- Authenticated via service account OIDC (Cloud Scheduler → Cloud Run, no keys)

The nudge endpoint internally decides which users need syncing and with what window.

## Data Model Changes

### SourceCalendar additions

```go
EmojiPrefix  string `json:"emojiPrefix"  store:"emoji_prefix"`
ColorID      string `json:"colorId"      store:"color_id"`
```

### SyncConfig additions

```go
RefreshToken        string `json:"-"  store:"refresh_token"`
SyncIntervalMinutes int    `json:"syncIntervalMinutes" store:"sync_interval_minutes"`
LastSyncAt          string `json:"-"  store:"last_sync_at"`
```

## Build Order

1. **5.1 + 5.6**: Sync window param in API + UI, rename to "Sync Calendars"
2. **5.2**: Nudge endpoint + refresh token storage + schedule config
3. **5.3**: Past event cleanup
4. **5.4**: Batch writes
5. **5.5**: Placeholder colors/emoji
6. **5.8**: Fix tools date filter
7. **5.9**: Idempotent verification + dry-run
8. **5.10**: Per-calendar log breakdown
9. **5.11**: Docker Compose
10. **5.12**: Cloud Scheduler

## Nudge Auth Flow

### Cloud Run (OIDC — zero key management)

```
Cloud Scheduler
  → POST https://calendar-sync.run.app/api/sync/nudge
    Auth: OIDC token signed by service account

Cloud Run validates OIDC token automatically (IAM invoker role)

Server:
  1. Request passes Cloud Run's built-in OIDC validation
  2. Enumerate all SyncConfigs
  3. For each user due for sync:
     a. Use stored RefreshToken → get fresh access token
     b. Run sync
     c. Update LastSyncAt
```

### TrueNAS / localhost (deployment key)

```
Cron: curl -X POST -H "X-Nudge-Key: <key>" http://localhost:4004/api/sync/nudge

Server:
  1. Check X-Nudge-Key against SYNC_NUDGE_KEY env var
  2. Same enumeration + sync logic as above
```

The refresh token is captured from the user's session on login. If revoked, the nudge logs the error — user must re-login to re-authorize.
