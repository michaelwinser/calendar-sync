// Package calendartest provides a stateful in-memory fake of the Google Calendar
// API for driving behavioural sync tests. It implements the exact HTTP surface
// internal/platform/calendar.Client uses — calendar list, events list (window,
// incremental-by-sync-token, and privateExtendedProperty filters), create, patch,
// delete, and the multipart batch-delete — backed by in-memory calendars with a
// per-calendar change version so sync tokens replay a real delta log.
//
// It exists so Phases 2–3 of M8 (the migration and two-tier sync) can be tested
// end to end without touching Google. `./dev ci` has no behavioural sync coverage
// today; this is the seam that adds it.
//
// Scope notes:
//   - Events are single instances only (no recurrence expansion yet — a Phase 3
//     follow-up).
//   - The `fields` partial-response mask is ignored (the fake always returns full
//     events, which is correct for the client, and sync must never use a mask).
//   - A nextSyncToken is attached only to an unrestricted list (no timeMin/timeMax/
//     orderBy/privateExtendedProperty), matching Google — where those params are
//     mutually exclusive with sync tokens. The current client always sends a window,
//     so it never obtains a token and its incremental path is effectively dead; that's
//     the crux Phase 3 must fix. Tests obtain a starting token via SyncToken().
//   - Incremental deltas are NOT time-filtered: a sync token replays every change
//     regardless of window. This is deliberate — the M8 plan has the fast pass apply
//     the timeMin/timeMax window client-side, treating the token stream as unbounded
//     in time, so the fake exercises exactly that path.
//   - PATCH replaces the event rather than field-merging. Sync's callers always send
//     a fully-populated event, so this is faithful for them; don't rely on the fake
//     for partial-patch semantics.
//   - Create/patch to an unknown calendar returns 404 (so a placeholder written to
//     the wrong hub surfaces). SeedEvent, by contrast, creates the calendar if needed
//     and may pin Status/Updated (to set up unchanged-event or clock-skew cases).
//   - Read/write operations are counted per type via Counts() so a test can assert
//     "the fast pass did one incremental call per source, zero full lists".
package calendartest

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/michaelwinser/calendar-sync/internal/platform/calendar"
)

// baseTime anchors the fake's deterministic "updated" timestamps. Each mutation
// bumps a global version and stamps updated = baseTime + version seconds, so event
// Updated values are ordered and reproducible across runs.
var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Counts tallies operations the fake has served, by type. Reads is the sum of the
// list-style read calls (Calendars + WindowList + FullSyncList + Incremental +
// PropertyList) — the Google-side analogue of the Firestore read count the cost work
// targets.
type Counts struct {
	Calendars    int // GET /users/me/calendarList
	WindowList   int // GET events with timeMin/timeMax (windowed read)
	FullSyncList int // GET events unrestricted (no window/filter) — the token bootstrap
	Incremental  int // GET events with syncToken
	PropertyList int // GET events with privateExtendedProperty filter
	Create       int
	Update       int
	Delete       int // single + per-item batch deletes
}

// Reads returns the total list-style read calls served.
func (c Counts) Reads() int {
	return c.Calendars + c.WindowList + c.FullSyncList + c.Incremental + c.PropertyList
}

type fakeEvent struct {
	ev      calendar.GCalEvent
	version int  // change version at which this event was last created/updated/deleted
	deleted bool // tombstone: still reported (as cancelled) to incremental syncs
}

type fakeCal struct {
	id        string
	name      string
	primary   bool
	events    map[string]*fakeEvent
	version   int // monotonic per-calendar change counter; also the sync-token value
	tokenBase int // sync tokens ≤ tokenBase are expired (410). ExpireTokens() bumps it.
}

// Fake is an in-memory Google Calendar API served over httptest. Construct with
// New, seed with AddCalendar/SeedEvent, point a client at BaseURL()/Client(), and
// inspect with Events/Counts. Safe for concurrent use.
type Fake struct {
	srv *httptest.Server

	mu         sync.Mutex
	cals       map[string]*fakeCal
	nextID     int
	counts     Counts
	failDelete map[string]bool // "calID\x00eventID" → delete returns 403 (test knob)
	pageSize   int             // 0 = no cap (single page); >0 forces paging (test knob)
}

// New starts a fake and returns it. Call Close when done.
func New() *Fake {
	f := &Fake{cals: map[string]*fakeCal{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.route))
	return f
}

// Close shuts down the underlying test server.
func (f *Fake) Close() { f.srv.Close() }

