package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// hubEvent represents a placeholder on the hub with its source metadata.
type hubEvent struct {
	event         GCalEvent
	sourceCalID   string
	sourceEventID string
}

// SyncResult holds the counts from a sync pass.
type SyncResult struct {
	Created int    `json:"created"`
	Updated int    `json:"updated"`
	Deleted int    `json:"deleted"`
	Errors  int    `json:"errors"`
	Message string `json:"message"`
}

// RunSync executes a full sync pass for the given user.
// It syncs events from each source calendar to the hub calendar.
func RunSync(ctx context.Context, token string, store *Store, config *SyncConfig, sources []SourceCalendar) (*SyncResult, error) {
	// Concurrent sync guard
	running, err := store.GetRunningSyncLog(config.UserID)
	if err != nil {
		return nil, fmt.Errorf("checking running sync: %w", err)
	}
	if running != nil {
		return nil, fmt.Errorf("a sync is already running (started %s)", running.StartedAt)
	}

	// Create sync log
	syncLog := &SyncLog{
		UserID:    config.UserID,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Status:    "running",
	}
	if err := store.CreateSyncLog(syncLog); err != nil {
		return nil, fmt.Errorf("creating sync log: %w", err)
	}

	result := &SyncResult{}

	// Phase 1: Inbound — sync each source calendar to the hub
	for _, source := range sources {
		err := syncSourceToHub(ctx, token, store, config, &source, result)
		if err != nil {
			log.Printf("inbound sync error for %s: %v", source.CalendarName, err)
			result.Errors++
		}
	}

	// Phase 2: Outbound — sync hub placeholders to each source calendar
	// Note: newly created/deleted hub events from Phase 1 may not be visible
	// to the API yet due to eventual consistency. They will propagate on the
	// next sync pass. This is an accepted trade-off.
	if err := syncHubToSources(ctx, token, store, config, sources, result); err != nil {
		log.Printf("outbound sync error: %v", err)
		result.Errors++
	}

	// Phase 3: Cleanup — delete placeholders for removed source calendars
	cleanupRemovedSources(ctx, token, store, config, sources, result)

	// Phase 4: Cleanup — delete placeholders for past events (end date before today)
	cleanupPastEvents(ctx, token, store, config, sources, result)

	// Complete sync log
	syncLog.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	syncLog.Created = result.Created
	syncLog.Updated = result.Updated
	syncLog.Deleted = result.Deleted
	syncLog.Errors = result.Errors
	syncLog.Status = "completed"
	if result.Errors > 0 {
		syncLog.Status = "completed_with_errors"
	}
	store.UpdateSyncLog(syncLog)

	// Record last sync time for nudge scheduling
	store.UpdateLastSyncAt(config.UserID)

	result.Message = fmt.Sprintf("Sync completed: %d created, %d updated, %d deleted",
		result.Created, result.Updated, result.Deleted)
	if result.Errors > 0 {
		result.Message += fmt.Sprintf(", %d errors", result.Errors)
	}

	return result, nil
}

