# M8 Implementation Plan

## Goal

The last planned milestone, and the biggest: move sync into `internal/sync`, migrate
its Firestore data to a module-owned namespace **and a point-lookup key**, and use that
key to make sync **two-tier** — a cheap incremental fast pass plus a periodic full
reconciliation. This is the real fix for the Firestore read cost (today every
15-minute pass scans the full `synced_events` collection ~3×, ≈3,848 reads regardless
of activity).

Highest-risk milestone: it changes **live production data**. It runs on a staging GCP
project first, with a non-destructive, idempotent migration and a defined rollback
window.

> Status: architecture-reviewed once → **REVISE**; this revision folds in all 7
> BLOCKING items and the advisories. Re-run the architecture review before building.

## Background (why the reads are high)

Four collections; only `synced_events` is large (~3N ≈ 1,280 = real events × calendars ×
`SyncWindowWeeks`). Each pass scans it ~3×. Google-side fetching is already incremental
(per-calendar sync tokens); the waste is the Firestore side reconciling by full scan.
Records are UUID-keyed and found by `Where(...).All()`, so a changed event's record
can't be point-fetched — the blocker to incremental.

## Phase 0 — De-risk (do first)

- **Validate appbase store assumptions** (load-bearing for Phases 2–3; none confirmed):
  (a) `Collection.Create` preserves a caller-supplied pk as the Firestore doc id;
  (b) a genuine point `Get(id)` exists — `UpdateSourceSyncToken` uses
  `Where("id","==",id).First()`, hinting it may not; if absent, **add it to appbase first**;
  (c) adding `LastFullSyncAt` migrates the SQLite schema for existing local/TrueNAS DBs.
- **Build the fake-Google test harness** (the seam exists: `calendar.Client.BaseURL`/
  `BatchURL`): in-memory calendars, event CRUD, `privateExtendedProperty` filtering, and
  **sync tokens with a replayable delta log**. Everything in Phases 2–3 is tested through
  this; `Store.Reads()` asserts the read counts. `./dev ci` has no behavioural sync
  coverage today, so this is prerequisite, not optional.

## Phase 1 — Modularize sync into `internal/sync` (structural, behaviour-preserving)

Move sync + config + store + placeholder logic out of `internal/app` into `internal/sync`
(same `RegisterRoutes`/`RegisterPages`); retire `internal/app`. **Promote `/api/calendars`**
to a shared owner (a tiny `internal/calendars` module) — the coupling M7 recorded; tools
and heatmap repoint. Reconcile the nudge auth discrepancy (code says unauthenticated;
UC-0051 says OIDC/`X-Nudge-Key`) — this is the last milestone, so pick one. No data or
endpoint changes.

## Phase 2 — Collection migration: namespace + point-lookup key (staged, non-destructive)

Two changes to `synced_events` in one migration: rename to `sync_synced_events`
(and `source_calendars` → `sync_source_calendars`), and re-key by a deterministic hash of
`(userID, sourceCalID, sourceEventID, targetCalID)` so a record is `Get(key)` (1 read).

**A one-shot CLI, not a startup migration** (a startup migration runs concurrently on
every cold-start instance during the riskiest deploy). Subcommands: `--dry-run`, `copy`,
`verify`, `delete-old` — the destructive step a distinct invocation days later.

**Write-freeze runbook (BLOCKING 1 — the collection is written live):** pause the Cloud
Scheduler nudge job **and** gate `/api/sync` + `/sync/nudge` behind a `SYNC_PAUSED` config
so a manual trigger can't slip in → `copy` → deploy the read-switch → unpause. A paused
interval of a few minutes is invisible. (Alternative if pausing is unacceptable: a
dual-write release → copy → read-new release → stop dual-write.)

**Collisions are real and must not be silent (BLOCKING 2):** UUID keys allow >1 record
per 4-tuple (the adopt branches create records without checking; the running-sync guard
is advisory, not a lock). Under the deterministic key those collide and the loser's
`TargetEventID` — a real placeholder — is lost.
- `--dry-run` reports every 4-tuple with >1 doc and their distinct `TargetEventID`s.
- Collision policy: keep newest `UpdatedAt`; for each loser whose `TargetEventID` differs,
  delete that Google placeholder (or list for review) — never drop the row silently.
- **Verify by key-set equality + a spot-check that each new doc's `TargetEventID` still
  resolves on Google** — not by `len(old)==len(new)`.
- Idempotent by construction (deterministic id + upsert); test a re-run after partial failure.

**Rollback (ADVISORY 10):** symmetric only before the first post-cutover sync write.
State the window; after it, recovery is a reverse copy, not a config flip. Snapshot with
**PITR enabled + `--snapshot-time`** (a default `firestore export` is not consistent).
Keep the old collection ≥ 2 full-pass cycles; delete it with M6's bounded/rate-limited
delete (Firestore has no drop-collection; 500 ops/commit).

## Phase 3 — Two-tier sync (the headline / cost fix)

