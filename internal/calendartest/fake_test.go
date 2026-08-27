package calendartest_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/michaelwinser/calendar-sync/internal/calendartest"
	"github.com/michaelwinser/calendar-sync/internal/platform/calendar"
)

const tok = "test-token"

func ev(summary, start, end string) calendar.GCalEvent {
	return calendar.GCalEvent{
		Summary: summary,
		Start:   calendar.EventTime{DateTime: start},
		End:     calendar.EventTime{DateTime: end},
	}
}

func TestCalendarListAndWindowRead(t *testing.T) {
	f := calendartest.New()
	defer f.Close()
	f.AddCalendar("work@x", "Work", true)
	f.AddCalendar("home@x", "Home", false)
	f.SeedEvent("work@x", ev("Standup", "2026-03-02T09:00:00Z", "2026-03-02T09:30:00Z"))
	f.SeedEvent("work@x", ev("Later", "2026-06-01T09:00:00Z", "2026-06-01T09:30:00Z"))

	c := f.Client()
	ctx := context.Background()

	cals, err := c.ListCalendars(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 2 {
		t.Fatalf("want 2 calendars, got %d", len(cals))
	}

	// Window read excludes the June event.
	res, err := c.ListEvents(ctx, tok, "work@x",
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 || res.Events[0].Summary != "Standup" {
		t.Fatalf("window read: want [Standup], got %+v", res.Events)
	}
	// A windowed read gets NO sync token from Google (timeMin/timeMax are mutually
	// exclusive with sync tokens) — the fake mirrors that.
	if res.SyncToken != "" {
		t.Fatalf("windowed read should not return a sync token, got %q", res.SyncToken)
	}
}

func TestIncrementalDeltaLifecycle(t *testing.T) {
	f := calendartest.New()
	defer f.Close()
	f.AddCalendar("work@x", "Work", true)
	c := f.Client()
	ctx := context.Background()

	// Capture a baseline token (as an unrestricted full sync would), then create an
	// event; incremental since the baseline should report exactly that event.
	baseTok := f.SyncToken("work@x")
	created, err := c.CreateEvent(ctx, tok, "work@x", ptr(ev("New", "2026-03-02T09:00:00Z", "2026-03-02T10:00:00Z")))
	if err != nil {
		t.Fatal(err)
	}
	d1, err := c.ListEventsIncremental(ctx, tok, "work@x", baseTok)
	if err != nil {
		t.Fatal(err)
	}
	if len(d1.Events) != 1 || d1.Events[0].ID != created.ID || d1.Events[0].Status == "cancelled" {
		t.Fatalf("delta after create: want [%s live], got %+v", created.ID, d1.Events)
	}

	// A second incremental with the fresh token sees no changes.
	d2, err := c.ListEventsIncremental(ctx, tok, "work@x", d1.SyncToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(d2.Events) != 0 {
		t.Fatalf("delta with up-to-date token: want none, got %+v", d2.Events)
	}

	// Delete → incremental reports a cancelled tombstone.
	if err := c.DeleteEvent(ctx, tok, "work@x", created.ID); err != nil {
		t.Fatal(err)
	}
	d3, err := c.ListEventsIncremental(ctx, tok, "work@x", d2.SyncToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(d3.Events) != 1 || d3.Events[0].Status != "cancelled" {
		t.Fatalf("delta after delete: want one cancelled, got %+v", d3.Events)
	}
}

func TestExpiredSyncToken(t *testing.T) {
	f := calendartest.New()
	defer f.Close()
	f.AddCalendar("work@x", "Work", true)
	c := f.Client()
	ctx := context.Background()

	baseTok := f.SyncToken("work@x")
	// Expire immediately, with NO intervening mutation — the case that must still 410
	// (a version bump inside ExpireTokens is what makes this work).
	f.ExpireTokens("work@x")

	_, err := c.ListEventsIncremental(ctx, tok, "work@x", baseTok)
	if !errors.Is(err, calendar.ErrSyncTokenExpired) {
		t.Fatalf("want ErrSyncTokenExpired, got %v", err)
	}
}

func TestWindowReadDeterministicOrder(t *testing.T) {
	f := calendartest.New()
	defer f.Close()
	f.AddCalendar("work@x", "Work", true)
	// Seed out of chronological order; the read must come back start-ordered.
	f.SeedEvent("work@x", ev("C", "2026-03-03T09:00:00Z", "2026-03-03T10:00:00Z"))
	f.SeedEvent("work@x", ev("A", "2026-03-01T09:00:00Z", "2026-03-01T10:00:00Z"))
	f.SeedEvent("work@x", ev("B", "2026-03-02T09:00:00Z", "2026-03-02T10:00:00Z"))

	c := f.Client()
	res, err := c.ListEvents(context.Background(), tok, "work@x",
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, e := range res.Events {
		got = append(got, e.Summary)
	}
	if want := []string{"A", "B", "C"}; !slices.Equal(got, want) {
		t.Fatalf("want start-ordered %v, got %v", want, got)
	}
}

func TestListEventsForSyncEstablishesToken(t *testing.T) {
	f := calendartest.New()
	defer f.Close()
	f.AddCalendar("work@x", "Work", true)
	f.SeedEvent("work@x", ev("March", "2026-03-02T09:00:00Z", "2026-03-02T09:30:00Z"))
	f.SeedEvent("work@x", ev("June", "2026-06-01T09:00:00Z", "2026-06-01T09:30:00Z"))

	c := f.Client()
	ctx := context.Background()

	// The unrestricted bootstrap read returns ALL events and — unlike a windowed read —
	// a usable sync token.
	res, err := c.ListEventsForSync(ctx, tok, "work@x")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 2 {
		t.Fatalf("bootstrap read should return all events, got %d", len(res.Events))
	}
	if res.SyncToken == "" {
		t.Fatal("bootstrap read must return a sync token")
	}
	if c := f.Counts(); c.FullSyncList != 1 || c.WindowList != 0 {
		t.Fatalf("want one FullSyncList read, got %+v", c)
	}

	// The token works: a subsequent change shows up incrementally.
	created, err := c.CreateEvent(ctx, tok, "work@x", ptr(ev("New", "2026-04-01T09:00:00Z", "2026-04-01T10:00:00Z")))
	if err != nil {
		t.Fatal(err)
	}
	d, err := c.ListEventsIncremental(ctx, tok, "work@x", res.SyncToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Events) != 1 || d.Events[0].ID != created.ID {
		t.Fatalf("incremental after bootstrap should report the new event, got %+v", d.Events)
	}
}

func TestPagingAcrossListModes(t *testing.T) {
	f := calendartest.New()
	defer f.Close()
	f.AddCalendar("work@x", "Work", true)
	for _, d := range []string{"01", "02", "03", "04", "05"} {
		f.SeedEvent("work@x", ev("E"+d, "2026-03-"+d+"T09:00:00Z", "2026-03-"+d+"T10:00:00Z"))
	}
	f.SetPageSize(2) // force ≥3 pages for 5 events

	c := f.Client()
	ctx := context.Background()

	// Windowed read pages through all 5 and returns no token.
	res, err := c.ListEvents(ctx, tok, "work@x",
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 5 {
		t.Fatalf("windowed paging should return all 5, got %d", len(res.Events))
	}

	// Bootstrap read pages through all 5 and returns a token only on the final page.
	full, err := c.ListEventsForSync(ctx, tok, "work@x")
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Events) != 5 {
		t.Fatalf("bootstrap paging should return all 5, got %d", len(full.Events))
	}
	if full.SyncToken == "" {
		t.Fatal("bootstrap read must still yield a token after paging")
	}
	// A multi-page read is multiple HTTP calls: 5 events / pageSize 2 = 3 pages each.
	if c := f.Counts(); c.WindowList != 3 || c.FullSyncList != 3 {
		t.Fatalf("want 3 window + 3 fullsync page-requests, got %+v", c)
	}
}

func TestPropertyFilter(t *testing.T) {
	f := calendartest.New()
	defer f.Close()
	f.AddCalendar("work@x", "Work", true)
	placeholder := ev("Placeholder", "2026-03-02T09:00:00Z", "2026-03-02T10:00:00Z")
	placeholder.ExtendedProperties = &calendar.ExtendedProperties{Private: map[string]string{"syncSource": "home@x"}}
	f.SeedEvent("work@x", placeholder)
	f.SeedEvent("work@x", ev("Normal", "2026-03-03T09:00:00Z", "2026-03-03T10:00:00Z"))

	c := f.Client()
	got, err := c.ListEventsByProperty(context.Background(), tok, "work@x", calendar.EventQuery{
		PrivateProps: map[string]string{"syncSource": "home@x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Summary != "Placeholder" {
		t.Fatalf("property filter: want [Placeholder], got %+v", got)
	}
}

func TestBatchDelete(t *testing.T) {
	f := calendartest.New()
	defer f.Close()
	f.AddCalendar("work@x", "Work", true)
	id1 := f.SeedEvent("work@x", ev("a", "2026-03-02T09:00:00Z", "2026-03-02T10:00:00Z"))
	id2 := f.SeedEvent("work@x", ev("b", "2026-03-03T09:00:00Z", "2026-03-03T10:00:00Z"))

	f.FailDelete("work@x", id2) // id2's delete will 403

	c := f.Client()
	res := c.BatchDeleteEvents(context.Background(), tok, "work@x",
		[]string{id1, id2, "missing"})
	// id1 deleted (2xx) and "missing" already-absent (404) → Gone; id2 → Failed.
	if !res.Gone[id1] || !res.Gone["missing"] {
		t.Fatalf("want id1 and missing Gone, got %+v", res)
	}
	if !res.Failed[id2] || res.Gone[id2] {
		t.Fatalf("want id2 Failed, got %+v", res)
	}
	// id2 must survive (its delete failed); id1 is gone.
	got := f.Events("work@x")
	if len(got) != 1 || got[0].ID != id2 {
		t.Fatalf("want only id2 remaining, got %+v", got)
	}
}

func TestCountsTrackReadsAndWrites(t *testing.T) {
	f := calendartest.New()
	defer f.Close()
	f.AddCalendar("work@x", "Work", true)
	c := f.Client()
	ctx := context.Background()

	_, _ = c.ListCalendars(ctx, tok)
	_, _ = c.ListEvents(ctx, tok, "work@x",
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	baseTok := f.SyncToken("work@x")
	_, _ = c.CreateEvent(ctx, tok, "work@x", ptr(ev("n", "2026-03-02T09:00:00Z", "2026-03-02T10:00:00Z")))
	_, _ = c.ListEventsIncremental(ctx, tok, "work@x", baseTok)

	got := f.Counts()
	if got.Calendars != 1 || got.WindowList != 1 || got.Incremental != 1 || got.Create != 1 {
		t.Fatalf("unexpected counts: %+v", got)
	}
	if got.Reads() != 3 { // calendars + window + incremental
		t.Fatalf("want Reads()=3, got %d", got.Reads())
	}
}

func ptr(e calendar.GCalEvent) *calendar.GCalEvent { return &e }
