# Calendar Platform — Modular Architecture Design

Status: **Draft** · Supersedes nothing; complements [DESIGN.md](DESIGN.md) (sync
internals), [MVSR.md](MVSR.md) (strategy/roadmap), [PRD.md](PRD.md) (use cases).

## 1. Motivation

`calendar-sync` began as a single-purpose app (hub-model Google Calendar sync).
It has since accreted a second, unrelated-but-useful capability — a **Tools** page
for bulk search/delete of events — and we want to add more calendar utilities
(starting with the **heatmap**, today a separate Apps Script web app). These tools
share almost nothing at the feature level but share *everything expensive*: one
Google OAuth client, one authenticated session, one Calendar API client, one
deployment. The app is becoming a **calendar workbench**, and its structure should
say so — without letting the tools trip over each other or over sync.

## 2. Guiding principle

**The architectural boundary is the authenticated calendar substrate, not the
feature.** Auth + session + Calendar access is costly to duplicate (consent screen,
token handling, hosting) and cheap to share. Features are the opposite. So we
consolidate behind one auth substrate and separate features into *modules within
one app*, rather than into separate apps (which would re-pay the substrate tax) or
one undifferentiated package (which lets modules entangle).

Target: a **modular monolith**.

## 3. Target architecture

One Go app (appbase: chi router, OAuth, store) hosting cleanly separated modules
over shared infrastructure.

```
cmd / main.go                 wiring: build shared infra, mount each module
internal/
  platform/                   shared infrastructure (no feature logic)
    calendar/                 Calendar API client + bulk-ops (batch, concurrency, progress)
    <auth/session from appbase>
  sync/                       hub-model sync (background nudge + its data)
  tools/                      bulk search/delete (and future bulk operations)
  heatmap/                    read-only weekly-occupancy heatmap
```

Each **module** is a package that:

- exposes `RegisterRoutes(r chi.Router)` (or equivalent) that `main` mounts under
  a namespaced path prefix (`/api/tools/...`, `/api/heatmap/...`, `/api/sync/...`);
- **owns its own store collections** and declares them from the shared `*db.DB`;
- contributes its own nav entry / page;
- depends only on **shared infrastructure** (auth/session, the calendar client,
  the `*db.DB` handle) and, when it genuinely must, on another module's **exported
  API** — never another module's routes, handlers, or raw collections.

This replaces today's single `internal/app` package with one `Server` god-struct
and one `Store` owning every collection.

## 4. Data / DB namespacing rules

The cost work taught us that shared, undifferentiated data access is where things
silently go wrong. Rules:

1. **The `*db.DB` (connection) is shared infrastructure; collections are owned by
   the module that defines them.** Retire the single all-tables `Store`.
2. **Collection names are module-prefixed** — `sync_*` (`sync_configs`,
   `sync_source_calendars`, `sync_synced_events`, `sync_sync_logs`), `tools_*`,
   etc. The name is the namespace in both backends (Firestore top-level
   collections, SQLite tables), so the prefix makes ownership greppable and
   collisions structurally unlikely.
3. **Cross-module data access goes through the owning module's exported API, never
   its raw collections.** When `tools` eventually cleans up sync-error placeholders
   it calls a function `sync` exports, so `sync` can evolve its schema without
   silently breaking `tools`.
4. **Cross-cutting identity** (user, session) comes from auth, not any module store.

Heatmap notably owns **no** collections — it is pure Calendar-API read.

## 5. Shared calendar + bulk-ops layer

Both the heatmap (bulk *read*) and bulk-delete (bulk *write*) are instances of the
same shape: **per-item, network-bound Calendar operations of unbounded size.** They
need a common home so the plumbing isn't reinvented per tool.

`internal/platform/calendar` provides:

