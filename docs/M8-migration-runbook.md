# M8 Migration Runbook

Operator steps for the Phase 2 data migration: copy `synced_events` →
`sync_synced_events` (re-keyed by `SyncedEventKey`) and `source_calendars` →
`sync_source_calendars`, then cut the app over to the new collections.

The migration is **non-destructive**: `copy` only writes the new collections, so the old
data is the rollback until `delete-old` runs (which refuses if the new collection is
empty, guarding against a copy-never-ran wipe). Recovery of last resort is cheap for this
single-user app: delete the hub
calendar and use the Tools page's bulk delete with the `calendarSyncMarker` property
selector to remove placeholders, then reconfigure sync from scratch.

## The `migrate` CLI

`calendar-sync migrate dry-run | copy | verify | delete-old`. It runs against whatever
store the environment points at:

- **SQLite (dev):** `STORE_TYPE=sqlite SQLITE_DB_PATH=… ./calendar-sync migrate dry-run`
- **Firestore emulator (rehearsal):** `FIRESTORE_EMULATOR_HOST=localhost:8080
  STORE_TYPE=firestore GOOGLE_CLOUD_PROJECT=demo ./calendar-sync migrate dry-run`
- **Prod Firestore:** `STORE_TYPE=firestore GOOGLE_CLOUD_PROJECT=xwind-calendar-sync
  ./calendar-sync migrate …` — needs `gcloud auth application-default login` first, and
  runs **outside** the nono sandbox (like `./dev deploy`, it needs GCP credentials the
  sandbox blocks).

## Optional: rehearse on the Firestore emulator

Low stakes here (single user, ~1.3k records, easy recovery), so this is optional — the
migration logic is unit-tested. Its value is confirming Firestore-specific behaviour
(doc-id validity of the hex keys, the 500-op commit limit, collection delete):

1. `gcloud emulators firestore start --host-port=localhost:8080`
2. Seed it (import a prod export, or just point a dev app instance at the emulator and
   run a sync to populate the old-named collections).
3. `FIRESTORE_EMULATOR_HOST=localhost:8080 STORE_TYPE=firestore GOOGLE_CLOUD_PROJECT=demo
   ./calendar-sync migrate dry-run` → `copy` → `verify`.

## Prod migration sequence

The only ordering that matters: **`copy` must finish before the read-switch build
deploys**, and no sync should write during that window.

1. **Pause background sync** so nothing writes the old collections mid-copy:
   `gcloud scheduler jobs pause calendar-sync-nudge --location=<region>
   --project=xwind-calendar-sync`, and don't click "Sync now" until step 6.
2. **Dry-run** to see the shape and any collisions (expect none for a single clean user):
   `… migrate dry-run`. Collisions are printed as orphaned-placeholder warnings to review.
3. **Copy:** `… migrate copy`. Idempotent — safe to re-run if it's interrupted.
4. **Verify:** `… migrate verify`. Must print `verify: OK`. (It checks key-set equality,
   per-doc `TargetEventID`, and the source-calendar id-set — not that each placeholder
   still resolves on Google.)
5. **Deploy the read-switch build** (the commit that points `Store` at the `sync_*`
   collections): `./dev deploy`. The app now reads/writes the new, re-keyed collections.
6. **Resume sync:** `gcloud scheduler jobs resume calendar-sync-nudge …`. Open the app,
   click "Sync now", confirm placeholders are intact and the `firestore_reads~=` log line
   looks sane. On the *synced-events* side the app self-heals even if `copy` were skipped
   — it re-adopts existing placeholders by extended property. **Source calendars do NOT
   self-heal:** an empty `sync_source_calendars` means `GetSources` returns nothing and
   sync is a no-op; you'd have to re-select every source in the UI (losing emoji/color and
   sync tokens).
7. **Delete old (days later)**, once the new collections are confirmed healthy:
   `… migrate delete-old --yes`. It refuses only if the new collection is empty (copy
   never ran); it does NOT re-verify key-set equality, because by now the live app has
   legitimately diverged the new collection from the old. Eyeball the `dry-run` counts
   before running it.

> **Never run `migrate copy` after step 5 (the read-switch deploy).** It upserts the old
> `SourceCalendar` rows under their original ids into `sync_source_calendars`; if you had
> meanwhile re-selected sources in the UI (which mints new uuid rows), each calendar now
> has two rows and `ReconcileSources` never removes them — every sync then processes each
> source twice. `copy` belongs only in the paused pre-cutover window.

## Rollback

- **Before step 5** (read-switch deploy): nothing to undo — the app is still on the old
  collections; just resume sync.
- **After step 5, before `delete-old`:** redeploy the previous (pre-read-switch) build to
  point back at the old collections, which are untouched. Any mappings created on the new
  collections since cutover are lost, but the app re-adopts placeholders on the next sync.
- **Last resort:** delete the hub calendar + bulk-delete placeholders via the Tools
  `calendarSyncMarker` selector, then reconfigure sync.
