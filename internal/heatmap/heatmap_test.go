package heatmap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/michaelwinser/calendar-sync/internal/platform/calendar"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func mustDate(t *testing.T, s string, loc *time.Location) time.Time {
	t.Helper()
	d, err := time.ParseInLocation("2006-01-02", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestParseParams(t *testing.T) {
	ids := []string{"a"}
	cases := []struct {
		name           string
		tz, start, end string
		ids            []string
		wantErr        bool
	}{
		{"valid", "America/Los_Angeles", "2026-01-01", "2026-06-01", ids, false},
		{"empty tz", "", "2026-01-01", "2026-06-01", ids, true},
		{"Local tz rejected", "Local", "2026-01-01", "2026-06-01", ids, true},
		{"bad tz", "Nowhere/Void", "2026-01-01", "2026-06-01", ids, true},
		{"start >= end", "UTC", "2026-06-01", "2026-06-01", ids, true},
		{"range too large", "UTC", "2026-01-01", "2029-01-01", ids, true},
		{"no calendars", "UTC", "2026-01-01", "2026-06-01", nil, true},
		{"too many calendars", "UTC", "2026-01-01", "2026-06-01", make([]string, 21), true},
		{"bad start", "UTC", "01/01/2026", "2026-06-01", ids, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, _, err := parseParams(c.tz, c.start, c.end, c.ids)
			if c.wantErr != (err != nil) {
				t.Fatalf("wantErr=%v, got err=%v", c.wantErr, err)
			}
		})
	}
}

func TestIncludeEvent(t *testing.T) {
	base := calendar.GCalEvent{
		Start: calendar.EventTime{DateTime: "2026-01-01T10:00:00Z"},
		End:   calendar.EventTime{DateTime: "2026-01-01T10:30:00Z"},
	}
	if !includeEvent(base) {
		t.Error("plain timed event should be included")
	}
	// empty eventType falls back to "default"
	base.EventType = ""
	if !includeEvent(base) {
		t.Error("empty eventType should count as default")
	}
	allDay := base
	allDay.Start = calendar.EventTime{Date: "2026-01-01"}
	allDay.End = calendar.EventTime{}
	if includeEvent(allDay) {
		t.Error("all-day event should be excluded")
	}
	for _, tc := range []struct {
		name string
		mut  func(*calendar.GCalEvent)
	}{
		{"cancelled", func(e *calendar.GCalEvent) { e.Status = "cancelled" }},
		{"transparent", func(e *calendar.GCalEvent) { e.Transparency = "transparent" }},
		{"outOfOffice", func(e *calendar.GCalEvent) { e.EventType = "outOfOffice" }},
		{"declined", func(e *calendar.GCalEvent) {
			e.Attendees = []calendar.Attendee{{Self: true, ResponseStatus: "declined"}}
		}},
	} {
		ev := base
		tc.mut(&ev)
		if includeEvent(ev) {
			t.Errorf("%s event should be excluded", tc.name)
		}
	}
}

// Wall-clock minutes must be Hour*60+Minute, correct on DST-transition days (an
// elapsed-minutes port would shift by an hour).
func TestEventSegmentsDST(t *testing.T) {
	la := mustLoc(t, "America/Los_Angeles")
	for _, tc := range []struct {
		name       string
		start, end string
	}{
		{"fall back (25h day)", "2026-11-01T10:00:00-08:00", "2026-11-01T10:30:00-08:00"},
		{"spring forward (23h day)", "2026-03-08T10:00:00-07:00", "2026-03-08T10:30:00-07:00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := calendar.GCalEvent{Summary: "X",
				Start: calendar.EventTime{DateTime: tc.start},
				End:   calendar.EventTime{DateTime: tc.end}}
			segs := eventSegments(ev, la, "2026-01-01", "2027-01-01")
			if len(segs) != 1 {
				t.Fatalf("want 1 segment, got %d: %+v", len(segs), segs)
			}
			if segs[0].S != 600 || segs[0].E != 630 {
				t.Fatalf("want s=600 e=630 (wall clock), got s=%d e=%d", segs[0].S, segs[0].E)
			}
		})
	}
}