// BaseURL is the value to pass to calendar.New (or Client.BaseURL). The client
// derives BatchURL as BaseURL+"/batch", which this fake also serves.
func (f *Fake) BaseURL() string { return f.srv.URL }

// Client returns a calendar.Client wired to this fake.
func (f *Fake) Client() *calendar.Client { return calendar.New(f.srv.URL) }

// Counts returns a snapshot of operations served so far.
func (f *Fake) Counts() Counts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts
}

// ResetCounts zeroes the operation counters (e.g. between sync passes in a test).
func (f *Fake) ResetCounts() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts = Counts{}
}

// AddCalendar registers a calendar so it appears in the calendar list.
func (f *Fake) AddCalendar(id, name string, primary bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cals[id] = &fakeCal{id: id, name: name, primary: primary, events: map[string]*fakeEvent{}}
}

// SeedEvent inserts an event directly (bypassing the create endpoint and its counter).
// A blank ev.ID is assigned one. Returns the stored event's ID.
func (f *Fake) SeedEvent(calID string, ev calendar.GCalEvent) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.mustCal(calID)
	return f.putLocked(c, ev)
}

// Events returns a copy of the live (non-deleted) events on a calendar.
func (f *Fake) Events(calID string) []calendar.GCalEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cals[calID]
	if c == nil {
		return nil
	}
	var out []calendar.GCalEvent
	for _, fe := range c.events {
		if !fe.deleted {
			out = append(out, clone(fe.ev))
		}
	}
	sortByStart(out)
	return out
}

// sortByStart orders events by start instant then ID, so results are reproducible
// (map iteration is randomized) and mirror the client's orderBy=startTime request.
func sortByStart(evs []calendar.GCalEvent) {
	sort.SliceStable(evs, func(i, j int) bool {
		si, _ := eventInstant(evs[i].Start)
		sj, _ := eventInstant(evs[j].Start)
		if !si.Equal(sj) {
			return si.Before(sj)
		}
		return evs[i].ID < evs[j].ID
	})
}

// ExpireTokens invalidates all currently-issued sync tokens for a calendar; the next
// incremental fetch with an old token gets 410 (drives the full-pass repair path).
// It bumps version first so a token issued at the current version also expires — else
// expiring right after a read (with no intervening mutation) would be a silent no-op.
func (f *Fake) ExpireTokens(calID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.cals[calID]; c != nil {
		c.version++
		c.tokenBase = c.version
	}
}

// FailDelete makes every delete (single or batch) of the given event on the given
// calendar return 403 — a permanent failure — so a test can verify the sync engine
// keeps the mapping rather than orphaning the placeholder.
func (f *Fake) FailDelete(calID, eventID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDelete == nil {
		f.failDelete = map[string]bool{}
	}
	f.failDelete[calID+"\x00"+eventID] = true
}

// AllowDelete undoes a prior FailDelete for the given event.
func (f *Fake) AllowDelete(calID, eventID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.failDelete, calID+"\x00"+eventID)
}

// SetPageSize caps how many events each list response returns, forcing the client to
// page (n ≤ 0 restores single-page behaviour). Lets a test exercise the paging loops
// with a handful of events instead of thousands.
func (f *Fake) SetPageSize(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pageSize = n
}

// SyncToken returns the calendar's current sync token, as an unrestricted full-sync
// list would. It's a test hook: the current client only issues windowed reads, which
// Google (and this fake) don't attach a token to, so a test that wants to exercise the
// incremental endpoint obtains its starting token here. Empty for an unknown calendar.
func (f *Fake) SyncToken(calID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.cals[calID]; c != nil {
		return c.token()
	}
	return ""
}

// --- internal state helpers (caller holds f.mu) ---

func (f *Fake) mustCal(calID string) *fakeCal {
	c := f.cals[calID]
	if c == nil {
		c = &fakeCal{id: calID, events: map[string]*fakeEvent{}}
		f.cals[calID] = c
	}
	return c
}

func (f *Fake) putLocked(c *fakeCal, ev calendar.GCalEvent) string {
	c.version++
	if ev.ID == "" {
		f.nextID++
		ev.ID = fmt.Sprintf("evt-%d", f.nextID)
	}
	// Live create/update paths clear Status/Updated so they always restamp; SeedEvent
	// may pin them (to set up unchanged-event / clock-skew cases), so preserve non-empty.
	if ev.Status == "" {
		ev.Status = "confirmed"
	}
	if ev.Updated == "" {
		ev.Updated = baseTime.Add(time.Duration(c.version) * time.Second).Format(time.RFC3339)
	}
	c.events[ev.ID] = &fakeEvent{ev: clone(ev), version: c.version}
	return ev.ID
}

