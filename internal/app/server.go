package app

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/michaelwinser/appbase"
	"github.com/michaelwinser/appbase/auth"
	"github.com/michaelwinser/appbase/server"
)

// Server handles API requests.
type Server struct {
	Store  *Store
	Google *auth.GoogleAuth
}

// getAccessToken returns the OAuth access token, refreshing if expired.
func getAccessToken(r *http.Request, google *auth.GoogleAuth) (string, error) {
	token := appbase.AccessToken(r)
	if token == "" {
		return "", fmt.Errorf("no Google API access token — re-login to grant Calendar permission")
	}

	// Attempt refresh if expired
	expiry := auth.TokenExpiry(r)
	if !expiry.IsZero() && time.Now().After(expiry) && google != nil {
		refreshToken := auth.RefreshToken(r)
		if refreshToken != "" {
			session := &auth.Session{
				AccessToken:  token,
				RefreshToken: refreshToken,
				TokenExpiry:  expiry,
			}
			newToken, err := google.RefreshAccessToken(r.Context(), session)
			if err != nil {
				log.Printf("token refresh failed: %v", err)
				// Return expired token; caller will get 401 from Google
				return token, nil
			}
			return newToken, nil
		}
	}

	return token, nil
}

// RegisterRoutes adds all API routes to the router.
func (s *Server) RegisterRoutes(r chi.Router) {
	r.Get("/api/calendars", s.ListCalendars)
	r.Get("/api/config", s.GetConfig)
	r.Put("/api/config", s.PutConfig)
	r.Post("/api/sync", s.TriggerSync)
	// NudgeSync is registered separately at /sync/nudge (not under /api/)
	// to bypass the session auth middleware. It does its own auth.
	r.Get("/api/sync/logs", s.ListSyncLogs)
	r.Get("/api/sync/events", s.ListSyncedEvents)
	r.Get("/api/status", s.Status)
	r.Get("/api/tools/search-events", s.SearchEvents)
	r.Post("/api/tools/delete-events", s.BulkDeleteEvents)
}

