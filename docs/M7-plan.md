# M7 Implementation Plan

## Goal

Bring the weekly-occupancy heatmap into the app as a read-only module
`internal/heatmap`, reusing `platform/calendar` and the M6 module contract, and retire
the standalone Apps Script prototype. See [DESIGN-platform.md](DESIGN-platform.md) §7.

Covers UC-0080 – UC-0084.

## Scope boundary

- **In:** `internal/heatmap` — a `GET /api/heatmap/events` endpoint (fetch, filter,
  dedupe, bucket → lean segments) and a `/heatmap` page (client-side aggregate +
  render, ported from `heatmap.gs`). Owns no store data. A home-page nav link. An
  additive field mask on the calendar client so the fetch is lean.
- **Out:** any change to sync/tools/store schemas; new config. The Apps Script
  prototype is retired (already gitignored — no repo change).
- **Deferred to M8 (record in DESIGN §11):** `/api/calendars` is owned by
  `internal/app` and fetched by the tools page and now the heatmap page. Promoting it
  to a shared owner belongs with the `app → internal/sync` split; M7 keeps fetching it.

## Design decisions

- **Client keeps the aggregation** (occupancy %, title tallies, ad-hoc summary,
  render) so filters re-render instantly (UC-0082). Server only fetches/filters/buckets.
- **One wall clock — the user's.** Bucketing an instant in its own rendered offset
  scatters one absolute time across rows when calendars/invites differ in zone. So the
  browser sends `tz = Intl.DateTimeFormat().resolvedOptions().timeZone`; the server
  rejects empty / `"Local"` / unparseable `tz` (400) then `time.LoadLocation(tz)`, and
  **everything shares `loc`**: `start`/`end` parsed as midnight-in-`loc` (used for both
  the fetch window and segment clipping), instants bucketed via `.In(loc)`, and
  `weekdayTotals` counting dates in `[start,end)`. `import _ "time/tzdata"` so
  non-container runs match the image.
- **Wall-clock minute math (BLOCKING).** On the `.In(loc)` instant: `min = Hour()*60 +
  Minute()` (never elapsed `sub.Minutes()` — wrong by an hour on DST days). Advance day
  boundaries with `time.Date(y,m,d+1,0,0,0,0,loc)` (never `Add(24h)`). A segment
  running to the next local midnight ends at `e = 1440`; drop zero-length segments.
- **Cross-calendar dedupe (BLOCKING).** This app manufactures duplicates: a synced
  meeting is a real event on its source *and* a placeholder on the hub / other
  calendars. Dedupe events **before** segmenting by canonical id — `app.SourceEventID(ev)`
  when `app.IsPlaceholder(ev)`, else `ev.ID` — so all copies of one meeting collapse to
  one (title-independent, so the 🔄 prefix doesn't defeat it). This is a sanctioned
  cross-module use of sync's exported API (like tools' `ListSyncPlaceholders`). First
  occurrence in `calendarId` order wins.

## Features

### 7.1 Module + wiring (first, so later steps are verifiable)
`internal/heatmap` with `RegisterRoutes` (`/api/heatmap/events`) + `RegisterPages`
(`/heatmap` via `LoginPage`), mounted in `main.go`. Home-page nav gets a **Heatmap**
link. Handler mirrors tools' `appbase.UserID(r)==""` → 401 guard.

### 7.2 Server: events → segments
`GET /api/heatmap/events?calendarId=<id>&…&start=YYYY-MM-DD&end=YYYY-MM-DD&tz=<IANA>`.
Token via `platform.AccessToken`. A testable `buildHeatmap(ctx, cal, token, calIDs,
start, end, loc)` seam (see Testing) does the work:

- **Validate/bound:** valid tz; `start < end`; span ≤ 24 months; 1 ≤ #calendars ≤ 20;
  else 400.
