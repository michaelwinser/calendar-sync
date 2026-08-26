# M8 Implementation Plan

## Goal

The last planned milestone, and the biggest: finish the modularization by moving sync
into `internal/sync`, migrate its Firestore data to a module-owned namespace **and a
point-lookup key**, and use that key to make sync **two-tier** — a cheap incremental
fast pass plus a periodic full reconciliation. This is the real fix for the Firestore
read cost (see the "Data architecture" discussion; today every 15-minute pass scans the
full `synced_events` collection ~3×, ≈3,848 reads regardless of activity).

Highest-risk milestone: it changes **live production data**. It runs on a staging GCP
project first, with a non-destructive migration and a rollback path.

## Background (why the reads are high today)

Firestore holds four collections; only `synced_events` is large (~3N ≈ 1,280 records =
real events × calendars, over `SyncWindowWeeks`). Each pass scans it ~3× (inbound
per-source map, outbound user-wide map, cleanup). Google-side fetching is *already*
incremental (per-calendar sync tokens → only changed events); the waste is that the
Firestore side reconciles by full scan even for a handful of changes. Records are keyed
by random UUID and found by `Where(...).All()`, so a changed event's record can't be
point-fetched — which is what makes incremental impossible today.

## Phases (each a reviewable PR; land structural before data before behavior)

### Phase 1 — Modularize sync into `internal/sync` (structural, behavior-preserving)
Move sync + config + store + placeholder logic (`sync.go`, `server.go` config/sync/nudge
handlers, `store.go`, `gcal.go`'s placeholder helpers, `pages.go` home page, `module.go`)
out of `internal/app` into `internal/sync`, exposing the same `RegisterRoutes`/`RegisterPages`.
Retire `internal/app`. **Promote `/api/calendars`** out to a shared owner (a tiny
`internal/calendars` module, or a platform-registered route) — the coupling M7 recorded;
tools and heatmap repoint to it. No data or endpoint changes.

### Phase 2 — Collection migration: namespace + point-lookup key (the risky data step)
Two changes to `synced_events` in one migration:
1. **Namespace** per M6 §6.5: `source_calendars` → `sync_source_calendars`,
   `synced_events` → `sync_synced_events` (the `sync_`-prefixed rows already keep theirs).
2. **Deterministic key** so records are point-fetchable: the doc id becomes a hash of
   `(userID, sourceCalID, sourceEventID, targetCalID)` instead of a UUID. A changed
   event's record is then `Get(key)` (1 read), not a collection scan.

Firestore has no native rename, so this is a **copy-then-cut-over** migration (a CLI
command, run against staging then prod): copy each old doc to the new collection under
its new key, verify counts, switch reads to the new collection, then delete the old.
Non-destructive until the final delete; **snapshot/export Firestore first**; rollback =
point reads back at the old collection. Open decision: run it as a one-shot CLI vs. a
guarded startup migration.

### Phase 3 — Two-tier sync (the headline / cost fix)
- **Fast pass (every interval):** per source, `ListEventsIncremental(syncToken)` → only
  changed events (incl. cancellations). For each, **point-get its record by key**,
  create/update/delete the placeholder + record; propagate the change outbound to the
  other calendars by point-getting/writing those records. No full scan, no cleanup.
  Reads ≈ **O(changes)** — near-zero when idle.
- **Full pass (e.g. daily):** today's complete sync — full outbound reconciliation
  (catches manual placeholder edits and anything the fast path missed) + cleanup of
  orphans and past events. Reads ≈ 3N, **once a day**. This is the safety net that made
  the earlier "skip when idle" optimization safe to *not* do before.
- **Scheduling:** add `LastFullSyncAt` to the config; the nudge runs a full pass when it's
  older than the full-pass interval, otherwise a fast pass. (Alternative: two cron jobs.)

Effect: `96 × 3,848` reads/day → `96 × O(changes) + 1 × 3,848` — ~90× less for a quiet
calendar, and a snappier fast path.

### Phase 4 — Fix carried-forward sync defects
The `SyncedEvent` store record is currently deleted regardless of whether the Google
placeholder delete succeeded (`sync.go` cleanup/orphan paths), orphaning placeholders.
Delete the record only when the Google delete succeeded or the event was already gone
(404/410). Fold in while the sync code is being restructured.

## Data Model Changes
- `synced_events` → `sync_synced_events`, re-keyed by `(userID, sourceCalID, sourceEventID, targetCalID)` hash.
- `source_calendars` → `sync_source_calendars`.
- `sync_configs` gains `LastFullSyncAt`.

## Testing
- Phase 1: `./dev ci` green with no behavior change (the sync unit/e2e suite is the guard);
  endpoints and pages unchanged.
- Phase 2: a migration unit test (old docs → new keys, idempotent re-run, count parity)
  against SQLite; **dry-run + verify on staging** before prod.
- Phase 3: unit tests for the fast path (a changed event reads only its own record; a
  cancellation deletes; idle reads ≈ 0) and the full path (reconciliation still correct);
  the read-count instrumentation confirms the drop.
- Phase 4: a test that a failed Google delete leaves the record intact.

## Risks & sequencing
- **Live data (highest risk).** Do Phase 2 on a **staging GCP project + Firestore** first
  (the deployment we said M8 would need); snapshot/export prod before cut-over; keep the
  old collection until the new one is verified in prod.
- **Incremental correctness.** The fast path must not silently drop unchanged records
  (the reason a naive incremental was avoided). The daily full pass is the backstop;
  the fast path must be conservative (when in doubt, defer to the full pass).
- **Sequencing:** Phase 1 (structural, safe) → Phase 2 (data, staged) → Phase 3 (behavior,
  depends on the key) → Phase 4 (fold-in). Do not combine the data migration with the
  behavior change in one deploy.

## Deferred / out of scope
- Moving the mapping off Firestore entirely (derive from hub placeholders) — a more
  radical alternative to two-tier that eliminates the collection; recorded as a future
  option, not M8.
