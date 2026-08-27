package sync

import (
	"log"
	"sort"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/michaelwinser/appbase/db"
	"github.com/michaelwinser/appbase/store"
)

// syncLogRetention caps how many sync logs we keep per user. Growth is trimmed
// opportunistically on the (already full-reading) GetRecentSyncLogs path.
const syncLogRetention = 200

// syncLogTrimBatch bounds deletes per GetRecentSyncLogs call so an existing large
// backlog drains over several page loads instead of adding many synchronous
// deletes to one request. Kept small to keep that page load responsive.
const syncLogTrimBatch = 50

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
// Since M8, ID is the deterministic SyncedEventKey (a hash of the userID/sourceCal/
// sourceEvent/targetCal 4-tuple), so a mapping is point-fetched with GetSyncedEventByKey
// (one Get) instead of a scan. The legacy per-source Where("source_calendar_id",...).All()
// scan (GetSyncedEvents) remains for the full-pass reconciliation paths.
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
	ID           string `json:"id"          store:"id,pk"`
	UserID       string `json:"userId"      store:"user_id,index"`
	StartedAt    string `json:"startedAt"   store:"started_at"`
	CompletedAt  string `json:"completedAt" store:"completed_at"`
	Created      int    `json:"created"     store:"created"`
	Updated      int    `json:"updated"     store:"updated"`
	Deleted      int    `json:"deleted"     store:"deleted"`
	Errors       int    `json:"errors"      store:"errors"`
	Status       string `json:"status"      store:"status"`
	ErrorMsg     string `json:"errorMsg"    store:"error_msg"`
	ErrorDetails string `json:"errorDetails" store:"error_details"` // JSON: []string of error messages
	Details      string `json:"details"     store:"details"`        // JSON: map[calendarName]{created,updated,deleted}
}

// Store provides access to all collections.
type Store struct {
	Configs      *store.Collection[SyncConfig]
	Sources      *store.Collection[SourceCalendar]
	SyncedEvents *store.Collection[SyncedEvent]
	SyncLogs     *store.Collection[SyncLog]

	// reads is a cumulative count of documents read from the backend across all
	// query methods, for cost instrumentation. appbase reads (and Firestore bills)
	// every document matching a query's first Where, so this proxies billed reads.
	reads atomic.Int64
}

// addReads records docs read by a query (call with the raw pre-filter count).
func (s *Store) addReads(n int) { s.reads.Add(int64(n)) }

// Reads returns the cumulative document-read count since process start.
func (s *Store) Reads() int64 { return s.reads.Load() }

func NewStore(d *db.DB) (*Store, error) {
	configs, err := store.NewCollection[SyncConfig](d, "sync_configs")
	if err != nil {
		return nil, err
	}
	// M8 read-switch: the sync module owns module-namespaced collections, and
	// synced_events is re-keyed by SyncedEventKey. Deploy this only AFTER `migrate copy`
	// has populated these collections (see docs/M8-migration-runbook.md).
	sources, err := store.NewCollection[SourceCalendar](d, newSourceCalendars)
	if err != nil {
		return nil, err
	}
	syncedEvents, err := store.NewCollection[SyncedEvent](d, newSyncedEvents)
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
	all, err := s.Sources.Where("user_id", "==", userID).All()
	if err == nil {
		s.addReads(len(all))
	}
	return all, err
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
	all, err := s.Configs.Where("hub_calendar_id", "!=", "").All()
	if err == nil {
		s.addReads(len(all))
	}
	return all, err
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
	s.addReads(len(all))
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
	all, err := s.SyncedEvents.Where("user_id", "==", userID).All()
	if err == nil {
		s.addReads(len(all))
	}
	return all, err
}

