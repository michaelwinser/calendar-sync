# M4 Implementation Plan

## Goal

Two-way sync: after syncing sources → hub (M3), sync placeholders from the hub out to each source calendar so every calendar shows events from all other calendars.

## Use Cases Covered

UC-0040 through UC-0047.

## Design

M3 already syncs source events **inbound** to the hub. M4 adds an **outbound** phase that runs after the inbound phase completes.

### Outbound Sync Algorithm

After the inbound sync (sources → hub) completes:

```
For each source calendar S:
  1. Gather all hub events that did NOT originate from S
     - These are hub placeholders whose sourceCalendarId != S.calendarID
     - Plus any native hub events (no calendarSyncMarker) — though we don't expect these

  2. Fetch existing outbound placeholders on S
     - ListPlaceholders(ctx, token, S.calendarID, hubCalendarID) won't work here
       because outbound placeholders have sourceCalendarId = the ORIGINAL source,
       not the hub
     - Instead: ListPlaceholders with calendarSyncMarker=v1 on S,
       then filter to those we created (vs reclaim or other tools)
     - Better: use a different marker for outbound? No — keep it simple.
       Query all our placeholders on S (calendarSyncMarker=v1) and reconcile.

  3. Load SyncedEvent records where targetCalendarID = S.calendarID

  4. For each hub event not from S:
     - Skip if the event's sourceCalendarId == S.calendarID (no self-sync, UC-0041)
     - Skip cancelled / declined

     a. No existing outbound placeholder → CREATE on S
     b. Source updated → UPDATE on S
     c. Unchanged → SKIP

  5. For each existing outbound placeholder on S with no matching hub event → DELETE

  6. Log results
```

### Key Invariants

- **No self-sync (UC-0041)**: An event from Calendar A appears on the hub and on Calendars B, C — never back on A. The `sourceCalendarId` extended property identifies the origin.

- **No placeholder chains (UC-0042)**: The outbound sync reads events from the **hub** (which are either inbound placeholders or native hub events). It never reads from other source calendars directly. The inbound sync already skips placeholders (IsPlaceholder check). So chains cannot form.

- **Read-only calendars (UC-0047)**: When creating an outbound placeholder on a source calendar, if the API returns 403 (no write access), log and skip silently. The `accessRole` from CalendarList could also be checked upfront, but the API call is the definitive check.

### SyncedEvent Records

M3 already creates SyncedEvent records with:
- `SourceCalendarID` = the source calendar
- `TargetCalendarID` = the hub

M4 adds records with:
- `SourceCalendarID` = the original source calendar (preserved from the hub event's extended properties)
- `SourceEventID` = the hub event's sourceEventId (the original event ID)
- `TargetCalendarID` = the destination source calendar
- `TargetEventID` = the outbound placeholder's ID

This means for an event on Calendar A with 2 other source calendars (B, C), there are 3 SyncedEvent records:
1. A → Hub (inbound, from M3)
2. A → B (outbound, from M4)
3. A → C (outbound, from M4)

### Source Removal Cleanup (UC-0045)

When a source calendar is removed and the next sync runs:
- Inbound: the source's events on the hub become orphans → deleted (M3 already handles this)
- Outbound: the source's events on other calendars become orphans → deleted (new in M4)

This works automatically because:
- Removed source has no events in the inbound phase → hub placeholders deleted
- With hub placeholders gone, outbound phase has nothing to propagate → outbound placeholders deleted

### New Source Backfill (UC-0046)

When a new source calendar is added:
- Inbound: its events are synced to the hub (M3 handles this — no syncToken yet, full fetch)
- Outbound: hub now has new events → outbound phase creates placeholders on all other calendars

This also works automatically.

## Changes to sync.go

Modify `RunSync` to add an outbound phase after the existing inbound phase:

```go
func RunSync(...) {
    // Phase 1: Inbound (existing M3 code)
    for _, source := range sources {
        syncSourceToHub(...)
    }

    // Phase 2: Outbound (new)
    syncHubToSources(ctx, token, store, config, sources, result)
}
```

New function `syncHubToSources`:
- Fetch ALL placeholders on the hub (calendarSyncMarker=v1)
- For each source calendar, filter hub events to those NOT from that source
- Create/update/delete outbound placeholders on each source calendar
- Handle 403 silently for read-only calendars

## Changes to gcal.go

Add function to list ALL our placeholders on a calendar (not filtered by source):

```go
func ListAllPlaceholders(ctx context.Context, token, calendarID string) ([]GCalEvent, error)
```

Uses `privateExtendedProperty=calendarSyncMarker=v1` only (no sourceCalendarId filter).

## API Changes

None — `POST /api/sync` already triggers the full sync. The outbound phase is part of it.

## Frontend Changes

None — the sync button and logs already cover this. The log counts will now include outbound creates/updates/deletes.

## E2E Tests

Like M3, the outbound sync requires real Google Calendar API access. The existing e2e tests cover the API shape. Manual integration testing validates the full two-way flow.

## Build Order

1. Add `ListAllPlaceholders` to `gcal.go`
2. Add `syncHubToSources` to `sync.go`
3. Modify `RunSync` to call outbound phase
4. Run `./dev ci`
5. Manual integration test: verify events from A appear on B and vice versa