// clone deep-copies an event via a JSON round-trip so stored state never aliases the
// caller's nested pointers/slices (ExtendedProperties, Attendees, RawMessage fields).
func clone(ev calendar.GCalEvent) calendar.GCalEvent {
	b, _ := json.Marshal(ev)
	var out calendar.GCalEvent
	_ = json.Unmarshal(b, &out)
	return out
}

func (c *fakeCal) token() string { return "tok-" + strconv.Itoa(c.version) }

// --- HTTP routing ---

func (f *Fake) route(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/batch" {
		f.handleBatch(w, r)
		return
	}
	if r.URL.Path == "/users/me/calendarList" {
		f.handleCalendarList(w, r)
		return
	}
	// /calendars/{calID}/events  and  /calendars/{calID}/events/{eventID}
	const p = "/calendars/"
	if rest, ok := strings.CutPrefix(r.URL.Path, p); ok {
		i := strings.Index(rest, "/events")
		if i < 0 {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		calID := unescape(rest[:i])
		tail := strings.TrimPrefix(rest[i:], "/events")
		switch {
		case tail == "" || tail == "/": // collection
			if r.Method == http.MethodPost {
				f.handleCreate(w, r, calID)
			} else {
				f.handleList(w, r, calID)
			}
		default: // /{eventID}
			eventID := unescape(strings.TrimPrefix(tail, "/"))
			switch r.Method {
			case http.MethodPatch:
				f.handlePatch(w, r, calID, eventID)
			case http.MethodDelete:
				f.handleDelete(w, r, calID, eventID)
			default:
				http.Error(w, "not found", http.StatusNotFound)
			}
		}
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func unescape(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

func (f *Fake) handleCalendarList(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts.Calendars++
	type item struct {
		ID         string `json:"id"`
		Summary    string `json:"summary"`
		Primary    bool   `json:"primary"`
		AccessRole string `json:"accessRole"`
	}
	var items []item
	for _, c := range f.cals {
		items = append(items, item{ID: c.id, Summary: c.name, Primary: c.primary, AccessRole: "owner"})
	}
	writeJSON(w, map[string]any{"items": items})
}

func (f *Fake) handleList(w http.ResponseWriter, r *http.Request, calID string) {
	q := r.URL.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cals[calID]
	if c == nil {
		http.Error(w, "calendar not found", http.StatusNotFound)
		return
	}

	// Incremental: syncToken present → delta since that version, incl. tombstones,
	// ordered by change version (the order Google reports changes in).
	if _, hasSyncToken := q["syncToken"]; hasSyncToken {
		st := q.Get("syncToken")
		if st == "" {
			apiError(w, http.StatusBadRequest, "badRequest", "syncToken must not be empty.")
			return
		}
		// Sync tokens are mutually exclusive with these params (real Google → 400).
		if q.Get("timeMin") != "" || q.Get("timeMax") != "" || q.Get("orderBy") != "" || len(q["privateExtendedProperty"]) > 0 {
			apiError(w, http.StatusBadRequest, "badRequest",
				"syncToken cannot be combined with timeMin/timeMax/orderBy/privateExtendedProperty.")
			return
		}
		f.counts.Incremental++
		since, ok := parseToken(st)
		if !ok || since < c.tokenBase {
			apiError(w, http.StatusGone, "fullSyncRequired", "Sync token is no longer valid.")
			return
		}
		var changed []*fakeEvent
		for _, fe := range c.events {
			if fe.version > since {
				changed = append(changed, fe)
			}
		}
		sort.SliceStable(changed, func(i, j int) bool { return changed[i].version < changed[j].version })
		items := make([]calendar.GCalEvent, 0, len(changed))
		for _, fe := range changed {
			items = append(items, eventForDelta(fe))
		}
		f.emitPageLocked(w, c, items, q, true)
		return
	}

	// Property filter vs. window read.
	privateFilters := q["privateExtendedProperty"]
	timeMin, hasMin := parseTime(q.Get("timeMin"))
	timeMax, hasMax := parseTime(q.Get("timeMax"))
	switch {
	case len(privateFilters) > 0:
		f.counts.PropertyList++
	case hasMin || hasMax:
		f.counts.WindowList++
	default:
		f.counts.FullSyncList++ // unrestricted read — the token-establishing bootstrap
	}

	var items []calendar.GCalEvent
	for _, fe := range c.events {
		if fe.deleted {
			continue
		}
		if !matchesPrivate(fe.ev, privateFilters) {
			continue
		}
		if hasMin || hasMax {
			if !inWindow(fe.ev, timeMin, hasMin, timeMax, hasMax) {
				continue
			}
		}
		items = append(items, clone(fe.ev))
	}
	sortByStart(items)
	// A sync token is attached only to an UNRESTRICTED list (no timeMin/timeMax/orderBy/
	// privateExtendedProperty — all mutually exclusive with sync tokens in Google's API).
	// The windowed ListEvents therefore never gets one; ListEventsForSync is the read that
	// does. See the package doc and calendar.ListEventsForSync.
	issueToken := len(privateFilters) == 0 && !hasMin && !hasMax && q.Get("orderBy") == ""
	f.emitPageLocked(w, c, items, q, issueToken)
}

// emitPageLocked writes one page of an ordered result set, honoring the client's
// maxResults and the fake's page-size cap. A sync token (when issueToken) is attached
// only to the LAST page — matching Google, where nextSyncToken and nextPageToken never
// co-occur. Page tokens encode the offset ("page-<n>"); the client resends the base
// query each page, so recomputing and re-slicing the (deterministically ordered) set is
// stable. Caller holds f.mu.
func (f *Fake) emitPageLocked(w http.ResponseWriter, c *fakeCal, items []calendar.GCalEvent, q url.Values, issueToken bool) {
	pageSize := len(items)
	if pageSize == 0 {
		pageSize = 1
	}
	if f.pageSize > 0 && f.pageSize < pageSize {
		pageSize = f.pageSize
	}
	if mr, err := strconv.Atoi(q.Get("maxResults")); err == nil && mr > 0 && mr < pageSize {
		pageSize = mr
	}
	offset := 0
	if n, err := strconv.Atoi(strings.TrimPrefix(q.Get("pageToken"), "page-")); err == nil && n > 0 {
		offset = n
	}
	offset = min(offset, len(items))
	end := min(offset+pageSize, len(items))

	resp := map[string]any{"items": items[offset:end]}
	switch {
	case end < len(items):
		resp["nextPageToken"] = "page-" + strconv.Itoa(end)
	case issueToken:
		resp["nextSyncToken"] = c.token()
	}
	writeJSON(w, resp)
}

func (f *Fake) handleCreate(w http.ResponseWriter, r *http.Request, calID string) {
	var ev calendar.GCalEvent
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts.Create++
	c := f.cals[calID]
	if c == nil {
		apiError(w, http.StatusNotFound, "notFound", "Calendar not found.")
		return
	}
	ev.ID = ""      // server assigns
	ev.Status = ""  // live create is always confirmed + freshly stamped
	ev.Updated = "" //
	id := f.putLocked(c, ev)
	writeJSON(w, c.events[id].ev)
}

func (f *Fake) handlePatch(w http.ResponseWriter, r *http.Request, calID, eventID string) {
	var patch calendar.GCalEvent
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts.Update++
	c := f.cals[calID]
	if c == nil || c.events[eventID] == nil || c.events[eventID].deleted {
		apiError(w, http.StatusNotFound, "notFound", "Event not found.")
		return
	}
	patch.ID = eventID
	patch.Status = ""  // a live update restamps Status/Updated (see #6 in scope notes)
	patch.Updated = "" //
	f.putLocked(c, patch)
	writeJSON(w, c.events[eventID].ev)
}

func (f *Fake) handleDelete(w http.ResponseWriter, _ *http.Request, calID, eventID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts.Delete++
	if f.failDelete[calID+"\x00"+eventID] {
		apiError(w, http.StatusForbidden, "forbidden", "Delete forbidden (test knob).")
		return
	}
	if f.deleteLocked(calID, eventID) {
		w.WriteHeader(http.StatusNoContent)
	} else {
		apiError(w, http.StatusNotFound, "notFound", "Event not found.")
	}
}

// deleteLocked tombstones an event. Returns false if it was absent/already gone.
func (f *Fake) deleteLocked(calID, eventID string) bool {
	c := f.cals[calID]
	if c == nil {
		return false
	}
	fe := c.events[eventID]
	if fe == nil || fe.deleted {
		return false
	}
	c.version++
	fe.deleted = true
	fe.version = c.version
	fe.ev.Status = "cancelled"
	return true
}

// handleBatch parses a multipart/mixed batch of DELETE sub-requests and returns a
// multipart response with a per-item HTTP status line (204 or 404), matching the
// format calendar.Client.doBatchDelete parses.
func (f *Fake) handleBatch(w http.ResponseWriter, r *http.Request) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		http.Error(w, "bad content-type", http.StatusBadRequest)
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])

	respBoundary := "batch_resp_" + strconv.Itoa(int(time.Now().UnixNano()))
	var out strings.Builder
	f.mu.Lock()
	item := 0
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		sub, _ := io.ReadAll(part)
		calID, eventID, ok := parseBatchDelete(string(sub))
		status := "404 Not Found"
		if ok {
			f.counts.Delete++
			switch {
			case f.failDelete[calID+"\x00"+eventID]:
				status = "403 Forbidden"
			case f.deleteLocked(calID, eventID):
				status = "204 No Content"
			}
		}
		// Echo a Content-ID (parts are emitted in request order, so item index matches)
		// to exercise the client's Content-ID→event mapping.
		out.WriteString("--" + respBoundary + "\r\n")
		out.WriteString("Content-Type: application/http\r\n")
		fmt.Fprintf(&out, "Content-ID: <response-item%d>\r\n\r\n", item)
		out.WriteString("HTTP/1.1 " + status + "\r\n\r\n")
		item++
	}
	f.mu.Unlock()
	out.WriteString("--" + respBoundary + "--\r\n")

	w.Header().Set("Content-Type", "multipart/mixed; boundary="+respBoundary)
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, out.String())
}

