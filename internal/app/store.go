package app

import (
	"time"

	"github.com/google/uuid"
	"github.com/michaelwinser/appbase/db"
	"github.com/michaelwinser/appbase/store"
)

// SyncConfig holds a user's sync configuration. One per user.
// Uniqueness of user_id is enforced at the application layer.
type SyncConfig struct {
	ID                  string `json:"id"                  store:"id,pk"`
	UserID              string `json:"userId"              store:"user_id,index"`
	HubCalendarID       string `json:"hubCalendarId"       store:"hub_calendar_id"`
	HubCalendarName     string `json:"hubCalendarName"     store:"hub_calendar_name"`
	SyncWindowWeeks     int    `json:"syncWindowWeeks"     store:"sync_window_weeks"`
	SyncIntervalMinutes int    `json:"syncIntervalMinutes" store:"sync_interval_minutes"`
	RefreshToken        string `json:"-"                   store:"refresh_token"`
	LastSyncAt          string `json:"-"                   store:"last_sync_at"`
	CreatedAt           string `json:"createdAt"           store:"created_at"`
	UpdatedAt           string `json:"updatedAt"           store:"updated_at"`
}

// SourceCalendar represents a calendar selected for synchronization.
// Uniqueness of (user_id, calendar_id) is enforced at the application layer.
type SourceCalendar struct {
	ID           string `json:"id"           store:"id,pk"`
	UserID       string `json:"userId"       store:"user_id,index"`
	CalendarID   string `json:"calendarId"   store:"calendar_id"`
	CalendarName string `json:"calendarName" store:"calendar_name"`
	EmojiPrefix  string `json:"emojiPrefix"  store:"emoji_prefix"`
	ColorID      string `json:"colorId"      store:"color_id"`
	SyncToken    string `json:"-"            store:"sync_token"`
	CreatedAt    string `json:"createdAt"    store:"created_at"`
}

// SyncedEvent tracks the mapping between a source event and a placeholder event.
// Lookup is via Where("source_calendar_id", ...).All() with in-memory filtering
// on source_event_id. Compound indexes are not supported by appbase store.
type SyncedEvent struct {
	ID               string `json:"id"               store:"id,pk"`
	UserID           string `json:"userId"            store:"user_id,index"`
	SourceCalendarID string `json:"sourceCalendarId"  store:"source_calendar_id,index"`
	SourceEventID    string `json:"sourceEventId"     store:"source_event_id"`
	TargetCalendarID string `json:"targetCalendarId"  store:"target_calendar_id"`
	TargetEventID    string `json:"targetEventId"     store:"target_event_id"`
	SourceUpdated    string `json:"sourceUpdated"     store:"source_updated"`
	CreatedAt        string `json:"createdAt"         store:"created_at"`
	UpdatedAt        string `json:"updatedAt"         store:"updated_at"`
}

// SyncLog records the result of a sync pass.
type SyncLog struct {
	ID          string `json:"id"          store:"id,pk"`
	UserID      string `json:"userId"      store:"user_id,index"`
	StartedAt   string `json:"startedAt"   store:"started_at"`
	CompletedAt string `json:"completedAt" store:"completed_at"`
	Created     int    `json:"created"     store:"created"`
	Updated     int    `json:"updated"     store:"updated"`
	Deleted     int    `json:"deleted"     store:"deleted"`
	Errors      int    `json:"errors"      store:"errors"`
	Status      string `json:"status"      store:"status"`
	ErrorMsg    string `json:"errorMsg"    store:"error_msg"`
}

// Store provides access to all collections.
type Store struct {
	Configs      *store.Collection[SyncConfig]
	Sources      *store.Collection[SourceCalendar]
	SyncedEvents *store.Collection[SyncedEvent]
	SyncLogs     *store.Collection[SyncLog]
}

func NewStore(d *db.DB) (*Store, error) {
	configs, err := store.NewCollection[SyncConfig](d, "sync_configs")
	if err != nil {
		return nil, err
	}
	sources, err := store.NewCollection[SourceCalendar](d, "source_calendars")
	if err != nil {
		return nil, err
	}
	syncedEvents, err := store.NewCollection[SyncedEvent](d, "synced_events")
	if err != nil {
		return nil, err
	}
	syncLogs, err := store.NewCollection[SyncLog](d, "sync_logs")
	if err != nil {
		return nil, err
	}
	return &Store{
		Configs:      configs,
		Sources:      sources,
		SyncedEvents: syncedEvents,
		SyncLogs:     syncLogs,
	}, nil
}