**A single comparison key both tiers compute from the same data (BLOCKING 4).** Today
`SyncedEvent.SourceUpdated` means the source event's `Updated` inbound but the *placeholder's*
`Updated` outbound — so the two tiers would disagree and rewrite everything each full pass.
Stamp `sourceUpdated` into the placeholder's **private extended properties** alongside
`sourceCalendarId`/`sourceEventId`; both tiers compare that value, and the record field
becomes a cache, not the truth. Also build outbound placeholders from the same source data
in both tiers (today outbound builds from the hub copy, so `ColorID=="source"` resolves
differently). Convergence test: fast pass then full pass on unchanged state → **zero writes**.

**Fast pass (every interval):** per source, `ListEventsIncremental` — **with
`singleEvents=true`** (BLOCKING 5: the token request must match the initial request or
Google returns recurring masters / 400, breaking UC-0027) — and **apply the
`timeMin`/`timeMax` window client-side** (a token stream is unbounded in time). For each
changed event, point-get its record and create/update/delete placeholder + record;
propagate outbound to other calendars by point-getting/writing those records. Reads ≈
**O(changes)**. Persist the new sync token for a source **only if every change for it
applied cleanly** — else keep the old token so the next pass re-reads the delta.
Transition table (the delta reports the event, not the reason — each needs an explicit
rule, since the fast path has no "unmatched remainder"):

| Source event now… | Fast-path action |
|---|---|
| created / changed (in window, busy) | upsert placeholder + record |
| cancelled | delete placeholder + record |
| declined / `workingLocation` / transparent | delete placeholder + record |
| moved outside `[now, now+window]` | delete placeholder + record |
| source has empty/expired sync token | full list for **that source only** |

**Full pass (≤ once/day/user):** the reconciliation backstop — but note today's full pass
**also uses the source sync token** (BLOCKING 3), so it never re-reads sources and can't
be the backstop the fast path relies on. The full pass must **clear `SourceCalendar.SyncToken`
and do a tokenless `ListEvents(now, now+window)` per source**, re-establishing the token —
which also fixes the window never sliding (a latent bug against UC-0031/0046). Then full
outbound reconciliation + cleanup of orphans/past events.

**Full-pass-only invariants (BLOCKING 6 — the spec of what a day of staleness costs):**
removed source cleanup, **old-hub cleanup (UC-0060 — confirm it exists in the sync path at
all; it appears matched by neither `cleanupRemovedSources` nor `syncOutboundToSource`)**,
past-event cleanup, manually edited/deleted placeholders, window slide. Config changes that
touch these must **force a full pass on the next run** — clear `LastFullSyncAt` (or set
`FullSyncRequested`) in `PutConfig`; the manual "Sync now" button also runs a full pass
(its purpose is repair).

**Scheduling:** `sync_configs` gains `LastFullSyncAt`; the nudge runs a full pass when it's
stale, else a fast pass. **Stagger the full-pass due time per user** (hash `userID` → hour
offset) or give it its own scheduled endpoint — else one nudge request carries N full
passes and risks the NFR-01 timeout.

## Phase 4 — Fix carried-forward defects

The `SyncedEvent` record is deleted regardless of whether the Google placeholder delete
succeeded, orphaning placeholders. Delete the record only when the Google delete succeeded
or the event was already gone (404/410). Fold in during the restructure.

## Data Model Changes
- `synced_events` → `sync_synced_events`, re-keyed by `(userID, sourceCalID, sourceEventID, targetCalID)`.
- `source_calendars` → `sync_source_calendars`.
- `sync_configs` gains `LastFullSyncAt`.
- `SyncLog` gains `kind` (`fast`/`full`); a fast pass that made no changes writes no durable
  row (heartbeat via `LastSyncAt`) — else 96 fast passes/day evict history in ~2 days.

## Testing
- **Phase 0/3 harness:** scripted scenarios against the fake Google — create → fast pass →
  edit → fast pass → assert reads via `Store.Reads()`; delete a placeholder by hand → full
  pass repairs it → fast pass makes **zero** writes; recurring series (edit one instance,
  edit the series, cancel one instance). Assert the honest read floor (~5–10/idle pass), not zero.
- **Phase 2:** dry-run + verify against **staging Firestore seeded from a prod export**
  (SQLite won't exercise doc-id constraints, the 500-op commit limit, or collection delete);
  idempotent re-run; collision handling.
- **Phase 1/4:** `./dev ci` green, no behaviour change; a test that a failed Google delete
  leaves the record intact.

## Risks & sequencing
Phase 0 (de-risk) → Phase 1 (structural, safe) → Phase 2 (data, staged, non-destructive) →
Phase 3 (behaviour, needs the key + harness) → Phase 4 (fold-in). **Never combine the data
migration and the behaviour change in one deploy.** Do Phase 2 against a staging GCP
project + Firestore first; the read-count instrumentation confirms Phase 3's drop.

## Deferred / out of scope
- Moving the mapping off Firestore entirely (derive from hub placeholders) — a more radical
  alternative to two-tier that eliminates the collection; recorded as a future option.
