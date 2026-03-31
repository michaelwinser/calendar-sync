# Calendar Sync - Mission, Vision, Strategy, Roadmap

## Mission

Allow users to have a consistent and correct calendar across multiple calendars.

Users often have multiple calendars from different work and personal contexts. While one can try to consolidate everything onto a single calendar, this is inconvenient and has negative consequences:

- **Privacy**: work colleagues shouldn't see personal appointment details and vice versa
- **Loss of information**: when a work relationship ends, calendar data tied to that account may be lost
- **Fragmented visibility**: no single view of all commitments, leading to double-bookings

## Vision

Users can select multiple calendars for synchronization. The app automatically creates placeholder events on each calendar so that free/busy status is clear and the user can see their appointments no matter which calendar they are currently using.

## Strategy

### Goals

- Open source, self-hosted application
- Runs on localhost, TrueNAS, or Cloud Run
- Google Calendar support (other providers are out of scope for now)
- Synchronization can be manually invoked or triggered via an endpoint

### Non-Goals

- Commercial distribution
- Support for non-Google calendar providers (initially)

### Technical Approach

Rather than doing pairwise sync across all selected calendars (which scales as O(n^2)), we use a dedicated **hub calendar** model:

- A single hub calendar acts as the central source of truth for cross-calendar visibility
- Events from each selected calendar are synced **to** the hub
- The hub then syncs placeholder events **out** to each selected calendar
- This reduces sync complexity to O(n) and provides a single place to see all commitments

**Hub calendar**: The user can either pick an existing Google Calendar as the hub or create a new one themselves. (The app does not create calendars on the user's behalf to avoid requiring elevated OAuth scopes.)

**Placeholder content**: Placeholder events initially carry the same information as the source event — title, description, video chat links, location, and attachments — and reflect the original free/busy status. Reminders are always turned off on placeholders. Per-calendar control over what fields are copied is a future enhancement.

### Implementation

- Built on [appbase](https://github.com/michaelwinser/appbase) (Go, chi router, SQLite, Google OAuth)
- Light pure HTML+CSS frontend with minimal JavaScript (no SPA framework)

### Development Process

**Use cases and e2e testing**:
- The PRD enumerates numbered use cases (e.g. UC-0001, UC-0002) that serve as the specification for end-to-end tests
- The e2e test suite exercises each use case via the CLI (using appbase's CLI client pattern)
- `./dev ci` runs the full e2e suite; all use cases marked as "completed" for the current milestone must pass
- Use cases that are not yet implemented may be skipped, but implemented ones must not regress

**Pre-commit code review**:
- Before each commit, a code review agent is invoked on the staged changes
- The review must pass before the commit proceeds
- See `docs/AGENTS.md` for the agent prompt

**Milestone planning**:
- Each milestone begins with an implementation plan document (`docs/M{n}-plan.md`)
- An architecture review agent reviews the plan before implementation begins
- See `docs/AGENTS.md` for the agent prompt

## Roadmap

> This roadmap is illustrative, not the formal engineering or product plan. It shows a reasonable progression of capabilities and outcomes towards the end goal.

| Milestone | Description |
|-----------|-------------|
| **M0** | PRD and DESIGN docs |
| **M1** | Basic app that can login via a Google Account and get Google Calendar scopes. Dev, Staging, and Prod environments setup. |
| **M2** | User can select multiple calendars for synchronization and create/manage a hub calendar |
| **M3** | One-way sync from each selected calendar to the hub calendar. Sync handles creation, update, and deletion of events. |
| **M4** | Two-way sync from the hub to each calendar. Sync correctly propagates creation, update, and deletion of events from the original calendar to the hub and then onwards to all calendars. |
| **M5** | Polish and enhancements as determined during development. |