func syncSourceToHub(ctx context.Context, token string, store *Store, config *SyncConfig, source *SourceCalendar, result *SyncResult) error {
	hubCalID := config.HubCalendarID
	syncWindow := config.SyncWindowWeeks
	if syncWindow <= 0 {
		syncWindow = 8
	}

	now := time.Now().UTC()
	timeMin := now
	timeMax := now.Add(time.Duration(syncWindow) * 7 * 24 * time.Hour)

	// 1. Fetch source events (incremental if syncToken available, else full)
	var sourceEvents []GCalEvent
	var newSyncToken string

	if source.SyncToken != "" {
		res, err := ListEventsIncremental(ctx, token, source.CalendarID, source.SyncToken)
		if errors.Is(err, ErrSyncTokenExpired) {
			log.Printf("sync token expired for %s, falling back to full sync", source.CalendarName)
			source.SyncToken = ""
			// Fall through to full sync below
		} else if err != nil {
			return fmt.Errorf("incremental fetch from %s: %w", source.CalendarName, err)
		} else {
			sourceEvents = res.Events
			newSyncToken = res.SyncToken
		}
	}

	if source.SyncToken == "" {
		// Full sync
		res, err := ListEvents(ctx, token, source.CalendarID, timeMin, timeMax)
		if err != nil {
			return fmt.Errorf("fetching events from %s: %w", source.CalendarName, err)
		}
		sourceEvents = res.Events
		newSyncToken = res.SyncToken
	}

	// 2. Fetch existing placeholders on hub for this source
	placeholders, err := ListPlaceholders(ctx, token, hubCalID, source.CalendarID)
	if err != nil {
		return fmt.Errorf("fetching placeholders for %s: %w", source.CalendarName, err)
	}

	// 3. Load SyncedEvent records for this source → hub
	syncedEvents, err := store.GetSyncedEvents(config.UserID, source.CalendarID, hubCalID)
	if err != nil {
		return fmt.Errorf("loading synced events: %w", err)
	}

	// 4. Build lookup maps
	sourceByID := make(map[string]GCalEvent, len(sourceEvents))
	for _, e := range sourceEvents {
		sourceByID[e.ID] = e
	}

	placeholderBySourceID := make(map[string]GCalEvent, len(placeholders))
	for _, p := range placeholders {
		srcID := SourceEventID(p)
		if srcID != "" {
			placeholderBySourceID[srcID] = p
		}
	}

	syncedBySourceID := make(map[string]SyncedEvent, len(syncedEvents))
	for _, se := range syncedEvents {
		syncedBySourceID[se.SourceEventID] = se
	}

	// 5. Process source events: create or update placeholders
	for _, event := range sourceEvents {
		if event.Status == "cancelled" {
			continue
		}
		if IsDeclined(event) {
			continue
		}
		if IsPlaceholder(event) {
			continue
		}
		if shouldSkipEventType(event.EventType) {
			continue
		}

		placeholder := BuildPlaceholder(event, source.CalendarID)
		existingSynced, hasSynced := syncedBySourceID[event.ID]

		if !hasSynced {
			// Check if a placeholder already exists on the hub (e.g. after DB wipe).
			// Adopt it instead of creating a duplicate.
			if existingPlaceholder, found := placeholderBySourceID[event.ID]; found {
				err := store.CreateSyncedEvent(&SyncedEvent{
					UserID:           config.UserID,
					SourceCalendarID: source.CalendarID,
					SourceEventID:    event.ID,
					TargetCalendarID: hubCalID,
					TargetEventID:    existingPlaceholder.ID,
					SourceUpdated:    event.Updated,
				})
				if err != nil {
					log.Printf("failed to adopt existing placeholder: %v", err)
					result.Errors++
				}
				// Remove from map so it isn't orphan-deleted
				delete(syncedBySourceID, event.ID)
				continue
			}

			// CREATE: no existing placeholder anywhere
			created, err := CreateEvent(ctx, token, hubCalID, &placeholder)
			if err != nil {
				log.Printf("failed to create placeholder for %s: %v", event.Summary, err)
				result.Errors++
				continue
			}
			err = store.CreateSyncedEvent(&SyncedEvent{
				UserID:           config.UserID,
				SourceCalendarID: source.CalendarID,
				SourceEventID:    event.ID,
				TargetCalendarID: hubCalID,
				TargetEventID:    created.ID,
				SourceUpdated:    event.Updated,
			})
			if err != nil {
				log.Printf("failed to store synced event: %v", err)
				result.Errors++
				continue
			}
			result.Created++
		} else if event.Updated > existingSynced.SourceUpdated {
			// UPDATE: source changed since last sync
			_, err := UpdateEvent(ctx, token, hubCalID, existingSynced.TargetEventID, &placeholder)
			if err != nil {
				if isNotFoundError(err) {
					// Placeholder was deleted by user — remove stale record so
					// the next sync pass recreates it (option 2: sync owns placeholders)
					log.Printf("placeholder for %s was deleted, will recreate next pass", event.Summary)
					store.DeleteSyncedEvent(existingSynced.ID)
				} else {
					log.Printf("failed to update placeholder for %s: %v", event.Summary, err)
					result.Errors++
				}
				continue
			}
			existingSynced.SourceUpdated = event.Updated
			if err := store.UpdateSyncedEvent(&existingSynced); err != nil {
				log.Printf("failed to update synced event: %v", err)
			}
			result.Updated++
		}
		// else: unchanged, skip

		// Remove from map so step 6 knows this source event still exists
		delete(syncedBySourceID, event.ID)
	}

	// 6. Delete orphaned placeholders (source event no longer exists)
	for _, se := range syncedBySourceID {
		err := DeleteEvent(ctx, token, hubCalID, se.TargetEventID)
		if err != nil {
			log.Printf("failed to delete orphaned placeholder %s: %v", se.TargetEventID, err)
			result.Errors++
			continue
		}
		if err := store.DeleteSyncedEvent(se.ID); err != nil {
			log.Printf("failed to delete synced event record: %v", err)
		}
		result.Deleted++
	}

	// 7. Persist sync token
	if newSyncToken != "" {
		if err := store.UpdateSourceSyncToken(source.ID, newSyncToken); err != nil {
			log.Printf("failed to persist sync token: %v", err)
		}
	}

	return nil
}

