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

func placeholdersOn(fake *calendartest.Fake, calID string) []calendar.GCalEvent {
	var out []calendar.GCalEvent
	for _, ev := range fake.Events(calID) {
		if IsPlaceholder(ev) {
			out = append(out, ev)
		}
	}
	return out
}
