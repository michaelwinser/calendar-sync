package sync

import (
	"context"
	"testing"
	"time"

	"github.com/michaelwinser/calendar-sync/internal/calendartest"
	"github.com/michaelwinser/calendar-sync/internal/platform/calendar"
)

// TestFullSyncCreatesPlaceholderAndConverges is the first behavioural sync test: it
// drives the real RunSync path against the in-memory fake Google. A source event
// should produce a hub placeholder on the first pass, and an unchanged second pass
// should make no further writes (convergence) — the property Phase 3's two-tier work
// must preserve. It also exercises the Store's Firestore read counter.
func TestFullSyncCreatesPlaceholderAndConverges(t *testing.T) {
	const (
		userID = "u1"
		hubID  = "hub@x"
		srcID  = "work@x"
		token  = "tok"
	)
	ctx := context.Background()

	store := newTestStore(t)
	if _, err := store.SaveConfig(userID, SaveConfigInput{
		HubCalendarID: hubID, HubCalendarName: "Hub",
		SyncWindowWeeks: 8, SyncIntervalMinutes: 15,
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if _, err := store.ReconcileSources(userID, []SourceCalendarInput{
		{CalendarID: srcID, CalendarName: "Work"},
	}); err != nil {
		t.Fatalf("ReconcileSources: %v", err)
	}
	cfg, err := store.GetConfig(userID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	sources, err := store.GetSources(userID)
	if err != nil {
		t.Fatalf("GetSources: %v", err)
	}

	fake := calendartest.New()
	defer fake.Close()
	fake.AddCalendar(hubID, "Hub", false)
	fake.AddCalendar(srcID, "Work", true)
	// A real meeting on the source calendar, inside the sync window.
	start := time.Now().Add(48 * time.Hour).UTC()
	srcEvent := fake.SeedEvent(srcID, calendar.GCalEvent{
		Summary: "Team Standup",
		Start:   calendar.EventTime{DateTime: start.Format(time.RFC3339)},
		End:     calendar.EventTime{DateTime: start.Add(30 * time.Minute).Format(time.RFC3339)},
	})

	// First pass: should create a hub placeholder for the source event.
	res, err := RunSync(ctx, fake.Client(), token, store, cfg, sources)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if res.Errors != 0 {
		t.Fatalf("first pass had errors: %v", res.ErrorDetails)
	}
	if res.Created != 1 {
		t.Fatalf("first pass created %d placeholders, want 1", res.Created)
	}

	placeholders := placeholdersOn(fake, hubID)
	if len(placeholders) != 1 {
		t.Fatalf("want 1 hub placeholder, got %d", len(placeholders))
	}
	if got := SourceEventID(placeholders[0]); got != srcEvent {
		t.Fatalf("placeholder points at source %q, want %q", got, srcEvent)
	}

	// Second pass over unchanged state must converge with no writes. Reload config and
	// sources first, as production does per request (server.go).
	cfg, err = store.GetConfig(userID)
	if err != nil {
		t.Fatalf("reload GetConfig: %v", err)
	}
	sources, err = store.GetSources(userID)
	if err != nil {
		t.Fatalf("reload GetSources: %v", err)
	}

	fake.ResetCounts()
	readsBefore := store.Reads()
	res2, err := RunSync(ctx, fake.Client(), token, store, cfg, sources)
	if err != nil {
		t.Fatalf("second RunSync: %v", err)
	}
	if res2.Errors != 0 {
		t.Fatalf("second pass had errors: %v", res2.ErrorDetails)
	}
	if res2.Created != 0 || res2.Updated != 0 || res2.Deleted != 0 {
		t.Fatalf("second pass should converge with no writes, got created=%d updated=%d deleted=%d",
			res2.Created, res2.Updated, res2.Deleted)
	}
	if len(placeholdersOn(fake, hubID)) != 1 {
		t.Fatalf("second pass changed the hub placeholder set")
	}

	// Pass 1 bootstrapped the token (an unrestricted FullSyncList read); pass 2, now that
	// a token exists, reconciles with a cheap WINDOWED read per source and preserves the
	// token — no repeated unwindowed bootstrap, and never incremental (the fast pass's job).
	counts := fake.Counts()
	if counts.Incremental != 0 {
		t.Fatalf("full pass should not go incremental, counts=%+v", counts)
	}
	if counts.WindowList != len(sources) || counts.FullSyncList != 0 {
		t.Fatalf("a full pass with a token should do one windowed read per source, counts=%+v", counts)
	}
	if store.Reads() <= readsBefore {
		t.Fatalf("expected the full pass to read the mapping (reads went %d→%d)", readsBefore, store.Reads())
	}

	// The full pass established a sync token on each source (the fast pass will use it).
	for _, s := range sources {
		if s.SyncToken == "" {
			t.Fatalf("full pass should establish a sync token on source %s", s.CalendarID)
		}
	}
}

// TestFailedPlaceholderDeleteKeepsRecord is the Phase 4 regression: when the Google
// delete of an orphaned placeholder fails, the SyncedEvent record must survive so the
// next pass retries — otherwise the record is dropped and the placeholder is orphaned
// forever. When the delete later succeeds, the record is cleaned up.
func TestFailedPlaceholderDeleteKeepsRecord(t *testing.T) {
	const (
		userID = "u1"
		hubID  = "hub@x"
		srcID  = "work@x"
		token  = "tok"
	)
	ctx := context.Background()

	store := newTestStore(t)
	if _, err := store.SaveConfig(userID, SaveConfigInput{
		HubCalendarID: hubID, HubCalendarName: "Hub", SyncWindowWeeks: 8, SyncIntervalMinutes: 15,
	}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if _, err := store.ReconcileSources(userID, []SourceCalendarInput{{CalendarID: srcID, CalendarName: "Work"}}); err != nil {
		t.Fatalf("ReconcileSources: %v", err)
	}
	reload := func() (*SyncConfig, []SourceCalendar) {
		cfg, err := store.GetConfig(userID)
		if err != nil {
			t.Fatalf("GetConfig: %v", err)
		}
		sources, err := store.GetSources(userID)
		if err != nil {
			t.Fatalf("GetSources: %v", err)
		}
		return cfg, sources
	}

	fake := calendartest.New()
	defer fake.Close()
	fake.AddCalendar(hubID, "Hub", false)
	fake.AddCalendar(srcID, "Work", true)
	start := time.Now().Add(48 * time.Hour).UTC()
	srcEvent := fake.SeedEvent(srcID, calendar.GCalEvent{
		Summary: "Doomed",
		Start:   calendar.EventTime{DateTime: start.Format(time.RFC3339)},
		End:     calendar.EventTime{DateTime: start.Add(30 * time.Minute).Format(time.RFC3339)},
	})

	// Pass 1: create the placeholder.
	cfg, sources := reload()
	if _, err := RunSync(ctx, fake.Client(), token, store, cfg, sources); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	ph := placeholdersOn(fake, hubID)
	if len(ph) != 1 {
		t.Fatalf("want 1 placeholder after pass 1, got %d", len(ph))
	}
	placeholderID := ph[0].ID

	// Now the source event disappears (→ placeholder becomes an orphan to delete) and
	// that placeholder's delete is rigged to fail.
	if err := fake.Client().DeleteEvent(ctx, token, srcID, srcEvent); err != nil {
		t.Fatalf("deleting source event: %v", err)
	}
	fake.FailDelete(hubID, placeholderID)

	// Pass 2: orphan cleanup tries to delete the placeholder and fails.
	cfg, sources = reload()
	res2, err := RunSync(ctx, fake.Client(), token, store, cfg, sources)
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if res2.Errors == 0 {
		t.Fatalf("pass 2 should report the failed delete as an error")
	}
	if len(placeholdersOn(fake, hubID)) != 1 {
		t.Fatalf("placeholder should still exist after a failed delete")
	}
	// The mapping MUST survive so pass 3 retries — the regression this test guards.
	synced, err := store.GetSyncedEventsForUser(userID)
	if err != nil {
		t.Fatalf("GetSyncedEventsForUser: %v", err)
	}
	if len(synced) != 1 {
		t.Fatalf("failed delete must keep the SyncedEvent record, got %d records", len(synced))
	}

	// Pass 3: allow the delete to succeed → placeholder and record both cleaned up.
	fake.AllowDelete(hubID, placeholderID)
	cfg, sources = reload()
	if _, err := RunSync(ctx, fake.Client(), token, store, cfg, sources); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if got := placeholdersOn(fake, hubID); len(got) != 0 {
		t.Fatalf("placeholder should be gone after recovery, got %d", len(got))
	}
	synced, _ = store.GetSyncedEventsForUser(userID)
	if len(synced) != 0 {
		t.Fatalf("record should be cleaned up after successful delete, got %d", len(synced))
	}
}

// TestCreateSyncedEventPointGet covers the M8 read-switch: mappings are keyed by their
// deterministic 4-tuple hash and fetchable with a single point Get, and re-creating the
// same 4-tuple upserts rather than duplicating.
func TestCreateSyncedEventPointGet(t *testing.T) {
	store := newTestStore(t)
	se := &SyncedEvent{UserID: "u", SourceCalendarID: "a", SourceEventID: "e1", TargetCalendarID: "hub", TargetEventID: "p1"}
	if err := store.CreateSyncedEvent(se); err != nil {
		t.Fatal(err)
	}
	if want := SyncedEventKey("u", "a", "e1", "hub"); se.ID != want {
		t.Fatalf("ID = %q, want deterministic key %q", se.ID, want)
	}

	got, err := store.GetSyncedEventByKey("u", "a", "e1", "hub")
	if err != nil || got == nil {
		t.Fatalf("point-get: %v (got %v)", err, got)
	}
	if got.TargetEventID != "p1" {
		t.Fatalf("point-get returned wrong record: %q", got.TargetEventID)
	}

	// A missing key is (nil, nil), not an error.
	if miss, err := store.GetSyncedEventByKey("u", "a", "absent", "hub"); err != nil || miss != nil {
		t.Fatalf("missing key should be (nil,nil), got (%v,%v)", miss, err)
	}

	// An UPDATE on the same key replaces the mapping in place (the real flow's path for a
	// changed source event); it does not create a second row.
	got.TargetEventID = "p1-new"
	if err := store.UpdateSyncedEvent(got); err != nil {
		t.Fatal(err)
	}
	all, err := store.GetSyncedEventsForUser("u")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].TargetEventID != "p1-new" {
		t.Fatalf("update must replace in place, got %+v", all)
	}
}

// TestTwoSourceOutboundConvergence exercises outbound propagation and the BLOCKING-4
// comparison key: an event on source A becomes a hub placeholder and propagates to
// source B (not back to A), and a second unchanged pass writes nothing — the outbound
// placeholder is compared by the source's stamped Updated, not the hub placeholder's own.
func TestTwoSourceOutboundConvergence(t *testing.T) {
	const (
		userID = "u1"
		hubID  = "hub@x"
		aID    = "a@x"
		bID    = "b@x"
		token  = "tok"
	)
	ctx := context.Background()
	store := newTestStore(t)
	if _, err := store.SaveConfig(userID, SaveConfigInput{HubCalendarID: hubID, HubCalendarName: "Hub", SyncWindowWeeks: 8, SyncIntervalMinutes: 15}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileSources(userID, []SourceCalendarInput{{CalendarID: aID, CalendarName: "A"}, {CalendarID: bID, CalendarName: "B"}}); err != nil {
		t.Fatal(err)
	}
	reload := func() (*SyncConfig, []SourceCalendar) {
		cfg, _ := store.GetConfig(userID)
		src, _ := store.GetSources(userID)
		return cfg, src
	}

	fake := calendartest.New()
	defer fake.Close()
	fake.AddCalendar(hubID, "Hub", false)
	fake.AddCalendar(aID, "A", false)
	fake.AddCalendar(bID, "B", false)
	start := time.Now().Add(48 * time.Hour).UTC()
	fake.SeedEvent(aID, calendar.GCalEvent{
		Summary: "Meeting",
		Start:   calendar.EventTime{DateTime: start.Format(time.RFC3339)},
		End:     calendar.EventTime{DateTime: start.Add(time.Hour).Format(time.RFC3339)},
	})

	cfg, sources := reload()
	res, err := RunSync(ctx, fake.Client(), token, store, cfg, sources)
	if err != nil || res.Errors != 0 {
		t.Fatalf("pass 1: err=%v errors=%v", err, res.ErrorDetails)
	}
	if got := len(placeholdersOn(fake, hubID)); got != 1 {
		t.Fatalf("hub should have 1 placeholder, got %d", got)
	}
	bPlaceholders := placeholdersOn(fake, bID)
	if len(bPlaceholders) != 1 {
		t.Fatalf("source B should have 1 propagated placeholder, got %d", len(bPlaceholders))
	}
	srcEv := fake.Events(aID)[0] // the seeded source event on A
	// The outbound stamp must be the SOURCE's Updated (not the hub placeholder's own),
	// and sourceEventId must be the origin event's id (not the hub placeholder's) — both
	// would be wrong under the pre-3a code.
	if got := SourceUpdated(bPlaceholders[0]); got != srcEv.Updated {
		t.Fatalf("outbound sourceUpdated = %q, want source A's Updated %q", got, srcEv.Updated)
	}
	if got := SourceEventID(bPlaceholders[0]); got != srcEv.ID {
		t.Fatalf("outbound sourceEventId = %q, want origin event %q", got, srcEv.ID)
	}
	if got := len(placeholdersOn(fake, aID)); got != 0 {
		t.Fatalf("source A (the origin) must not get a placeholder, got %d", got)
	}

	// Unchanged second pass converges — no outbound churn.
	cfg, sources = reload()
	res2, err := RunSync(ctx, fake.Client(), token, store, cfg, sources)
	if err != nil || res2.Errors != 0 {
		t.Fatalf("pass 2: err=%v errors=%v", err, res2.ErrorDetails)
	}
	if res2.Created != 0 || res2.Updated != 0 || res2.Deleted != 0 {
		t.Fatalf("two-source pass should converge, got created=%d updated=%d deleted=%d",
			res2.Created, res2.Updated, res2.Deleted)
	}
}

// fastSyncSetup builds a store+fake with the given source calendars and returns helpers.
// Callers run the first full pass themselves (it establishes the sync tokens the fast
// pass needs).
func fastSyncSetup(t *testing.T, sources ...string) (*Store, *calendartest.Fake, func() (*SyncConfig, []SourceCalendar)) {
	t.Helper()
	const (
		userID = "u1"
		hubID  = "hub@x"
		token  = "tok"
	)
	store := newTestStore(t)
	if _, err := store.SaveConfig(userID, SaveConfigInput{HubCalendarID: hubID, HubCalendarName: "Hub", SyncWindowWeeks: 8, SyncIntervalMinutes: 15}); err != nil {
		t.Fatal(err)
	}
	inputs := make([]SourceCalendarInput, len(sources))
	for i, s := range sources {
		inputs[i] = SourceCalendarInput{CalendarID: s, CalendarName: s}
	}
	if _, err := store.ReconcileSources(userID, inputs); err != nil {
		t.Fatal(err)
	}
	fake := calendartest.New()
	t.Cleanup(fake.Close)
	fake.AddCalendar(hubID, "Hub", false)
	for _, s := range sources {
		fake.AddCalendar(s, s, false)
	}
	reload := func() (*SyncConfig, []SourceCalendar) {
		cfg, _ := store.GetConfig(userID)
		src, _ := store.GetSources(userID)
		return cfg, src
	}
	return store, fake, reload
}

func TestFastPassConvergesAndAppliesEdit(t *testing.T) {
	const (
		hubID, aID, token = "hub@x", "a@x", "tok"
	)
	ctx := context.Background()
	store, fake, reload := fastSyncSetup(t, aID)
	start := time.Now().Add(48 * time.Hour).UTC()
	evID := fake.SeedEvent(aID, calendar.GCalEvent{
		Summary: "Standup",
		Start:   calendar.EventTime{DateTime: start.Format(time.RFC3339)},
		End:     calendar.EventTime{DateTime: start.Add(30 * time.Minute).Format(time.RFC3339)},
	})

	// Full pass: establishes the token and creates the hub placeholder.
	cfg, sources := reload()
	if _, err := RunSync(ctx, fake.Client(), token, store, cfg, sources); err != nil {
		t.Fatalf("full pass: %v", err)
	}
	if len(placeholdersOn(fake, hubID)) != 1 {
		t.Fatal("full pass should have created the hub placeholder")
	}

	// Fast pass, no change: zero writes, and it goes incremental (no full scan).
	cfg, sources = reload()
	fake.ResetCounts()
	res, err := RunSyncWithOptions(ctx, fake.Client(), token, store, cfg, sources, SyncOptions{Fast: true})
	if err != nil || res.Errors != 0 {
		t.Fatalf("fast no-op: err=%v errors=%v", err, res.ErrorDetails)
	}
	if res.Created+res.Updated+res.Deleted != 0 {
		t.Fatalf("fast no-op should write nothing, got %+v", res)
	}
	c := fake.Counts()
	if c.Incremental < 1 {
		t.Fatalf("fast pass should use the sync token, counts=%+v", c)
	}
	if c.FullSyncList != 0 || c.WindowList != 0 {
		t.Fatalf("fast pass must not do a full/windowed re-list, counts=%+v", c)
	}

	// Edit the source event → fast pass updates exactly the one placeholder.
	fake.SeedEvent(aID, calendar.GCalEvent{
		ID:      evID,
		Summary: "Standup (moved)",
		Start:   calendar.EventTime{DateTime: start.Format(time.RFC3339)},
		End:     calendar.EventTime{DateTime: start.Add(30 * time.Minute).Format(time.RFC3339)},
	})
	cfg, sources = reload()
	res2, err := RunSyncWithOptions(ctx, fake.Client(), token, store, cfg, sources, SyncOptions{Fast: true})
	if err != nil || res2.Errors != 0 {
		t.Fatalf("fast edit: err=%v errors=%v", err, res2.ErrorDetails)
	}
	if res2.Updated != 1 || res2.Created != 0 || res2.Deleted != 0 {
		t.Fatalf("fast pass should apply exactly one update, got %+v", res2)
	}
	if got := placeholdersOn(fake, hubID)[0].Summary; got != "Standup (moved)" {
		t.Fatalf("hub placeholder not updated, summary=%q", got)
	}

	// A full pass now converges — the fast pass already recorded the new state.
	cfg, sources = reload()
	res3, err := RunSync(ctx, fake.Client(), token, store, cfg, sources)
	if err != nil || res3.Errors != 0 {
		t.Fatalf("converging full pass: err=%v errors=%v", err, res3.ErrorDetails)
	}
	if res3.Created+res3.Updated+res3.Deleted != 0 {
		t.Fatalf("full pass after fast should converge, got %+v", res3)
	}
}

func TestFastPassDeletesRemovedEvent(t *testing.T) {
	const (
		hubID, aID, token = "hub@x", "a@x", "tok"
	)
	ctx := context.Background()
	store, fake, reload := fastSyncSetup(t, aID)
	start := time.Now().Add(48 * time.Hour).UTC()
	evID := fake.SeedEvent(aID, calendar.GCalEvent{
		Summary: "Doomed",
		Start:   calendar.EventTime{DateTime: start.Format(time.RFC3339)},
		End:     calendar.EventTime{DateTime: start.Add(time.Hour).Format(time.RFC3339)},
	})
	cfg, sources := reload()
	if _, err := RunSync(ctx, fake.Client(), token, store, cfg, sources); err != nil {
		t.Fatalf("full pass: %v", err)
	}

	// Delete the source event → the fast pass removes the placeholder + mapping.
	if err := fake.Client().DeleteEvent(ctx, token, aID, evID); err != nil {
		t.Fatal(err)
	}
	cfg, sources = reload()
	res, err := RunSyncWithOptions(ctx, fake.Client(), token, store, cfg, sources, SyncOptions{Fast: true})
	if err != nil || res.Errors != 0 {
		t.Fatalf("fast delete: err=%v errors=%v", err, res.ErrorDetails)
	}
	if res.Deleted != 1 {
		t.Fatalf("fast pass should delete the placeholder, got %+v", res)
	}
	if len(placeholdersOn(fake, hubID)) != 0 {
		t.Fatal("hub placeholder should be gone")
	}
	synced, _ := store.GetSyncedEventsForUser("u1")
	if len(synced) != 0 {
		t.Fatalf("mapping should be removed, got %d", len(synced))
	}
}

func TestFastPassOutboundPropagation(t *testing.T) {
	const (
		hubID, aID, bID, token = "hub@x", "a@x", "b@x", "tok"
	)
	ctx := context.Background()
	store, fake, reload := fastSyncSetup(t, aID, bID)
	start := time.Now().Add(48 * time.Hour).UTC()
	evID := fake.SeedEvent(aID, calendar.GCalEvent{
		Summary: "Meeting",
		Start:   calendar.EventTime{DateTime: start.Format(time.RFC3339)},
		End:     calendar.EventTime{DateTime: start.Add(time.Hour).Format(time.RFC3339)},
	})
	cfg, sources := reload()
	if _, err := RunSync(ctx, fake.Client(), token, store, cfg, sources); err != nil {
		t.Fatalf("full pass: %v", err)
	}
	if len(placeholdersOn(fake, bID)) != 1 {
		t.Fatal("full pass should propagate a placeholder to B")
	}

	// Edit on A → fast pass updates both the hub and B's outbound placeholder.
	fake.SeedEvent(aID, calendar.GCalEvent{
		ID:      evID,
		Summary: "Meeting (updated)",
		Start:   calendar.EventTime{DateTime: start.Format(time.RFC3339)},
		End:     calendar.EventTime{DateTime: start.Add(time.Hour).Format(time.RFC3339)},
	})
	cfg, sources = reload()
	res, err := RunSyncWithOptions(ctx, fake.Client(), token, store, cfg, sources, SyncOptions{Fast: true})
	if err != nil || res.Errors != 0 {
		t.Fatalf("fast edit: err=%v errors=%v", err, res.ErrorDetails)
	}
	if res.Updated != 2 { // hub + B
		t.Fatalf("fast pass should update hub and B, got %+v", res)
	}
	if got := placeholdersOn(fake, bID)[0].Summary; got != "Meeting (updated)" {
		t.Fatalf("B's placeholder not updated, summary=%q", got)
	}
	if got := len(placeholdersOn(fake, aID)); got != 0 {
		t.Fatalf("A (origin) must not get a placeholder, got %d", got)
	}
}

// TestFastPassWithoutTokenRunsFullPass covers the MUST-FIX guard: a fast pass on a
// source that has no sync token must delegate to a full pass (which establishes the
// token and reconciles) rather than replay the whole calendar through the per-event path.
func TestFastPassWithoutTokenRunsFullPass(t *testing.T) {
	const (
		hubID, aID, token = "hub@x", "a@x", "tok"
	)
	ctx := context.Background()
	store, fake, reload := fastSyncSetup(t, aID)
	start := time.Now().Add(48 * time.Hour).UTC()
	fake.SeedEvent(aID, calendar.GCalEvent{
		Summary: "E",
		Start:   calendar.EventTime{DateTime: start.Format(time.RFC3339)},
		End:     calendar.EventTime{DateTime: start.Add(time.Hour).Format(time.RFC3339)},
	})

	// No full pass has run, so the source has no token. A fast pass must promote to full.
	cfg, sources := reload()
	fake.ResetCounts()
	res, err := RunSyncWithOptions(ctx, fake.Client(), token, store, cfg, sources, SyncOptions{Fast: true})
	if err != nil || res.Errors != 0 {
		t.Fatalf("promoted pass: err=%v errors=%v", err, res.ErrorDetails)
	}
	if res.Created != 1 {
		t.Fatalf("promoted full pass should create the placeholder, got %+v", res)
	}
	if c := fake.Counts(); c.FullSyncList < 1 || c.Incremental != 0 {
		t.Fatalf("expected a full-pass token read, not incremental, counts=%+v", c)
	}
	if src, _ := store.GetSources("u1"); src[0].SyncToken == "" {
		t.Fatal("the promoted full pass should have established the sync token")
	}
}

// TestConfigChangeForcesFullPass covers the scheduling wiring: a completed full pass makes
// the user not-due, and clearing LastFullSyncAt (what PutConfig does on a config change)
// makes them due again.
func TestConfigChangeForcesFullPass(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.SaveConfig("u1", SaveConfigInput{HubCalendarID: "hub@x", SyncWindowWeeks: 8}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.SetLastFullSyncAt("u1", now.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	cfg, _ := store.GetConfig("u1")
	if FullPassDue(cfg, now) {
		t.Fatal("right after a full pass, not due")
	}
	// A config change clears it.
	if err := store.SetLastFullSyncAt("u1", ""); err != nil {
		t.Fatal(err)
	}
	cfg, _ = store.GetConfig("u1")
	if cfg.LastFullSyncAt != "" {
		t.Fatalf("clear failed, LastFullSyncAt=%q", cfg.LastFullSyncAt)
	}
	if !FullPassDue(cfg, now) {
		t.Fatal("after a config change, a full pass must be due")
	}
}

func TestFastPassExpiredTokenClearsAndPromotes(t *testing.T) {
	const (
		hubID, aID, token = "hub@x", "a@x", "tok"
	)
	ctx := context.Background()
	store, fake, reload := fastSyncSetup(t, aID)
	start := time.Now().Add(48 * time.Hour).UTC()
	evID := fake.SeedEvent(aID, calendar.GCalEvent{
		Summary: "E",
		Start:   calendar.EventTime{DateTime: start.Format(time.RFC3339)},
		End:     calendar.EventTime{DateTime: start.Add(time.Hour).Format(time.RFC3339)},
	})
	cfg, sources := reload()
	if _, err := RunSync(ctx, fake.Client(), token, store, cfg, sources); err != nil {
		t.Fatalf("full pass: %v", err)
	}

	// Expire the token and edit the event. The fast pass gets a 410, clears the token,
	// and skips the source this pass.
	fake.ExpireTokens(aID)
	fake.SeedEvent(aID, calendar.GCalEvent{ID: evID, Summary: "E edited",
		Start: calendar.EventTime{DateTime: start.Format(time.RFC3339)},
		End:   calendar.EventTime{DateTime: start.Add(time.Hour).Format(time.RFC3339)}})
	cfg, sources = reload()
	if _, err := RunSyncWithOptions(ctx, fake.Client(), token, store, cfg, sources, SyncOptions{Fast: true}); err != nil {
		t.Fatalf("fast pass (410): %v", err)
	}
	if src, _ := store.GetSources("u1"); src[0].SyncToken != "" {
		t.Fatal("an expired-token fast pass must clear the token")
	}

	// The next fast pass now sees no token → promotes to a full pass → applies the edit.
	cfg, sources = reload()
	if _, err := RunSyncWithOptions(ctx, fake.Client(), token, store, cfg, sources, SyncOptions{Fast: true}); err != nil {
		t.Fatalf("promoted pass: %v", err)
	}
	if got := placeholdersOn(fake, hubID)[0].Summary; got != "E edited" {
		t.Fatalf("promoted full pass should apply the edit, summary=%q", got)
	}
}

func TestFastPassBacksOffWhenSyncRunning(t *testing.T) {
	const (
		aID, token = "a@x", "tok"
	)
	ctx := context.Background()
	store, fake, reload := fastSyncSetup(t, aID)
	start := time.Now().Add(48 * time.Hour).UTC()
	fake.SeedEvent(aID, calendar.GCalEvent{Summary: "E",
		Start: calendar.EventTime{DateTime: start.Format(time.RFC3339)},
		End:   calendar.EventTime{DateTime: start.Add(time.Hour).Format(time.RFC3339)}})
	cfg, sources := reload()
	if _, err := RunSync(ctx, fake.Client(), token, store, cfg, sources); err != nil {
		t.Fatalf("full pass: %v", err)
	}

	// A concurrent sync is in progress (a running log row).
	if err := store.CreateSyncLog(&SyncLog{UserID: "u1", StartedAt: time.Now().UTC().Format(time.RFC3339), Status: "running", Kind: "full"}); err != nil {
		t.Fatal(err)
	}
	cfg, sources = reload()
	if _, err := RunSyncWithOptions(ctx, fake.Client(), token, store, cfg, sources, SyncOptions{Fast: true}); err == nil {
		t.Fatal("fast pass should back off while another sync is running")
	}
}

// TestUpdateSourceSyncTokenPersists guards token persistence. (The Firestore-specific
// failure — Where-by-pk matching nothing — can't be reproduced on SQLite, where the pk is
// a real column; but this catches a silent no-op on a missing source, and regressions in
// the point-Get round-trip.)
func TestUpdateSourceSyncTokenPersists(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.SaveConfig("u1", SaveConfigInput{HubCalendarID: "hub@x", SyncWindowWeeks: 8}); err != nil {
		t.Fatal(err)
	}
	sources, err := store.ReconcileSources("u1", []SourceCalendarInput{{CalendarID: "a@x", CalendarName: "A"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSourceSyncToken(sources[0].ID, "tok-xyz"); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetSources("u1")
	if got[0].SyncToken != "tok-xyz" {
		t.Fatalf("sync token not persisted, got %q", got[0].SyncToken)
	}
	// An unknown source must error, not silently succeed (the old Where-by-pk path did).
	if err := store.UpdateSourceSyncToken("does-not-exist", "x"); err == nil {
		t.Fatal("updating an unknown source should error")
	}
}

func TestFullPassDue(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	if !FullPassDue(&SyncConfig{LastFullSyncAt: ""}, now) {
		t.Error("no prior full pass → due")
	}
	if FullPassDue(&SyncConfig{LastFullSyncAt: now.Add(-1 * time.Hour).Format(time.RFC3339)}, now) {
		t.Error("recent full pass → not due")
	}
	if !FullPassDue(&SyncConfig{LastFullSyncAt: now.Add(-25 * time.Hour).Format(time.RFC3339)}, now) {
		t.Error("stale full pass → due")
	}
	if !FullPassDue(&SyncConfig{LastFullSyncAt: "not-a-time"}, now) {
		t.Error("unparseable → due")
	}
}

func TestFastPassLogging(t *testing.T) {
	const (
		aID, token = "a@x", "tok"
	)
	ctx := context.Background()
	store, fake, reload := fastSyncSetup(t, aID)
	start := time.Now().Add(48 * time.Hour).UTC()
	mk := func(id, summary string) calendar.GCalEvent {
		return calendar.GCalEvent{ID: id, Summary: summary,
			Start: calendar.EventTime{DateTime: start.Format(time.RFC3339)},
			End:   calendar.EventTime{DateTime: start.Add(time.Hour).Format(time.RFC3339)}}
	}
	evID := fake.SeedEvent(aID, mk("", "E"))

	cfg, sources := reload()
	if _, err := RunSync(ctx, fake.Client(), token, store, cfg, sources); err != nil {
		t.Fatal(err)
	}
	base, _ := store.GetRecentSyncLogs("u1", 50)

	// A no-op fast pass writes no durable log row (heartbeat via LastSyncAt only).
	cfg, sources = reload()
	if _, err := RunSyncWithOptions(ctx, fake.Client(), token, store, cfg, sources, SyncOptions{Fast: true}); err != nil {
		t.Fatal(err)
	}
	if after, _ := store.GetRecentSyncLogs("u1", 50); len(after) != len(base) {
		t.Fatalf("no-op fast pass must not add a log row, %d→%d", len(base), len(after))
	}

	// A fast pass that changes something writes exactly one Kind=fast row.
	fake.SeedEvent(aID, mk(evID, "E edited"))
	cfg, sources = reload()
	if _, err := RunSyncWithOptions(ctx, fake.Client(), token, store, cfg, sources, SyncOptions{Fast: true}); err != nil {
		t.Fatal(err)
	}
	after, _ := store.GetRecentSyncLogs("u1", 50)
	if len(after) != len(base)+1 {
		t.Fatalf("a changing fast pass should add one log row, %d→%d", len(base), len(after))
	}
	// (Order isn't asserted — the full and fast rows can share an RFC3339 second.)
	fastRows := 0
	for _, l := range after {
		if l.Kind == "fast" {
			fastRows++
		}
	}
	if fastRows != 1 {
		t.Fatalf("expected exactly one Kind=fast log row, got %d", fastRows)
	}
}

func placeholdersOn(fake *calendartest.Fake, calID string) []calendar.GCalEvent {
	var out []calendar.GCalEvent
	for _, ev := range fake.Events(calID) {
		if IsPlaceholder(ev) {
			out = append(out, ev)
		}
	}
	return out
}