// syncHubToSources syncs hub placeholders outbound to each source calendar.
// For each source calendar S, it creates/updates/deletes placeholders for
// hub events that did NOT originate from S (no self-sync).
func syncHubToSources(ctx context.Context, token string, store *Store, config *SyncConfig, sources []SourceCalendar, result *SyncResult) error {
	hubCalID := config.HubCalendarID

	// Fetch ALL hub placeholders as ground truth for what should exist outbound.
	hubPlaceholders, err := ListAllPlaceholders(ctx, token, hubCalID)
	if err != nil {
		return fmt.Errorf("fetching hub placeholders: %w", err)
	}

	// Index hub placeholders by sourceEventID for lookup
	var hubEvents []hubEvent
	for _, p := range hubPlaceholders {
		if p.ExtendedProperties == nil {
			continue
		}
		srcCalID := p.ExtendedProperties.Private["sourceCalendarId"]
		srcEventID := p.ExtendedProperties.Private["sourceEventId"]
		if srcCalID == "" || srcEventID == "" {
			continue
		}
		hubEvents = append(hubEvents, hubEvent{
			event:         p,
			sourceCalID:   srcCalID,
			sourceEventID: srcEventID,
		})
	}

	// For each source calendar, propagate hub events that didn't originate from it
	for _, source := range sources {
		if err := syncOutboundToSource(ctx, token, store, config, &source, hubEvents, result); err != nil {
			log.Printf("outbound sync error for %s: %v", source.CalendarName, err)
			result.Errors++
		}
	}

	return nil
}