// A timed event crossing midnight splits into two segments, the first ending at 1440.
func TestEventSegmentsMultiDay(t *testing.T) {
	la := mustLoc(t, "America/Los_Angeles")
	ev := calendar.GCalEvent{Summary: "Night",
		Start: calendar.EventTime{DateTime: "2026-06-01T22:00:00-07:00"},
		End:   calendar.EventTime{DateTime: "2026-06-02T01:00:00-07:00"}}
	segs := eventSegments(ev, la, "2026-01-01", "2027-01-01")
	if len(segs) != 2 {
		t.Fatalf("want 2 segments, got %d: %+v", len(segs), segs)
	}
	if segs[0].D != "2026-06-01" || segs[0].S != 1320 || segs[0].E != 1440 {
		t.Errorf("day 1: got %+v, want {2026-06-01 1320 1440}", segs[0])
	}
	if segs[1].D != "2026-06-02" || segs[1].S != 0 || segs[1].E != 60 {
		t.Errorf("day 2: got %+v, want {2026-06-02 0 60}", segs[1])
	}
}

func TestWeekdayTotals(t *testing.T) {
	utc := time.UTC
	// 2026-01-01 (Thu) .. 2026-01-08 (excl) = one of each weekday.
	wt := weekdayTotals(mustDate(t, "2026-01-01", utc), mustDate(t, "2026-01-08", utc), utc)
	for d, n := range wt {
		if n != 1 {
			t.Fatalf("weekday %d = %d, want 1 (wt=%v)", d, n, wt)
		}
	}
}

// fakeGoogle serves events.list per calendar id from a canned map.
func fakeGoogle(t *testing.T, byCal map[string][]calendar.GCalEvent, fail map[string]bool) *calendar.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// path: /calendars/{id}/events
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/calendars/"), "/")
		calID := parts[0]
		if fail[calID] {
			// 403 forbidden is fail-fast (not retried) — a realistic read-only/revoked
			// calendar, and it keeps the test quick.
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"errors":[{"reason":"forbidden"}]}}`))
			return
		}
		// Honor the partial-response field mask like Google does: drop fields the mask
		// doesn't name. This makes the dedupe test regress if the mask loses id /
		// extendedProperties (the fields canonicalID depends on).
		fields := r.URL.Query().Get("fields")
		evs := byCal[calID]
		if fields != "" {
			masked := make([]calendar.GCalEvent, len(evs))
			for i, e := range evs {
				if !strings.Contains(fields, "id,") {
					e.ID = ""
				}
				if !strings.Contains(fields, "extendedProperties") {
					e.ExtendedProperties = nil
				}
				masked[i] = e
			}
			evs = masked
		}
		json.NewEncoder(w).Encode(map[string]any{"items": evs})
	}))
	t.Cleanup(srv.Close)
	return calendar.New(srv.URL)
}

func timed(id, title, start, end string) calendar.GCalEvent {
	return calendar.GCalEvent{ID: id, Summary: title,
		Start: calendar.EventTime{DateTime: start}, End: calendar.EventTime{DateTime: end}}
}

// Same meeting on a source calendar (real) and the hub (placeholder) collapses to one
// segment via canonical-id dedupe; a failing calendar becomes a warning.
func TestBuildHeatmapDedupeAndWarnings(t *testing.T) {
	utc := time.UTC
	real := timed("X", "Standup", "2026-06-01T10:00:00Z", "2026-06-01T10:30:00Z")
	placeholder := timed("P", "🔄 Standup", "2026-06-01T10:00:00Z", "2026-06-01T10:30:00Z")
	placeholder.ExtendedProperties = &calendar.ExtendedProperties{
		Private: map[string]string{"calendarSyncMarker": "v1", "sourceEventId": "X"},
	}
	cal := fakeGoogle(t,
		map[string][]calendar.GCalEvent{
			"src": {real},
			"hub": {placeholder},
		},
		map[string]bool{"broken": true},
	)

	res := buildHeatmap(context.Background(), cal, "tok",
		[]string{"src", "hub", "broken"},
		mustDate(t, "2026-06-01", utc), mustDate(t, "2026-06-08", utc), utc)

	if len(res.Segments) != 1 {
		t.Fatalf("want 1 segment after dedupe, got %d: %+v", len(res.Segments), res.Segments)
	}
	if res.Segments[0].S != 600 || res.Segments[0].E != 630 {
		t.Errorf("segment minutes: got s=%d e=%d, want 600/630", res.Segments[0].S, res.Segments[0].E)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].CalendarID != "broken" {
		t.Fatalf("want one warning for 'broken', got %+v", res.Warnings)
	}
}