// --- matching / parsing helpers ---

// eventForDelta returns the event as an incremental sync would report it: a live
// event as-is, a tombstone with status=cancelled and no other fields guaranteed.
func eventForDelta(fe *fakeEvent) calendar.GCalEvent {
	if fe.deleted {
		return calendar.GCalEvent{ID: fe.ev.ID, Status: "cancelled", Updated: fe.ev.Updated}
	}
	return fe.ev
}

func matchesPrivate(ev calendar.GCalEvent, filters []string) bool {
	for _, f := range filters {
		k, v, _ := strings.Cut(f, "=")
		if ev.ExtendedProperties == nil || ev.ExtendedProperties.Private[k] != v {
			return false
		}
	}
	return true
}

// inWindow reports whether an event overlaps [min,max). Uses start/end dateTime or
// all-day date; an event with an unparseable time is included (conservative).
func inWindow(ev calendar.GCalEvent, min time.Time, hasMin bool, max time.Time, hasMax bool) bool {
	start, okS := eventInstant(ev.Start)
	end, okE := eventInstant(ev.End)
	if !okS || !okE {
		return true
	}
	if hasMax && !start.Before(max) {
		return false
	}
	if hasMin && !end.After(min) {
		return false
	}
	return true
}

func eventInstant(t calendar.EventTime) (time.Time, bool) {
	if t.DateTime != "" {
		if v, err := time.Parse(time.RFC3339, t.DateTime); err == nil {
			return v, true
		}
	}
	if t.Date != "" {
		if v, err := time.Parse("2006-01-02", t.Date); err == nil {
			return v, true
		}
	}
	return time.Time{}, false
}

func parseToken(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(s, "tok-"))
	return n, err == nil
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	v, err := time.Parse(time.RFC3339, s)
	return v, err == nil
}

// parseBatchDelete extracts the calendar and event IDs from one batch sub-request
// whose first non-header line is "DELETE /calendar/v3/calendars/{cal}/events/{id}?…".
func parseBatchDelete(sub string) (calID, eventID string, ok bool) {
	for line := range strings.SplitSeq(sub, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "DELETE ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return "", "", false
		}
		path := fields[1]
		if q := strings.IndexByte(path, '?'); q >= 0 {
			path = path[:q]
		}
		path = strings.TrimPrefix(path, "/calendar/v3/calendars/")
		before, after, ok := strings.Cut(path, "/events/")
		if !ok {
			return "", "", false
		}
		return unescape(before), unescape(after), true
	}
	return "", "", false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// apiError writes a Google-style error body so the client's errorReason() can read
// the reason (used to classify retryable vs. permanent failures).
func apiError(w http.ResponseWriter, status int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":    status,
			"message": message,
			"errors":  []map[string]any{{"reason": reason, "message": message}},
		},
	})
}