- **Fetch (lean, bounded):** per calendar `deps.Cal.ListEvents` over `[start,end)`
  with an additive **field mask** (new `ListEventsOptions{Fields}`, default off so sync
  is untouched) = prototype's `EVENT_FIELDS`. Run calendars with **bounded concurrency
  (4)** under a **wall-clock deadline (25s)**, mirroring bulk-delete; a calendar that
  errors or doesn't finish goes into `warnings`, never hanging or silently shrinking
  the grid.
- **Filter (busy):** skip all-day (`start.date`), cancelled, transparent,
  RSVP-declined, and `eventType` (with `|| "default"` fallback) not in `{default}`.
- **Dedupe** by canonical id (above), then **bucket** each timed instance into per-
  local-day segments `{t,r,w,d,s,e}`, clipped to `[start,end)`.
- **Cap:** if segments exceed ~50k, stop and add a `warnings` entry telling the user to
  narrow the range (dedupe already cuts this by the #calendars factor).
- **Response:** `{segments, weekdayTotals, warnings:[{calendarId,error}]}`.

Field-mask caveat (documented on the option): a mask that omits `nextSyncToken` yields
an empty `SyncToken` — fine for the heatmap's one-shot read, but sync's incremental
fetch must never use a mask lacking it.

### 7.3 Client: aggregate + render (go:embed, not a Go string)
`go:embed` a real `.html`/`.js` for the page (settles DESIGN §11.4 and avoids the
prototype's template-literal double-escaping — `\\p{…}`, `\\u{…}` — which would silently
break `cleanTitle` if copied into a Go raw string). Ported behavior:
- calendars from **`/api/calendars`**; segments from **`fetch('/api/heatmap/events?…&tz=')`**;
- a **warnings banner** when calendars were skipped;
- reslice (exclude/recurring), occupancy %, title tallies + ad-hoc summary, table +
  tooltips + details panel — **`escapeHtml` preserved on every title insertion path**;
- **default range = trailing** (today − 6 months → today): past occupancy is what the
  grid actually measures; a future window shows only already-booked (mostly recurring)
  events. User-adjustable.

## Data Model Changes
None. Read-only; owns no collections.

## Build Order
1. **7.1** wiring (endpoint + page stubs reachable).
2. **7.2** field mask on the client; `buildHeatmap` (pure/testable) + handler.
3. **7.3** ported client page.
4. e2e for the mounted endpoints.

## Testing
- **`buildHeatmap` against an httptest fake Google** (via `Client.BaseURL`, the
  `gcal_test.go` pattern): multi-calendar merge, pagination, canonical dedupe (same
  meeting on two calendar IDs → one segment), per-calendar-failure → `warnings`, and
  the 400 matrix (bad/empty/`"Local"` tz, `start>=end`, >24 months, 0 and 21
  calendars). This covers UC-0081 through the seam without the token/browser blockers.
- **Pure unit:** the segment builder across a **non-UTC tz** and a **DST-transition
  date** — assert a 10:00–10:30 event yields `{s:600,e:630}` on both the 23h and 25h
  day — plus `weekdayTotals` and the `e=1440` boundary.
- **e2e (UC-0084):** `/api/heatmap/events` and `/heatmap` mounted and reject
  unauthenticated (mirrors UC-0074).
- **Manual (live Google):** the rendered heatmap against `calendar-sync-test`.
- **PRD status (honest):** `UC-0081 tested via the endpoint seam; UC-0080/0082/0083
  browser/live-Google manual; UC-0084 e2e`.

## Risks
- **tz + DST correctness is load-bearing** — the DST/non-UTC tests are the guard.
- **Dedupe couples heatmap → app** (`IsPlaceholder`, `SourceEventID`, both exported) —
  the sanctioned pattern; moves to `internal/sync` in M8 with the rest.
- **Field mask** is additive/default-off; verify sync tests green after the client change.
- **Large ported client JS** — `go:embed` keeps it diffable against `heatmap.gs`;
  escaping and the un-double-escaped regexes are the things the manual check must confirm.
