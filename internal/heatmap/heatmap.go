// Package heatmap is a read-only utility module: a weekly occupancy heatmap over the
// user's calendars. It owns no store data; it fetches via platform/calendar and uses
// sync's exported placeholder API to dedupe the app's own cross-calendar copies.
package heatmap

import (
	"context"
	"fmt"
	"net/http"
	"time"

	_ "time/tzdata" // bundle zoneinfo so non-container runs/tests match the image

	"github.com/michaelwinser/appbase"
	"github.com/michaelwinser/appbase/server"
	"golang.org/x/sync/errgroup"

	"github.com/michaelwinser/calendar-sync/internal/app"
	"github.com/michaelwinser/calendar-sync/internal/platform"
	"github.com/michaelwinser/calendar-sync/internal/platform/calendar"
)

const (
	maxCalendars     = 20
	maxSpanMonths    = 24
	maxSegments      = 50000
	fetchDeadline    = 25 * time.Second
	fetchConcurrency = 4

	// eventFields is a partial-response mask: only the fields the heatmap needs.
	// It requests start/end whole so start.date stays visible for the all-day filter,
	// and MUST include id + extendedProperties/private — canonicalID dedupe relies on
	// both (omitting them would collapse the whole grid to one event).
	eventFields = "items(id,summary,start,end,status,transparency,eventType,recurringEventId,extendedProperties/private,attendees(self,responseStatus)),nextPageToken"
)

var countedEventTypes = map[string]bool{"default": true}

type module struct{ cal *calendar.Client }

// RegisterRoutes mounts the heatmap API.
func RegisterRoutes(deps platform.Deps) error {
	m := &module{cal: deps.Cal}
	deps.Router.Get("/api/heatmap/events", m.events)
	return nil
}

// RegisterPages mounts the heatmap page.
func RegisterPages(deps platform.Deps) {
	deps.Router.Get("/heatmap", deps.LoginPage(page))
}

// segment is one event instance clipped to a single local day.
type segment struct {
	T string `json:"t"` // title
	R bool   `json:"r"` // recurring
	W int    `json:"w"` // weekday, 0=Sunday
	D string `json:"d"` // date key, yyyy-mm-dd
	S int    `json:"s"` // start minute of day (wall clock)
	E int    `json:"e"` // end minute of day (wall clock; 1440 = local midnight)
}

type warning struct {
	CalendarID string `json:"calendarId"`
	Error      string `json:"error"`
}

type heatmapResult struct {
	Segments      []segment `json:"segments"`
	WeekdayTotals [7]int    `json:"weekdayTotals"`
	Warnings      []warning `json:"warnings"`
}

