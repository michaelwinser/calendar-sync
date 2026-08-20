# M6 Implementation Plan

## Goal

Establish the modular-monolith foundation from [DESIGN-platform.md](DESIGN-platform.md)
and prove it on the safest, near-data-free feature: extract **Tools** (bulk
search/delete) into its own module behind a shared calendar/bulk-ops layer, and fix
bulk delete for large selections. Sync is left in place (modularized in M8); M6 must
not change sync behavior or data.

Covers UC-0070 – UC-0074, NFR-06, NFR-07 (documented; exercised in M7/M8).

## Scope boundary

- **In:** the module contract + wiring; a shared `platform/calendar` package (a
  configurable `Client` + bulk-ops); extracting Tools into `internal/tools`; fixing
  bulk delete's execution model; fixing token refresh for multi-request operations;
  establishing the collection-namespacing convention with an explicit M8 mapping.
- **Out:** moving sync into a module, renaming/migrating collections, rerouting sync's
  own delete path, the heatmap (M7), the app rename. Sync-specific placeholder logic
  stays in sync, and sync keeps `BatchDeleteEvents` unchanged.

## Features

### 6.1 Module contract + wiring

A module is a package that exposes two hooks, matching the app's two lifecycle phases
(API routes register in `setup()` on `a.Router()`; pages register inside
`SetServeFunc` on `a.Server().Router()`, which runs only for `serve`):

- `RegisterRoutes(deps platform.Deps)` — API routes under `/api/<module>/...`;
- `RegisterPages(deps platform.Deps)` — authenticated pages via the appbase
  `LoginPage` wrapper (the `/tools` page uses this today).

`platform.Deps` bundles what a module may depend on: the chi `Router`, `LoginPage`,
`*db.DB`, and the Google client. A module may also depend on another module's
**exported API** (see 6.3) — never its routes, handlers, or raw collections.

Auth boundary rule (documented in the contract): appbase applies session auth by the
`/api/` prefix; routes outside it are unauthenticated by design (as `/sync/nudge` is).
The existing catch-all `r.Get("/*", …)` coexists with specific page routes via chi
specificity — the contract notes this so M7's `/heatmap` can rely on it.

`main.go` builds `Deps` once and calls each module's hooks; existing sync/config
endpoints keep their paths (no client-visible change).

### 6.2 Shared calendar layer (`internal/platform/calendar`)

Carve the *generic* Calendar API surface out of `internal/app/gcal.go`. This is
mechanical but **not signature-preserving**: today's package-level functions over the
`calendarAPIBase` const become a **`Client{ BaseURL, HTTP }` struct with methods**, so
tests can point `BaseURL` at a local fake (see Testing). Every generic call site
(sync + tools) updates to the client; sync logic and data are unchanged.

- **To `platform/calendar`:**
  - `Client` with the shared request helper as an exported `Client.Do` — this is where
    the **classified retry** lives (below), so it fixes retry behavior for *all* callers
    (search, sync, listing), not just bulk-ops;
  - `ListEvents` / incremental list, `CreateEvent`, `UpdateEvent`, `DeleteEvent`;
  - `ListEventsByProperty(ctx, calID, opts)` where `opts` carries private-property
    filters **and** `timeMin`/`timeMax`/`singleEvents` — so it subsumes all three of
    today's `ListPlaceholders`, `ListAllPlaceholders`, and `ListPlaceholdersInRange`,
    and M7's time-windowed heatmap read reuses it. (Resolve in this step the
    contradiction between `server.go:392` "property + time window don't combine
    reliably" and `ListPlaceholdersInRange` depending on exactly that; if they do
    combine, `syncOnly` search should pass the window instead of paging all
    placeholders and filtering in Go.)
  - the **bulk-ops** helper (6.4).
- **Classified retry (in `Client.Do`):** parse the JSON error `reason`; retry only
  `rateLimitExceeded` / `userRateLimitExceeded` / `quotaExceeded` / `backendError` /
  429 / 5xx (honor `Retry-After`); **fail fast on other 4xx**. This replaces
  `doGCalRequestRaw`'s blanket 403-retry (5×/30s), which otherwise hangs on a permanent
  403 (read-only calendar) everywhere it's used.
- **Stays with sync:** `BuildPlaceholder`, `IsPlaceholder`, `SourceEventID`, the marker
  constant, extended-property handling, and `BatchDeleteEvents` + its three call sites
  (unchanged). `BatchDeleteEvents`/`doBatchDelete` call `Client.Do` and take the
  configurable `BaseURL` too, so M8 doesn't inherit an untestable sync delete path.
  Rerouting sync onto bulk-ops (which changes sync-log counts + mid-pass concurrency)
  is deferred to M8.

Note: 410 Gone is already meaningful here as an expired sync token
(`ErrSyncTokenExpired`); the idempotent-delete mapping below is scoped to the delete
path only, not `Client.Do`.

### 6.3 Extract Tools into `internal/tools`