- the authenticated Calendar client (list with field masks + pagination — already
  the efficient pattern in today's `gcal.go`);
- **bulk operations** with batching and/or **bounded concurrency**, context
  cancellation, and rate-limit backoff;
- a **progress-reporting** contract so callers can surface "N of M done".

Principle to encode: *a bulk calendar operation needs (a) efficient API use (batch
or bounded concurrency) and (b) an execution model that survives past one request
timeout, with visible progress.*

## 6. Bulk operations execution model

Today `BulkDeleteEvents` runs Google batch requests **sequentially and
synchronously inside one HTTP handler** — 500 events = ~10 serial batch round-trips,
which can exceed a browser/proxy timeout with no progress shown (the observed
"it hangs / silently times out"). Two changes:

- **Efficiency:** run batches with bounded concurrency (not serially); verify the
  batch endpoint is actually succeeding (the current multipart-response parsing is
  fragile) or fall back to bounded-concurrent individual deletes.
- **Execution model (chosen): client-chunked with progress.** The browser sends
  deletes in chunks (~50–100) and drives a progress bar; each request is short and
  can't time out, and the user sees advancement. For a single-user tool this beats a
  server-side async-job/poll system (no job state to manage). An async job stays a
  future option if an operation must run detached from any open tab.

## 7. Heatmap: Apps Script → module

Decision: **bring the heatmap into the app as `internal/heatmap`** and retire the
Apps Script version. Rationale: the Cloud Run pipeline (appconfig, `./dev deploy`,
CI, code review, real version control, local dev) is far more mature than Apps
Script's copy-paste workflow, and the incremental infra cost is ~zero because the
OAuth client, Cloud Run service, and calendar client already exist. The server side
becomes a thin handler over the shared calendar client's list; the client-side
aggregation + details panel port over as-is. This also removes the Go/JS
calendar-logic duplication. (What we give up — Apps Script running "as the user"
with no token management — is moot; the app already manages that token.)

## 8. Naming / rename — open decision

Consolidation makes `calendar-sync` a partial misnomer. Renaming to an umbrella
(e.g. `calendar-utilities`) is defensible but carries deployment churn: the OAuth
client, the `calendar-sync.winser.net` domain, appconfig, and the Cloud Run service
name. **Recommendation: defer.** Treat the name as cosmetic and decouple it from the
structural work; schedule a rename on its own if/when the misnomer bothers us. The
architecture does not depend on it.

## 9. Migration sequencing & risk

| Milestone | Scope | Risk |
|---|---|---|
| **M6** | Module foundation + `platform/calendar` bulk-ops layer; extract bulk-delete into `internal/tools`; fix its execution model (client-chunked progress + bounded concurrency). | **Low** — tools is *data-free* today (depends only on token + calendar helpers), so extraction can't corrupt data. Establishes the module/`RegisterRoutes`/namespacing pattern and ships a real UX fix. |
| **M7** | Heatmap as `internal/heatmap`; nav entry; retire Apps Script. | **Low–med** — read-only, no data; mostly porting + wiring. |
| **M8** | Move sync into `internal/sync`; migrate its collections to module-owned, prefixed names; retire the god-`Store`. | **High** — touches live production data + the background job. Needs an explicit data-migration plan (collection rename/copy) and a rollback path. Done last, deliberately. |

Order rationale: prove the pattern on the safe, data-free tool first; do the
data-carrying, background-service module last when the pattern is settled.

## 10. Non-goals

- Multi-tenant / multi-user productization (still a personal, single-user app).
- A general plugin system — modules are compiled in, not dynamically loaded.
- Splitting into multiple deployed apps (revisit only on a real forcing function:
  a different OAuth scope such as Gmail, a different audience, or a heavy/different
  runtime).
- Reworking the sync algorithm itself (covered by DESIGN.md; out of scope here).

## 11. Open decisions to confirm

1. Rename now vs. defer (§8) — recommendation: defer.
2. Collection-name migration for sync in M8: rename-in-place vs. copy-then-cut-over;
   acceptable downtime, if any.
3. Bulk-ops progress transport for the client-chunked model: sequential chunked
   POSTs driven by the browser (simplest) vs. server-sent progress.
4. Where the heatmap's page assets live (served template vs. embedded static).