func (m *module) events(w http.ResponseWriter, r *http.Request) {
	if appbase.UserID(r) == "" {
		server.RespondError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	token, err := platform.AccessToken(r)
	if err != nil {
		server.RespondError(w, http.StatusForbidden, err.Error())
		return
	}
	q := r.URL.Query()
	calIDs := q["calendarId"]
	loc, start, end, err := parseParams(q.Get("tz"), q.Get("start"), q.Get("end"), calIDs)
	if err != nil {
		server.RespondError(w, http.StatusBadRequest, err.Error())
		return
	}
	res := buildHeatmap(r.Context(), m.cal, token, calIDs, start, end, loc)
	server.RespondJSON(w, http.StatusOK, res)
}

// parseParams validates and normalizes the query into (loc, start, end).
func parseParams(tz, startStr, endStr string, calIDs []string) (*time.Location, time.Time, time.Time, error) {
	var zero time.Time
	if tz == "" || tz == "Local" {
		return nil, zero, zero, fmt.Errorf("tz is required (an IANA timezone name)")
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, zero, zero, fmt.Errorf("invalid tz %q", tz)
	}
	if len(calIDs) < 1 || len(calIDs) > maxCalendars {
		return nil, zero, zero, fmt.Errorf("select between 1 and %d calendars", maxCalendars)
	}
	start, err := time.ParseInLocation("2006-01-02", startStr, loc)
	if err != nil {
		return nil, zero, zero, fmt.Errorf("start must be YYYY-MM-DD")
	}
	end, err := time.ParseInLocation("2006-01-02", endStr, loc)
	if err != nil {
		return nil, zero, zero, fmt.Errorf("end must be YYYY-MM-DD")
	}
	if !start.Before(end) {
		return nil, zero, zero, fmt.Errorf("start must be before end")
	}
	if end.After(start.AddDate(0, maxSpanMonths, 0)) {
		return nil, zero, zero, fmt.Errorf("date range too large (max %d months)", maxSpanMonths)
	}
	return loc, start, end, nil
}

// buildHeatmap fetches each calendar (bounded-concurrent, under a deadline), filters
// to busy events, dedupes the app's cross-calendar copies, and buckets each instance
// into per-local-day segments. Calendars that error or time out become warnings.
func buildHeatmap(ctx context.Context, cal *calendar.Client, token string, calIDs []string, start, end time.Time, loc *time.Location) heatmapResult {
	res := heatmapResult{WeekdayTotals: weekdayTotals(start, end, loc)}

	fctx, cancel := context.WithTimeout(ctx, fetchDeadline)
	defer cancel()

	type fetchOut struct {
		calID  string
		events []calendar.GCalEvent
		err    error
	}
	outs := make([]fetchOut, len(calIDs))
	g, gctx := errgroup.WithContext(fctx)
	g.SetLimit(fetchConcurrency)
	for i, cid := range calIDs {
		g.Go(func() error {
			r, err := cal.ListEventsFields(gctx, token, cid, start, end, eventFields)
			if err != nil {
				outs[i] = fetchOut{calID: cid, err: err}
				return nil // collect as a warning; don't cancel the group
			}
			outs[i] = fetchOut{calID: cid, events: r.Events}
			return nil
		})
	}
	_ = g.Wait()

	startKey := start.Format("2006-01-02")
	endKey := end.Format("2006-01-02")

	// Collect fetch failures first, so a later truncation can't drop them.
	for _, o := range outs {
		if o.err != nil {
			res.Warnings = append(res.Warnings, warning{o.calID, o.err.Error()})
		}
	}

	seen := map[string]bool{} // canonical id -> already counted (dedupe cross-calendar copies)
	for _, o := range outs {
		if o.err != nil {
			continue
		}
		for _, ev := range o.events {
			if !includeEvent(ev) {
				continue
			}
			// Dedupe only on a known canonical id — a missing id must not erase the rest.
			if cid := canonicalID(ev); cid != "" {
				if seen[cid] {
					continue
				}
				seen[cid] = true
			}
			for _, s := range eventSegments(ev, loc, startKey, endKey) {
				res.Segments = append(res.Segments, s)
				if len(res.Segments) >= maxSegments {
					res.Warnings = append(res.Warnings, warning{"", fmt.Sprintf("too many events (≥%d) — narrow the date range or calendar selection", maxSegments)})
					return finalize(res)
				}
			}
		}
	}
	return finalize(res)
}

func finalize(res heatmapResult) heatmapResult {
	if res.Segments == nil {
		res.Segments = []segment{}
	}
	if res.Warnings == nil {
		res.Warnings = []warning{}
	}
	return res
}

// includeEvent applies the stable "is this busy" filters (view filters are client-side).
func includeEvent(ev calendar.GCalEvent) bool {
	if ev.Start.DateTime == "" || ev.End.DateTime == "" {
		return false // all-day (date only) or malformed
	}
	if ev.Status == "cancelled" {
		return false
	}
	if ev.Transparency == "transparent" {
		return false
	}
	et := ev.EventType
	if et == "" {
		et = "default"
	}
	if !countedEventTypes[et] {
		return false
	}
	for _, a := range ev.Attendees {
		if a.Self && a.ResponseStatus == "declined" {
			return false
		}
	}
	return true
}

// canonicalID collapses the app's cross-calendar copies of one meeting: a placeholder
// resolves to its source event, everything else to its own id.
func canonicalID(ev calendar.GCalEvent) string {
	if app.IsPlaceholder(ev) {
		if src := app.SourceEventID(ev); src != "" {
			return src
		}
	}
	return ev.ID
}

// eventSegments splits a timed instance into per-local-day segments in loc, clipped to
// [startKey, endKey). Minutes are wall-clock (Hour*60+Minute), correct across DST.
func eventSegments(ev calendar.GCalEvent, loc *time.Location, startKey, endKey string) []segment {
	evStart, err1 := time.Parse(time.RFC3339, ev.Start.DateTime)
	evEnd, err2 := time.Parse(time.RFC3339, ev.End.DateTime)
	if err1 != nil || err2 != nil || !evStart.Before(evEnd) {
		return nil
	}
	evStart, evEnd = evStart.In(loc), evEnd.In(loc)
	title, recurring := ev.Summary, ev.RecurringEventId != ""

	var out []segment
	day := time.Date(evStart.Year(), evStart.Month(), evStart.Day(), 0, 0, 0, 0, loc)
	for day.Before(evEnd) {
		nextDay := time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, loc)
		segStart, segEnd := laterTime(evStart, day), earlierTime(evEnd, nextDay)
		dkey := day.Format("2006-01-02")
		if segStart.Before(segEnd) && dkey >= startKey && dkey < endKey {
			sMin := segStart.Hour()*60 + segStart.Minute()
			eMin := 1440
			if !segEnd.Equal(nextDay) {
				eMin = segEnd.Hour()*60 + segEnd.Minute()
			}
			if eMin > sMin {
				out = append(out, segment{T: title, R: recurring, W: int(day.Weekday()), D: dkey, S: sMin, E: eMin})
			}
		}
		day = nextDay
	}
	return out
}

// weekdayTotals counts how many of each weekday fall in [start, end).
func weekdayTotals(start, end time.Time, loc *time.Location) [7]int {
	var wt [7]int
	for d := start; d.Before(end); d = time.Date(d.Year(), d.Month(), d.Day()+1, 0, 0, 0, 0, loc) {
		wt[int(d.Weekday())]++
	}
	return wt
}

func laterTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlierTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
