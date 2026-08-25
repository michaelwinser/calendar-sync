// Package tools is the calendar Tools module: bulk search and delete of events.
// It owns no store data; it operates directly on the Calendar API on behalf of the
// logged-in user, plus (for the sync-only filter) sync's exported placeholder query.
package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/michaelwinser/appbase"
	"github.com/michaelwinser/appbase/auth"
	"github.com/michaelwinser/appbase/server"

	"github.com/michaelwinser/calendar-sync/internal/app"
	"github.com/michaelwinser/calendar-sync/internal/platform"
	"github.com/michaelwinser/calendar-sync/internal/platform/calendar"
)

type module struct {
	cal    *calendar.Client
	google *auth.GoogleAuth
}

// RegisterRoutes mounts the Tools API routes.
func RegisterRoutes(deps platform.Deps) error {
	m := &module{cal: deps.Cal, google: deps.Google}
	deps.Router.Get("/api/tools/search-events", m.searchEvents)
	deps.Router.Post("/api/tools/delete-events", m.bulkDelete)
	return nil
}

// RegisterPages mounts the Tools page.
func RegisterPages(deps platform.Deps) {
	deps.Router.Get("/tools", deps.LoginPage(page))
}

// searchResult is a single event returned by searchEvents.
type searchResult struct {
	ID       string `json:"id"`
	Summary  string `json:"summary"`
	Start    string `json:"start"`
	End      string `json:"end"`
	Location string `json:"location,omitempty"`
}

// searchEvents searches a calendar for events matching filters.
// Query params: calendarId, timeMin, timeMax, q (title substring), syncOnly.
func (m *module) searchEvents(w http.ResponseWriter, r *http.Request) {
	if appbase.UserID(r) == "" {
		server.RespondError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	token, err := platform.AccessToken(r, m.google)
	if err != nil {
		server.RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	calendarID := r.URL.Query().Get("calendarId")
	if calendarID == "" {
		server.RespondError(w, http.StatusBadRequest, "calendarId is required")
		return
	}
	timeMinStr := r.URL.Query().Get("timeMin")
	timeMaxStr := r.URL.Query().Get("timeMax")
	query := r.URL.Query().Get("q")
	if timeMinStr == "" || timeMaxStr == "" {
		server.RespondError(w, http.StatusBadRequest, "timeMin and timeMax are required")
		return
	}
	timeMin, err := time.Parse("2006-01-02", timeMinStr)
	if err != nil {
		server.RespondError(w, http.StatusBadRequest, "timeMin must be YYYY-MM-DD")
		return
	}
	timeMax, err := time.Parse("2006-01-02", timeMaxStr)
	if err != nil {
		server.RespondError(w, http.StatusBadRequest, "timeMax must be YYYY-MM-DD")
		return
	}
	timeMax = timeMax.Add(24 * time.Hour) // make timeMax inclusive of the full day

	syncOnly := r.URL.Query().Get("syncOnly") == "true"

	var events []calendar.GCalEvent
	if syncOnly {
		// Fetch all sync-engine placeholders (via sync's exported query), then filter
		// by date client-side (privateExtendedProperty + timeMin/timeMax don't combine
		// reliably in the API).
		all, err := app.ListSyncPlaceholders(r.Context(), m.cal, token, calendarID)
		if err != nil {
			server.RespondError(w, http.StatusBadGateway, "Google Calendar API: "+err.Error())
			return
		}
		minDate := timeMin.Format("2006-01-02")
		maxDate := timeMax.Format("2006-01-02")
		for _, e := range all {
			startDate := e.Start.Date
			if startDate == "" && e.Start.DateTime != "" {
				if t, err := time.Parse(time.RFC3339, e.Start.DateTime); err == nil {
					startDate = t.Format("2006-01-02")
				}
			}
			if startDate == "" {
				continue
			}
			if startDate >= minDate && startDate < maxDate {
				events = append(events, e)
			}
		}
	} else {
		res, err := m.cal.ListEvents(r.Context(), token, calendarID, timeMin, timeMax)
		if err != nil {
			server.RespondError(w, http.StatusBadGateway, "Google Calendar API: "+err.Error())
			return
		}
		events = res.Events
	}

	// Filter by title substring (case-insensitive).
	var results []searchResult
	queryLower := strings.ToLower(query)
	for _, e := range events {
		if e.Status == "cancelled" {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(e.Summary), queryLower) {
			continue
		}
		start := e.Start.DateTime
		if start == "" {
			start = e.Start.Date
		}
		end := e.End.DateTime
		if end == "" {
			end = e.End.Date
		}
		results = append(results, searchResult{
			ID:       e.ID,
			Summary:  e.Summary,
			Start:    start,
			End:      end,
			Location: e.Location,
		})
	}
	if results == nil {
		results = []searchResult{}
	}
	server.RespondJSON(w, http.StatusOK, results)
}

// bulkDeleteRequest is the JSON body for bulkDelete.
type bulkDeleteRequest struct {
	CalendarID string   `json:"calendarId"`
	EventIDs   []string `json:"eventIds"`
}

// bulkDelete deletes multiple events from a calendar.
func (m *module) bulkDelete(w http.ResponseWriter, r *http.Request) {
	if appbase.UserID(r) == "" {
		server.RespondError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	token, err := platform.AccessToken(r, m.google)
	if err != nil {
		server.RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	var req bulkDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		server.RespondError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.CalendarID == "" || len(req.EventIDs) == 0 {
		server.RespondError(w, http.StatusBadRequest, "calendarId and eventIds are required")
		return
	}

	deleted, errors := m.cal.BatchDeleteEvents(r.Context(), token, req.CalendarID, req.EventIDs)
	server.RespondJSON(w, http.StatusOK, map[string]any{
		"deleted": deleted,
		"errors":  errors,
		"message": fmt.Sprintf("Deleted %d events (%d errors)", deleted, errors),
	})
}