func syncOutboundToSource(ctx context.Context, token string, store *Store, config *SyncConfig, source *SourceCalendar, hubEvents []hubEvent, result *SyncResult) error {
	targetCalID := source.CalendarID

	// Load existing outbound SyncedEvent records for this target calendar
	existingSynced, err := store.GetSyncedEventsForTarget(config.UserID, targetCalID)
	if err != nil {
		return fmt.Errorf("loading outbound synced events: %w", err)
	}

	// Fetch existing placeholders on this target calendar for adoption after DB wipe
	existingPlaceholders, err := ListAllPlaceholders(ctx, token, targetCalID)
	if err != nil {
		// If we can't list (e.g. read-only), skip this calendar
		if isPermissionError(err) {
			log.Printf("skipping read-only calendar %s", source.CalendarName)
			return nil
		}
		return fmt.Errorf("listing outbound placeholders on %s: %w", source.CalendarName, err)
	}

	// Index existing placeholders by sourceCalID + "|" + sourceEventID
	placeholderByKey := make(map[string]GCalEvent, len(existingPlaceholders))
	for _, p := range existingPlaceholders {
		srcCalID := ""
		srcEventID := ""
		if p.ExtendedProperties != nil {
			srcCalID = p.ExtendedProperties.Private["sourceCalendarId"]
			srcEventID = p.ExtendedProperties.Private["sourceEventId"]
		}
		if srcCalID != "" && srcEventID != "" {
			placeholderByKey[srcCalID+"|"+srcEventID] = p
		}
	}

	// Index by a composite key: sourceCalID + "|" + sourceEventID
	syncedByKey := make(map[string]SyncedEvent, len(existingSynced))
	for _, se := range existingSynced {
		key := se.SourceCalendarID + "|" + se.SourceEventID
		syncedByKey[key] = se
	}

	// Process hub events: create or update outbound placeholders
	for _, he := range hubEvents {
		// No self-sync: skip events that originated from this source calendar
		if he.sourceCalID == targetCalID {
			continue
		}
		if he.event.Status == "cancelled" {
			continue
		}

		key := he.sourceCalID + "|" + he.sourceEventID
		placeholder := BuildPlaceholder(he.event, he.sourceCalID)

		existingSe, hasSynced := syncedByKey[key]

		if !hasSynced {
			// Check if a placeholder already exists (e.g. after DB wipe). Adopt it.
			if existingP, found := placeholderByKey[key]; found {
				err := store.CreateSyncedEvent(&SyncedEvent{
					UserID:           config.UserID,
					SourceCalendarID: he.sourceCalID,
					SourceEventID:    he.sourceEventID,
					TargetCalendarID: targetCalID,
					TargetEventID:    existingP.ID,
					SourceUpdated:    he.event.Updated,
				})
				if err != nil {
					log.Printf("failed to adopt outbound placeholder: %v", err)
					result.Errors++
				}
				delete(syncedByKey, key)
				continue
			}

			// CREATE outbound placeholder
			created, err := CreateEvent(ctx, token, targetCalID, &placeholder)
			if err != nil {
				// Silently skip read-only calendars (UC-0047)
				if isPermissionError(err) {
					log.Printf("skipping read-only calendar %s", source.CalendarName)
					return nil // skip entire calendar
				}
				log.Printf("failed to create outbound placeholder on %s: %v", source.CalendarName, err)
				result.Errors++
				continue
			}
			err = store.CreateSyncedEvent(&SyncedEvent{
				UserID:           config.UserID,
				SourceCalendarID: he.sourceCalID,
				SourceEventID:    he.sourceEventID,
				TargetCalendarID: targetCalID,
				TargetEventID:    created.ID,
				SourceUpdated:    he.event.Updated,
			})
			if err != nil {
				log.Printf("failed to store outbound synced event: %v", err)
				result.Errors++
				continue
			}
			result.Created++
		} else if he.event.Updated > existingSe.SourceUpdated {
			// UPDATE outbound placeholder
			_, err := UpdateEvent(ctx, token, targetCalID, existingSe.TargetEventID, &placeholder)
			if err != nil {
				if isPermissionError(err) {
					return nil
				}
				if isNotFoundError(err) {
					log.Printf("outbound placeholder on %s was deleted, will recreate next pass", source.CalendarName)
					store.DeleteSyncedEvent(existingSe.ID)
					continue
				}
				log.Printf("failed to update outbound placeholder on %s: %v", source.CalendarName, err)
				result.Errors++
				continue
			}
			existingSe.SourceUpdated = he.event.Updated
			if err := store.UpdateSyncedEvent(&existingSe); err != nil {
				log.Printf("failed to update outbound synced event: %v", err)
			}
			result.Updated++
		}

		// Remove from map so orphan detection works
		delete(syncedByKey, key)
	}

	// Delete orphaned outbound placeholders
	for _, se := range syncedByKey {
		// Only delete outbound records (not inbound hub records)
		if se.TargetCalendarID != targetCalID {
			continue
		}
		err := DeleteEvent(ctx, token, targetCalID, se.TargetEventID)
		if err != nil {
			if isPermissionError(err) {
				return nil
			}
			log.Printf("failed to delete orphaned outbound placeholder: %v", err)
			result.Errors++
			continue
		}
		if err := store.DeleteSyncedEvent(se.ID); err != nil {
			log.Printf("failed to delete outbound synced event record: %v", err)
		}
		result.Deleted++
	}

	return nil
}