// GetConfig returns the user's sync config, or nil if not configured.
func (s *Store) GetConfig(userID string) (*SyncConfig, error) {
	return s.Configs.Where("user_id", "==", userID).First()
}

// SaveConfigInput holds the fields that can be set via the API.
type SaveConfigInput struct {
	HubCalendarID       string
	HubCalendarName     string
	SyncWindowWeeks     int
	SyncIntervalMinutes int
}

// SaveConfig creates or updates the user's sync config.
func (s *Store) SaveConfig(userID string, input SaveConfigInput) (*SyncConfig, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	if input.SyncWindowWeeks <= 0 {
		input.SyncWindowWeeks = 8
	}
	if input.SyncIntervalMinutes <= 0 {
		input.SyncIntervalMinutes = 15
	}

	existing, err := s.GetConfig(userID)
	if err != nil {
		return nil, err
	}

	if existing != nil {
		existing.HubCalendarID = input.HubCalendarID
		existing.HubCalendarName = input.HubCalendarName
		existing.SyncWindowWeeks = input.SyncWindowWeeks
		existing.SyncIntervalMinutes = input.SyncIntervalMinutes
		existing.UpdatedAt = now
		if err := s.Configs.Update(existing.ID, existing); err != nil {
			return nil, err
		}
		return existing, nil
	}

	cfg := &SyncConfig{
		ID:                  uuid.New().String(),
		UserID:              userID,
		HubCalendarID:       input.HubCalendarID,
		HubCalendarName:     input.HubCalendarName,
		SyncWindowWeeks:     input.SyncWindowWeeks,
		SyncIntervalMinutes: input.SyncIntervalMinutes,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	if err := s.Configs.Create(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// GetSources returns all source calendars for a user.
func (s *Store) GetSources(userID string) ([]SourceCalendar, error) {
	return s.Sources.Where("user_id", "==", userID).All()
}

// ReconcileSources updates the source calendar list to match the desired state.
// Preserves sync_token for sources that remain. Returns the final list.
func (s *Store) ReconcileSources(userID string, desired []SourceCalendarInput) ([]SourceCalendar, error) {
	existing, err := s.GetSources(userID)
	if err != nil {
		return nil, err
	}

	// Index existing by calendar_id for lookup
	existingByCalID := make(map[string]SourceCalendar, len(existing))
	for _, src := range existing {
		existingByCalID[src.CalendarID] = src
	}

	// Index desired by calendar_id
	desiredByCalID := make(map[string]SourceCalendarInput, len(desired))
	for _, d := range desired {
		desiredByCalID[d.CalendarID] = d
	}

	// Delete sources that are no longer desired
	for _, src := range existing {
		if _, ok := desiredByCalID[src.CalendarID]; !ok {
			if err := s.Sources.Delete(src.ID); err != nil {
				return nil, err
			}
		}
	}

	// Add new sources, update emoji/color on existing ones
	now := time.Now().UTC().Format(time.RFC3339)
	for _, d := range desired {
		if existing, ok := existingByCalID[d.CalendarID]; ok {
			// Update emoji/color if changed
			if existing.EmojiPrefix != d.EmojiPrefix || existing.ColorID != d.ColorID {
				existing.EmojiPrefix = d.EmojiPrefix
				existing.ColorID = d.ColorID
				s.Sources.Update(existing.ID, &existing)
			}
		} else {
			src := &SourceCalendar{
				ID:           uuid.New().String(),
				UserID:       userID,
				CalendarID:   d.CalendarID,
				CalendarName: d.CalendarName,
				EmojiPrefix:  d.EmojiPrefix,
				ColorID:      d.ColorID,
				CreatedAt:    now,
			}
			if err := s.Sources.Create(src); err != nil {
				return nil, err
			}
		}
	}

	// Return the final list
	return s.GetSources(userID)
}

// SourceCalendarInput is the input for reconciling sources (no internal ID or sync_token).
type SourceCalendarInput struct {
	CalendarID   string `json:"calendarId"`
	CalendarName string `json:"calendarName"`
	EmojiPrefix  string `json:"emojiPrefix"`
	ColorID      string `json:"colorId"`
}

// UpdateRefreshToken stores the user's Google refresh token for background sync.
func (s *Store) UpdateRefreshToken(userID, refreshToken string) error {
	cfg, err := s.GetConfig(userID)
	if err != nil || cfg == nil {
		return err
	}
	cfg.RefreshToken = refreshToken
	cfg.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return s.Configs.Update(cfg.ID, cfg)
}

// UpdateLastSyncAt records when the user's last sync completed.
func (s *Store) UpdateLastSyncAt(userID string) error {
	cfg, err := s.GetConfig(userID)
	if err != nil || cfg == nil {
		return err
	}
	cfg.LastSyncAt = time.Now().UTC().Format(time.RFC3339)
	cfg.UpdatedAt = cfg.LastSyncAt
	return s.Configs.Update(cfg.ID, cfg)
}

// GetAllConfigs returns all sync configs (for the nudge endpoint).
func (s *Store) GetAllConfigs() ([]SyncConfig, error) {
	return s.Configs.Where("hub_calendar_id", "!=", "").All()
}

// UpdateSourceSyncToken persists the syncToken for a source calendar.
func (s *Store) UpdateSourceSyncToken(id, syncToken string) error {
	src, err := s.Sources.Where("id", "==", id).First()
	if err != nil || src == nil {
		return err
	}
	src.SyncToken = syncToken
	return s.Sources.Update(id, src)
}

// GetSyncedEvents returns all synced event mappings for a source→target pair.
func (s *Store) GetSyncedEvents(userID, sourceCalID, targetCalID string) ([]SyncedEvent, error) {
	all, err := s.SyncedEvents.Where("source_calendar_id", "==", sourceCalID).All()
	if err != nil {
		return nil, err
	}
	var filtered []SyncedEvent
	for _, se := range all {
		if se.UserID == userID && se.TargetCalendarID == targetCalID {
			filtered = append(filtered, se)
		}
	}
	return filtered, nil
}

// GetSyncedEventsForUser returns all synced events for a user.
func (s *Store) GetSyncedEventsForUser(userID string) ([]SyncedEvent, error) {
	return s.SyncedEvents.Where("user_id", "==", userID).All()
}

// GetSyncedEventsForTarget returns all synced events targeting a specific calendar.
func (s *Store) GetSyncedEventsForTarget(userID, targetCalID string) ([]SyncedEvent, error) {
	all, err := s.SyncedEvents.Where("user_id", "==", userID).All()
	if err != nil {
		return nil, err
	}
	var filtered []SyncedEvent
	for _, se := range all {
		if se.TargetCalendarID == targetCalID {
			filtered = append(filtered, se)
		}
	}
	return filtered, nil
}

// CreateSyncedEvent inserts a new synced event mapping.
func (s *Store) CreateSyncedEvent(se *SyncedEvent) error {
	se.ID = uuid.New().String()
	se.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	se.UpdatedAt = se.CreatedAt
	return s.SyncedEvents.Create(se)
}

// UpdateSyncedEvent updates an existing synced event mapping.
func (s *Store) UpdateSyncedEvent(se *SyncedEvent) error {
	se.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return s.SyncedEvents.Update(se.ID, se)
}

// DeleteSyncedEvent removes a synced event mapping.
func (s *Store) DeleteSyncedEvent(id string) error {
	return s.SyncedEvents.Delete(id)
}

// CreateSyncLog inserts a new sync log entry.
func (s *Store) CreateSyncLog(log *SyncLog) error {
	log.ID = uuid.New().String()
	return s.SyncLogs.Create(log)
}

// UpdateSyncLog updates an existing sync log entry.
func (s *Store) UpdateSyncLog(log *SyncLog) error {
	return s.SyncLogs.Update(log.ID, log)
}

// GetRecentSyncLogs returns the most recent sync logs for a user.
func (s *Store) GetRecentSyncLogs(userID string, limit int) ([]SyncLog, error) {
	all, err := s.SyncLogs.Where("user_id", "==", userID).OrderBy("started_at", store.Desc).All()
	if err != nil {
		return nil, err
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// GetRunningSyncLog returns a sync log with status "running" if one exists.
func (s *Store) GetRunningSyncLog(userID string) (*SyncLog, error) {
	logs, err := s.SyncLogs.Where("user_id", "==", userID).All()
	if err != nil {
		return nil, err
	}
	staleThreshold := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	for i := range logs {
		if logs[i].Status == "running" {
			// Mark stale running logs as failed
			if logs[i].StartedAt < staleThreshold {
				logs[i].Status = "failed"
				logs[i].ErrorMsg = "timed out"
				logs[i].CompletedAt = time.Now().UTC().Format(time.RFC3339)
				s.SyncLogs.Update(logs[i].ID, &logs[i])
				continue
			}
			return &logs[i], nil
		}
	}
	return nil, nil
}
