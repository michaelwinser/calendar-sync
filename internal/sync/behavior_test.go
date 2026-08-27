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

	// Call-shape baseline that documents today's cost and what Phase 3 must change: the
	// idle pass re-reads the FULL window per source and never goes incremental, because
	// a windowed events.list yields no sync token from Google (so SyncToken stays empty
	// and the incremental branch is dead code). This full re-read every pass is the
	// Firestore/Google cost the two-tier work targets.
	counts := fake.Counts()
	if counts.Incremental != 0 {
		t.Fatalf("second pass unexpectedly went incremental (no token should exist yet), counts=%+v", counts)
	}
	if counts.WindowList != len(sources) {
		t.Fatalf("second pass should re-read the full window once per source (%d), counts=%+v", len(sources), counts)
	}
	// It also scans the Firestore mapping on this idle pass — exactly the cost M8
	// Phase 3 (two-tier sync) removes.
	if store.Reads() <= readsBefore {
		t.Fatalf("expected the idle pass to still read the mapping (reads went %d→%d)",
			readsBefore, store.Reads())
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

func placeholdersOn(fake *calendartest.Fake, calID string) []calendar.GCalEvent {
	var out []calendar.GCalEvent
	for _, ev := range fake.Events(calID) {
		if IsPlaceholder(ev) {
			out = append(out, ev)
		}
	}
	return out
}