// shouldSkipEventType returns true for event types that should not be synced.
// workingLocation is an account-specific feature that cannot be created as a
// regular event on all calendar types. outOfOffice and focusTime are synced
// because they intentionally block time to prevent meetings.
func shouldSkipEventType(eventType string) bool {
	return eventType == "workingLocation"
}

// cleanupRemovedSources deletes all placeholder events that originated from
// source calendars no longer in the active config. This handles the case where
// a user unchecks a source calendar — all its placeholders (on the hub and on
// other source calendars) should be removed.
func cleanupRemovedSources(ctx context.Context, token string, store *Store, config *SyncConfig, activeSources []SourceCalendar, result *SyncResult) {
	// Build set of active source calendar IDs
	activeIDs := make(map[string]bool, len(activeSources))
	for _, s := range activeSources {
		activeIDs[s.CalendarID] = true
	}

	// Find all SyncedEvent records for this user
	allSynced, err := store.GetSyncedEventsForUser(config.UserID)
	if err != nil {
		log.Printf("cleanup: failed to load synced events: %v", err)
		return
	}

	// Delete placeholders whose source calendar is no longer active
	for _, se := range allSynced {
		if activeIDs[se.SourceCalendarID] {
			continue // source is still active
		}

		// Delete the placeholder event from the target calendar
		err := DeleteEvent(ctx, token, se.TargetCalendarID, se.TargetEventID)
		if err != nil && !isNotFoundError(err) {
			log.Printf("cleanup: failed to delete placeholder %s on %s: %v",
				se.TargetEventID, se.TargetCalendarID, err)
			result.Errors++
			continue
		}

		// Remove the tracking record
		if err := store.DeleteSyncedEvent(se.ID); err != nil {
			log.Printf("cleanup: failed to delete synced event record: %v", err)
		}
		result.Deleted++
	}
}

// cleanupPastEvents deletes placeholder events whose end date is before today.
// Placeholders only exist to prevent meeting conflicts — past events don't need them.
func cleanupPastEvents(ctx context.Context, token string, store *Store, config *SyncConfig, sources []SourceCalendar, result *SyncResult) {
	today := time.Now().UTC().Truncate(24 * time.Hour).Format("2006-01-02")

	allSynced, err := store.GetSyncedEventsForUser(config.UserID)
	if err != nil {
		log.Printf("past cleanup: failed to load synced events: %v", err)
		return
	}
	if len(allSynced) == 0 {
		return
	}

	// Collect all target calendar IDs to query for placeholders
	targetCalIDs := make(map[string]bool)
	targetCalIDs[config.HubCalendarID] = true
	for _, s := range sources {
		targetCalIDs[s.CalendarID] = true
	}

	// For each target calendar, find our placeholders and check end dates
	for calID := range targetCalIDs {
		placeholders, err := ListAllPlaceholders(ctx, token, calID)
		if err != nil {
			log.Printf("past cleanup: failed to list placeholders on %s: %v", calID, err)
			continue
		}

		for _, p := range placeholders {
			endDate := p.End.Date
			if endDate == "" && p.End.DateTime != "" {
				// Extract date from dateTime
				t, err := time.Parse(time.RFC3339, p.End.DateTime)
				if err != nil {
					continue
				}
				endDate = t.UTC().Format("2006-01-02")
			}
			if endDate == "" || endDate >= today {
				continue
			}

			// Past event — delete the placeholder
			err := DeleteEvent(ctx, token, calID, p.ID)
			if err != nil && !isNotFoundError(err) {
				log.Printf("past cleanup: failed to delete %s: %v", p.ID, err)
				result.Errors++
				continue
			}

			// Remove the SyncedEvent record if one exists
			srcEventID := SourceEventID(p)
			for _, se := range allSynced {
				if se.TargetEventID == p.ID || (se.TargetCalendarID == calID && se.SourceEventID == srcEventID) {
					store.DeleteSyncedEvent(se.ID)
					break
				}
			}
			result.Deleted++
		}
	}
}

// isNotFoundError checks if an error is a Google API 404 (event deleted).
func isNotFoundError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "status 404")
}

// isPermissionError checks if an error is a Google API 403 (no write access).
func isPermissionError(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "status 403") || strings.Contains(err.Error(), "forbidden"))
}