// CreateSyncedEvent inserts a synced event mapping, keyed by its deterministic
// point-lookup key (SyncedEventKey) rather than a random uuid — this is what lets the
// mapping be point-fetched with GetSyncedEventByKey instead of a full-collection scan.
// Plain Create (not the migration's upsert): on Firestore it's Doc.Set, so re-creating
// the same 4-tuple is idempotent for free; the real sync flow only creates when no
// mapping was loaded, so it never double-fires the SQLite INSERT. Avoids a wasted read
// on the hot path — the whole point of this milestone.
func (s *Store) CreateSyncedEvent(se *SyncedEvent) error {
	se.ID = SyncedEventKey(se.UserID, se.SourceCalendarID, se.SourceEventID, se.TargetCalendarID)
	now := time.Now().UTC().Format(time.RFC3339)
	se.CreatedAt = now
	se.UpdatedAt = now
	return s.SyncedEvents.Create(se)
}

// GetSyncedEventByKey fetches a mapping with a single point Get on its deterministic
// key, or (nil, nil) if none exists. This is the O(1) lookup two-tier sync uses in
// place of scanning the whole collection.
func (s *Store) GetSyncedEventByKey(userID, sourceCalID, sourceEventID, targetCalID string) (*SyncedEvent, error) {
	s.addReads(1)
	return s.SyncedEvents.Get(SyncedEventKey(userID, sourceCalID, sourceEventID, targetCalID))
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

// GetRecentSyncLogs returns a user's most recent sync logs, newest first, up to
// limit. appbase@v0.2.3 pushes only the first Where to Firestore and can't push a
// limit there, so this reads all of the user's logs — viewing the history is thus
// the one read whose cost tracks log count. To keep that bounded it trims the
// collection back toward syncLogRetention here (delete-only, capped at
// syncLogTrimBatch per call so a large backlog drains over several views rather
// than blocking one request). A never-viewed history just accumulates as cheap
// storage; the status-scoped GetRunningSyncLog means log count no longer affects
// per-sync read cost.
func (s *Store) GetRecentSyncLogs(userID string, limit int) ([]SyncLog, error) {
	all, err := s.SyncLogs.Where("user_id", "==", userID).All()
	if err != nil {
		return nil, err
	}
	s.addReads(len(all))

	// Sort newest-first ourselves rather than trusting the backend's ordering: this
	// drives irreversible deletes, so it must not depend on appbase's OrderBy.
	sort.Slice(all, func(i, j int) bool { return all[i].StartedAt > all[j].StartedAt })

	if excess := len(all) - syncLogRetention; excess > 0 {
		if excess > syncLogTrimBatch {
			excess = syncLogTrimBatch
		}
		for _, old := range all[len(all)-excess:] { // oldest are the tail
			if err := s.SyncLogs.Delete(old.ID); err != nil {
				log.Printf("sync log retention: failed to delete %s: %v", old.ID, err)
			}
		}
		all = all[:len(all)-excess]
	}

	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// GetRunningSyncLog returns the requesting user's running sync log if one exists.
// It queries by status (the pushed-down predicate) so the backend returns only
// running logs (typically 0-1), not the user's entire history; the user match is
// applied in memory since appbase pushes just the first Where (see GetRecentSyncLogs).
// It also reaps stale running rows so the running set can't grow monotonically: the
// caller's own after 5 minutes (a crashed mid-sync), and anyone's after an hour
// (an abandoned account's — no real sync runs that long), while never failing another
// user's merely-long in-flight sync.
func (s *Store) GetRunningSyncLog(userID string) (*SyncLog, error) {
	logs, err := s.SyncLogs.Where("status", "==", "running").All()
	if err != nil {
		return nil, err
	}
	s.addReads(len(logs))
	now := time.Now().UTC()
	ownStale := now.Add(-5 * time.Minute).Format(time.RFC3339)
	abandoned := now.Add(-1 * time.Hour).Format(time.RFC3339)
	for i := range logs {
		veryOld := logs[i].StartedAt < abandoned
		if logs[i].UserID != userID && !veryOld {
			continue // another user's in-flight sync — leave it alone
		}
		if veryOld || logs[i].StartedAt < ownStale {
			logs[i].Status = "failed"
			logs[i].ErrorMsg = "timed out"
			logs[i].CompletedAt = now.Format(time.RFC3339)
			s.SyncLogs.Update(logs[i].ID, &logs[i])
			continue
		}
		return &logs[i], nil // our own active running sync
	}
	return nil, nil
}
