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

	// Window read excludes the June event; issues a sync token.
	res, err := c.ListEvents(ctx, tok, "work@x",
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 || res.Events[0].Summary != "Standup" {
		t.Fatalf("window read: want [Standup], got %+v", res.Events)
	}
	if res.SyncToken == "" {
		t.Fatal("window read should return a sync token")
	}
}

func TestIncrementalDeltaLifecycle(t *testing.T) {
	f := calendartest.New()
	defer f.Close()
	f.AddCalendar("work@x", "Work", true)
	c := f.Client()
	ctx := context.Background()

	// Establish a baseline token with an empty window read.
	base, err := c.ListEvents(ctx, tok, "work@x",
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	// Create via the client, then incremental should report exactly that event.
	created, err := c.CreateEvent(ctx, tok, "work@x", ptr(ev("New", "2026-03-02T09:00:00Z", "2026-03-02T10:00:00Z")))
	if err != nil {
		t.Fatal(err)
	}
	d1, err := c.ListEventsIncremental(ctx, tok, "work@x", base.SyncToken)
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

	base, _ := c.ListEvents(ctx, tok, "work@x",
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	// Expire immediately, with NO intervening mutation — the case that must still 410
	// (a version bump inside ExpireTokens is what makes this work).
	f.ExpireTokens("work@x")

	_, err := c.ListEventsIncremental(ctx, tok, "work@x", base.SyncToken)
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

	c := f.Client()
	deleted, errCount := c.BatchDeleteEvents(context.Background(), tok, "work@x",
		[]string{id1, id2, "missing"})
	if deleted != 2 {
		t.Fatalf("want 2 deleted, got %d (errors=%d)", deleted, errCount)
	}
	if errCount != 1 {
		t.Fatalf("want 1 error (missing id), got %d", errCount)
	}
	if got := f.Events("work@x"); len(got) != 0 {
		t.Fatalf("want calendar empty after batch delete, got %+v", got)
	}
}

func TestCountsTrackReadsAndWrites(t *testing.T) {
	f := calendartest.New()
	defer f.Close()
	f.AddCalendar("work@x", "Work", true)
	c := f.Client()
	ctx := context.Background()

	_, _ = c.ListCalendars(ctx, tok)
	base, _ := c.ListEvents(ctx, tok, "work@x",
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	_, _ = c.CreateEvent(ctx, tok, "work@x", ptr(ev("n", "2026-03-02T09:00:00Z", "2026-03-02T10:00:00Z")))
	_, _ = c.ListEventsIncremental(ctx, tok, "work@x", base.SyncToken)

	got := f.Counts()
	if got.Calendars != 1 || got.WindowList != 1 || got.Incremental != 1 || got.Create != 1 {
		t.Fatalf("unexpected counts: %+v", got)
	}
	if got.Reads() != 3 { // calendars + window + incremental
		t.Fatalf("want Reads()=3, got %d", got.Reads())
	}
}

func ptr(e calendar.GCalEvent) *calendar.GCalEvent { return &e }