Move `SearchEvents` and `BulkDeleteEvents` plus the `/tools` page/JS into
`internal/tools`, with its own `RegisterRoutes`/`RegisterPages`. Tools owns **no**
collections. For the `syncOnly` filter it depends on **sync's exported function**
`sync.ListSyncPlaceholders(ctx, client, calID)` (exporting a function, not the marker
constant, keeps sync's schema private per DESIGN §4 rule 3). Behavior preserved (UC-0074).

### 6.4 Fix bulk delete execution model

**Server — bounded, rate-limited, individual deletes** (primary path, not batch): one
`DELETE` per event → one unambiguous result, so counts are exact by construction
(UC-0073) with no multipart parser. Controls:

- **Concurrency bound** (~4–8 in flight) **and a shared request-rate limiter**
  (token bucket, start ~5–10 req/s) — concurrency alone doesn't cap sustained rate, and
  500 individual deletes will hit per-user write limits without it.
- **Per-request server deadline** (~30s) and a **partial-result contract**:
  `{ deleted, failed, unprocessed: [ids] }`. The handler returns what it finished; it
  never open-endedly blocks. Tune the numeric values against a real 500+ run.
- **Idempotent success:** 410 Gone (and 404) count as *success* (double-click / re-run).
- **Token provider, not token string:** bulk-ops takes `func(ctx) (string, error)`;
  on a 401 it refreshes once and retries the item before counting it failed. This
  requires the token fix below.

**Token fix (NFR-05, now a deliverable):** `getAccessToken` (`server.go:25`) currently
returns the *expired* token with `nil` error on refresh failure, and never persists the
refreshed access token — so every chunk re-refreshes and a failed refresh becomes a
misleading "N failed". Change it to (a) return an error on refresh failure, and
(b) persist the refreshed access token to the session (mirror how `TriggerSync`
persists the refresh token, or confirm the appbase session write-back API).

**Client — chunked with progress:** the browser sends deletions in **sequential**
chunks (~50–100), re-queuing any `unprocessed` ids from a chunk's partial result,
driving a progress bar (UC-0072, NFR-06). **Abort after 2 consecutive whole-chunk
failures** and surface the server's error text (so a permanent 403 stops fast, not
after all ~10 chunks). The definitive re-search runs **after the last chunk**
(replacing `setTimeout(searchEvents, 1000)`, which races read-after-write consistency).

### 6.5 Collection-namespacing convention

Rule: **every collection carries its owning module's prefix; add it where missing.**
Documented in M6; Tools introduces no collections. The current→target mapping for sync
(executed in M8 against live data):

| Current | Target |
|---|---|
| `sync_configs` | `sync_configs` (already prefixed) |
| `sync_logs` | `sync_logs` (already prefixed) |
| `source_calendars` | `sync_source_calendars` |
| `synced_events` | `sync_synced_events` |

NFR-07 is *documented* in M6 and first *exercised* when a module owns namespaced
collections (M7/M8).

## Data Model Changes

None. Tools is data-free; sync's schema and collections are untouched in M6.

## Build Order

1. **6.2** — `platform/calendar` `Client` (configurable `BaseURL`, `Client.Do` with
   classified retry, `ListEventsByProperty`); update sync + server call sites; sync
   stays on `BatchDeleteEvents`. `./dev ci` green, no behavior change beyond retry
   classification (verify sync tests).
2. **6.1** — `platform.Deps` + `RegisterRoutes`/`RegisterPages` wiring in `main.go`.
3. **6.3** — move Tools into `internal/tools`; add `sync.ListSyncPlaceholders`.
4. **Token fix (6.4)** — `getAccessToken` persistence + error-on-failure; token provider.
5. **6.4** — bulk-ops (concurrency + rate limit + deadline + partial results + idempotent
   410), server switch, then client-chunked progress UI.
6. **6.5** — document the namespacing convention.

Each step compiles and is testable alone; no big-bang integration.

## Testing

- **Prerequisite spike (gates the e2e plan):** confirm whether `AUTH_MODE=dev`
  (`e2e/03-config.sh`) seeds a Google **access token**. Tools handlers call
  `getAccessToken`, which 403s when `appbase.AccessToken(r)` is empty. If dev mode seeds
  none, add an env-injected fake access token to the dev-auth path **before** relying on
  tools e2e. (If this needs new tooling/appbase changes, raise it explicitly.)
- **e2e via injectable `BaseURL` → local fake-Google stub:** covers the **server**
  contract — search (UC-0070/0071), 500+ deletes (UC-0072 server side: chunking, exact
  counts, 410-as-success, partial results), classified retry, rate limiting.
- **Honest UC-0072 scope:** the *browser* chunk loop + progress bar is **not** covered
  by CLI e2e and stays manual, unless we add a headless-browser test — a tooling
  decision to raise explicitly, not assume.
- **Unit:** `platform/calendar` bulk-ops with a stub HTTP client — concurrency/rate
  bounds, retry classification (retry 429/5xx, fail-fast permanent 403), idempotent 410,
  partial-result shape, exact totals.
- **e2e (unauthenticated):** `/api/tools/*` exist and reject unauthenticated requests;
  sync/config endpoints unchanged.

## Risks & validation

- **`gcal.go` → `Client` refactor (6.2)** touches every generic call site (not
  signature-preserving). Mitigation: compiler + `./dev ci` (incl. sync unit tests);
  the only intended behavior change is retry classification — verify it doesn't break a
  sync path that (wrongly) relied on 403-retry.
- **Batch-endpoint note (correction):** the code uses the still-supported API-specific
  `…/batch/calendar/v3`, not the retired global `/batch`; today's miscount is a parsing
  bug. Moot for Tools (moves to individual deletes); relevant only to sync's retained
  `BatchDeleteEvents`.
- **Rate/deadline tuning** — start ~5–10 req/s, ~30s deadline, ~50–100 chunk; tune
  against a real 500+ delete.
- **Known sync defect for M8 (out of M6 scope):** at all three sync delete sites the
  `SyncedEvent` record is removed *regardless* of Google-delete success
  (`sync.go` ~365/591/636), orphaning placeholders. Recorded so M8 fixes it, not inherits it.
