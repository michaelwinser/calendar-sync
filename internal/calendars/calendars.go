// Package calendars is a tiny shared module that exposes the user's Google
// Calendar list at GET /api/calendars. It owns no store data; it reads directly
// from the Calendar API on behalf of the logged-in user.
//
// The calendar list is not sync-specific — the sync UI, Tools, and Heatmap pages
// all need it to populate calendar pickers — so it lives in its own module rather
// than being owned by any one feature.
package calendars

import (
	"net/http"

	"github.com/michaelwinser/appbase"
	"github.com/michaelwinser/appbase/server"

	"github.com/michaelwinser/calendar-sync/internal/platform"
	"github.com/michaelwinser/calendar-sync/internal/platform/calendar"
)

type module struct {
	cal *calendar.Client
}

// RegisterRoutes mounts the shared calendar-list API route.
func RegisterRoutes(deps platform.Deps) error {
	m := &module{cal: deps.Cal}
	deps.Router.Get("/api/calendars", m.listCalendars)
	return nil
}

// listCalendars fetches the user's Google Calendar list.
func (m *module) listCalendars(w http.ResponseWriter, r *http.Request) {
	if appbase.UserID(r) == "" {
		server.RespondError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	token, err := platform.AccessToken(r)
	if err != nil {
		server.RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	list, err := m.cal.ListCalendars(r.Context(), token)
	if err != nil {
		server.RespondError(w, http.StatusBadGateway, "Google Calendar API: "+err.Error())
		return
	}

	server.RespondJSON(w, http.StatusOK, list)
}