// Status returns the authenticated user's status.
func (s *Server) Status(w http.ResponseWriter, r *http.Request) {
	userID := appbase.UserID(r)
	if userID == "" {
		server.RespondError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	server.RespondJSON(w, http.StatusOK, map[string]interface{}{
		"email":  appbase.Email(r),
		"status": "ok",
	})
}

// ListCalendars fetches the user's Google Calendar list.
func (s *Server) ListCalendars(w http.ResponseWriter, r *http.Request) {
	token, err := getAccessToken(r, s.Google)
	if err != nil {
		server.RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	calendars, err := ListCalendars(r.Context(), token)
	if err != nil {
		server.RespondError(w, http.StatusBadGateway, "Google Calendar API: "+err.Error())
		return
	}

	server.RespondJSON(w, http.StatusOK, calendars)
}

// configResponse is the JSON shape for GET /api/config.
type configResponse struct {
	HubCalendarID       string               `json:"hubCalendarId"`
	HubCalendarName     string               `json:"hubCalendarName"`
	SyncWindowWeeks     int                  `json:"syncWindowWeeks"`
	SyncIntervalMinutes int                  `json:"syncIntervalMinutes"`
	Sources             []sourceCalendarView `json:"sources"`
}

type sourceCalendarView struct {
	CalendarID   string `json:"calendarId"`
	CalendarName string `json:"calendarName"`
	EmojiPrefix  string `json:"emojiPrefix"`
	ColorID      string `json:"colorId"`
}

// GetConfig returns the user's sync configuration.
func (s *Server) GetConfig(w http.ResponseWriter, r *http.Request) {
	userID := appbase.UserID(r)
	if userID == "" {
		server.RespondError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	cfg, err := s.Store.GetConfig(userID)
	if err != nil {
		server.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	sources, err := s.Store.GetSources(userID)
	if err != nil {
		server.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := configResponse{
		SyncWindowWeeks:     8,
		SyncIntervalMinutes: 15,
		Sources:             make([]sourceCalendarView, 0, len(sources)),
	}
	if cfg != nil {
		resp.HubCalendarID = cfg.HubCalendarID
		resp.HubCalendarName = cfg.HubCalendarName
		resp.SyncWindowWeeks = cfg.SyncWindowWeeks
		resp.SyncIntervalMinutes = cfg.SyncIntervalMinutes
	}
	for _, src := range sources {
		resp.Sources = append(resp.Sources, sourceCalendarView{
			CalendarID:   src.CalendarID,
			CalendarName: src.CalendarName,
			EmojiPrefix:  src.EmojiPrefix,
			ColorID:      src.ColorID,
		})
	}

	server.RespondJSON(w, http.StatusOK, resp)
}

// configRequest is the JSON shape for PUT /api/config.
type configRequest struct {
	HubCalendarID       string                `json:"hubCalendarId"`
	HubCalendarName     string                `json:"hubCalendarName"`
	SyncWindowWeeks     int                   `json:"syncWindowWeeks"`
	SyncIntervalMinutes int                   `json:"syncIntervalMinutes"`
	Sources             []SourceCalendarInput `json:"sources"`
}

// PutConfig saves the user's full sync configuration.
func (s *Server) PutConfig(w http.ResponseWriter, r *http.Request) {
	userID := appbase.UserID(r)
	if userID == "" {
		server.RespondError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	var req configRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		server.RespondError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Defaults handled by SaveConfig

	// Validation: hub cannot be a source
	for _, src := range req.Sources {
		if src.CalendarID == req.HubCalendarID && req.HubCalendarID != "" {
			server.RespondError(w, http.StatusBadRequest, "hub calendar cannot also be a source calendar")
			return
		}
	}

	// Validation: no duplicate sources
	seen := make(map[string]bool, len(req.Sources))
	for _, src := range req.Sources {
		if seen[src.CalendarID] {
			server.RespondError(w, http.StatusBadRequest, "duplicate source calendar: "+src.CalendarID)
			return
		}
		seen[src.CalendarID] = true
	}

	// Save config
	cfg, err := s.Store.SaveConfig(userID, SaveConfigInput{
		HubCalendarID:       req.HubCalendarID,
		HubCalendarName:     req.HubCalendarName,
		SyncWindowWeeks:     req.SyncWindowWeeks,
		SyncIntervalMinutes: req.SyncIntervalMinutes,
	})
	if err != nil {
		server.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Reconcile sources
	sources, err := s.Store.ReconcileSources(userID, req.Sources)
	if err != nil {
		server.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := configResponse{
		HubCalendarID:       cfg.HubCalendarID,
		HubCalendarName:     cfg.HubCalendarName,
		SyncWindowWeeks:     cfg.SyncWindowWeeks,
		SyncIntervalMinutes: cfg.SyncIntervalMinutes,
		Sources:             make([]sourceCalendarView, 0, len(sources)),
	}
	for _, src := range sources {
		resp.Sources = append(resp.Sources, sourceCalendarView{
			CalendarID:   src.CalendarID,
			CalendarName: src.CalendarName,
			EmojiPrefix:  src.EmojiPrefix,
			ColorID:      src.ColorID,
		})
	}

	server.RespondJSON(w, http.StatusOK, resp)
}

// TriggerSync runs a sync pass for the authenticated user.
func (s *Server) TriggerSync(w http.ResponseWriter, r *http.Request) {
	userID := appbase.UserID(r)
	if userID == "" {
		server.RespondError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	// Check config before token — config validation doesn't need a token
	cfg, err := s.Store.GetConfig(userID)
	if err != nil || cfg == nil || cfg.HubCalendarID == "" {
		server.RespondError(w, http.StatusBadRequest, "sync not configured — set a hub calendar first")
		return
	}

	sources, err := s.Store.GetSources(userID)
	if err != nil {
		server.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(sources) == 0 {
		server.RespondError(w, http.StatusBadRequest, "no source calendars configured")
		return
	}

	token, err := getAccessToken(r, s.Google)
	if err != nil {
		server.RespondError(w, http.StatusForbidden, err.Error())
		return
	}

	// Store refresh token for background sync (nudge endpoint)
	if refreshToken := auth.RefreshToken(r); refreshToken != "" {
		if err := s.Store.UpdateRefreshToken(userID, refreshToken); err != nil {
			log.Printf("failed to store refresh token: %v", err)
		}
	}

	// Override sync window if days parameter is provided
	syncDays := cfg.SyncWindowWeeks * 7
	if daysStr := r.URL.Query().Get("days"); daysStr != "" {
		if days, err := strconv.Atoi(daysStr); err == nil && days > 0 {
			syncDays = days
		}
	}

	result, err := RunSyncWithDays(r.Context(), token, s.Store, cfg, sources, syncDays)
	if err != nil {
		server.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	server.RespondJSON(w, http.StatusOK, result)
}

// ListSyncLogs returns recent sync logs for the authenticated user.
func (s *Server) ListSyncLogs(w http.ResponseWriter, r *http.Request) {
	userID := appbase.UserID(r)
	if userID == "" {
		server.RespondError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	logs, err := s.Store.GetRecentSyncLogs(userID, 20)
	if err != nil {
		server.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if logs == nil {
		logs = []SyncLog{}
	}

	server.RespondJSON(w, http.StatusOK, logs)
}

// ListSyncedEvents returns synced event mappings for the authenticated user.
func (s *Server) ListSyncedEvents(w http.ResponseWriter, r *http.Request) {
	userID := appbase.UserID(r)
	if userID == "" {
		server.RespondError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	events, err := s.Store.GetSyncedEventsForUser(userID)
	if err != nil {
		server.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if events == nil {
		events = []SyncedEvent{}
	}

	server.RespondJSON(w, http.StatusOK, events)
}

// searchEventResult is a single event returned by SearchEvents.
type searchEventResult struct {
	ID       string `json:"id"`
	Summary  string `json:"summary"`
	Start    string `json:"start"`
	End      string `json:"end"`
	Location string `json:"location,omitempty"`
}

// SearchEvents searches for events on a calendar matching filters.
// Query params: calendarId, timeMin, timeMax, q (title substring)
func (s *Server) SearchEvents(w http.ResponseWriter, r *http.Request) {
	userID := appbase.UserID(r)
	if userID == "" {
		server.RespondError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	token, err := getAccessToken(r, s.Google)
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
	// Make timeMax inclusive of the full day
	timeMax = timeMax.Add(24 * time.Hour)

	syncOnly := r.URL.Query().Get("syncOnly") == "true"

	var events []GCalEvent
	if syncOnly {
		// Fetch only events created by our sync engine
		all, err := ListAllPlaceholders(r.Context(), token, calendarID)
		if err != nil {
			server.RespondError(w, http.StatusBadGateway, "Google Calendar API: "+err.Error())
			return
		}
		events = all
	} else {
		res, err := ListEvents(r.Context(), token, calendarID, timeMin, timeMax)
		if err != nil {
			server.RespondError(w, http.StatusBadGateway, "Google Calendar API: "+err.Error())
			return
		}
		events = res.Events
	}

	// Filter by title substring (case-insensitive)
	var results []searchEventResult
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
		results = append(results, searchEventResult{
			ID:       e.ID,
			Summary:  e.Summary,
			Start:    start,
			End:      end,
			Location: e.Location,
		})
	}

	if results == nil {
		results = []searchEventResult{}
	}
	server.RespondJSON(w, http.StatusOK, results)
}

// bulkDeleteRequest is the JSON body for BulkDeleteEvents.
type bulkDeleteRequest struct {
	CalendarID string   `json:"calendarId"`
	EventIDs   []string `json:"eventIds"`
}

// BulkDeleteEvents deletes multiple events from a calendar.
func (s *Server) BulkDeleteEvents(w http.ResponseWriter, r *http.Request) {
	userID := appbase.UserID(r)
	if userID == "" {
		server.RespondError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	token, err := getAccessToken(r, s.Google)
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

	deleted, errors := BatchDeleteEvents(r.Context(), token, req.CalendarID, req.EventIDs)

	server.RespondJSON(w, http.StatusOK, map[string]interface{}{
		"deleted": deleted,
		"errors":  errors,
		"message": fmt.Sprintf("Deleted %d events (%d errors)", deleted, errors),
	})
}

// NudgeSync triggers sync for all users who are due based on their schedule.
// Auth: X-Nudge-Key header required (matches SYNC_NUDGE_KEY env var).
// On Cloud Run, add OIDC at the infrastructure level as a second layer.
// Registered at /sync/nudge (not /api/) to bypass session auth middleware.
func (s *Server) NudgeSync(w http.ResponseWriter, r *http.Request) {
	// Auth: always require deployment key
	nudgeKey := os.Getenv("SYNC_NUDGE_KEY")
	if nudgeKey == "" {
		server.RespondError(w, http.StatusServiceUnavailable, "SYNC_NUDGE_KEY not configured")
		return
	}
	if r.Header.Get("X-Nudge-Key") != nudgeKey {
		server.RespondError(w, http.StatusUnauthorized, "invalid nudge key")
		return
	}

	configs, err := s.Store.GetAllConfigs()
	if err != nil {
		server.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	now := time.Now().UTC()
	synced, skipped, errors := 0, 0, 0

	for _, cfg := range configs {
		// Check if sync is due
		if cfg.LastSyncAt != "" {
			lastSync, err := time.Parse(time.RFC3339, cfg.LastSyncAt)
			if err == nil {
				interval := cfg.SyncIntervalMinutes
				if interval <= 0 {
					interval = 15
				}
				if now.Before(lastSync.Add(time.Duration(interval) * time.Minute)) {
					skipped++
					continue
				}
			}
		}

		// Check refresh token
		if cfg.RefreshToken == "" {
			log.Printf("nudge: user %s has no refresh token", cfg.UserID)
			errors++
			continue
		}

		// Get fresh access token
		if s.Google == nil {
			errors++
			continue
		}
		session := &auth.Session{RefreshToken: cfg.RefreshToken}
		token, err := s.Google.RefreshAccessToken(r.Context(), session)
		if err != nil {
			log.Printf("nudge: token refresh failed for user %s: %v", cfg.UserID, err)
			errors++
			continue
		}

		if session.RefreshToken != cfg.RefreshToken {
			s.Store.UpdateRefreshToken(cfg.UserID, session.RefreshToken)
		}

		sources, err := s.Store.GetSources(cfg.UserID)
		if err != nil || len(sources) == 0 {
			skipped++
			continue
		}

		syncDays := cfg.SyncWindowWeeks * 7
		if _, err := RunSyncWithDays(r.Context(), token, s.Store, &cfg, sources, syncDays); err != nil {
			log.Printf("nudge: sync failed for user %s: %v", cfg.UserID, err)
			errors++
			continue
		}
		synced++
	}

	server.RespondJSON(w, http.StatusOK, map[string]interface{}{
		"total":   len(configs),
		"synced":  synced,
		"skipped": skipped,
		"errors":  errors,
	})
}
