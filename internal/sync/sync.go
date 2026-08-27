package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/michaelwinser/calendar-sync/internal/platform/calendar"
)

// hubEvent represents a placeholder on the hub with its source metadata.
type hubEvent struct {
	event         GCalEvent
	sourceCalID   string
	sourceEventID string
}

// CalendarCounts tracks sync operations for a single calendar.
type CalendarCounts struct {
	Created int `json:"created"`
	Updated int `json:"updated"`
	Deleted int `json:"deleted"`
}

// SyncResult holds the counts from a sync pass.
type SyncResult struct {
	Created      int                        `json:"created"`
	Updated      int                        `json:"updated"`
	Deleted      int                        `json:"deleted"`
	Errors       int                        `json:"errors"`
	ErrorDetails []string                   `json:"errorDetails,omitempty"`
	Message      string                     `json:"message"`
	PerCalendar  map[string]*CalendarCounts `json:"perCalendar,omitempty"`
}

// addError logs an error, increments the count, and records the message.
func (r *SyncResult) addError(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Print(msg)
	r.Errors++
	r.ErrorDetails = append(r.ErrorDetails, msg)
}

func (r *SyncResult) calCounts(name string) *CalendarCounts {
	if r.PerCalendar == nil {
		r.PerCalendar = make(map[string]*CalendarCounts)
	}
	if r.PerCalendar[name] == nil {
		r.PerCalendar[name] = &CalendarCounts{}
	}
	return r.PerCalendar[name]
}

// SyncOptions controls sync behavior.
type SyncOptions struct {
	SyncDays int  // 0 = use config default
	DryRun   bool // if true, report what would change without writing
}

// RunSync executes a full sync pass using the configured sync window.
func RunSync(ctx context.Context, cal *calendar.Client, token string, store *Store, config *SyncConfig, sources []SourceCalendar) (*SyncResult, error) {
	return RunSyncWithOptions(ctx, cal, token, store, config, sources, SyncOptions{})
}

// RunSyncWithDays executes a full sync pass with an explicit window in days.
func RunSyncWithDays(ctx context.Context, cal *calendar.Client, token string, store *Store, config *SyncConfig, sources []SourceCalendar, syncDays int) (*SyncResult, error) {
	return RunSyncWithOptions(ctx, cal, token, store, config, sources, SyncOptions{SyncDays: syncDays})
}

// RunSyncWithOptions executes a full sync pass with explicit options.
func RunSyncWithOptions(ctx context.Context, cal *calendar.Client, token string, store *Store, config *SyncConfig, sources []SourceCalendar, opts SyncOptions) (*SyncResult, error) {
	readsBefore := store.Reads()
	syncDays := opts.SyncDays
	if syncDays <= 0 {
		syncDays = config.SyncWindowWeeks * 7
	}
	if syncDays <= 0 {
		syncDays = 56 // default 8 weeks
	}
	// Concurrent sync guard
	running, err := store.GetRunningSyncLog(config.UserID)
	if err != nil {
		return nil, fmt.Errorf("checking running sync: %w", err)
	}
	if running != nil {
		return nil, fmt.Errorf("a sync is already running (started %s)", running.StartedAt)
	}

	dryRun := opts.DryRun

	// Create sync log (skip in dry-run)
	syncLog := &SyncLog{
		UserID:    config.UserID,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Status:    "running",
	}
	if !dryRun {
		if err := store.CreateSyncLog(syncLog); err != nil {
			return nil, fmt.Errorf("creating sync log: %w", err)
		}
	}

	result := &SyncResult{}

	// snapshot takes a copy of current counts for delta calculation
	snapshot := func() (int, int, int) { return result.Created, result.Updated, result.Deleted }
	recordDelta := func(name string, c0, u0, d0 int) {
		cc := result.calCounts(name)
		cc.Created += result.Created - c0
		cc.Updated += result.Updated - u0
		cc.Deleted += result.Deleted - d0
	}

	// Phase 1: Inbound — sync each source calendar to the hub
	for _, source := range sources {
		c0, u0, d0 := snapshot()
		err := syncSourceToHub(ctx, cal, token, store, config, &source, syncDays, dryRun, result)
		recordDelta(source.CalendarName, c0, u0, d0)
		if err != nil {
			result.addError("inbound sync error for %s: %v", source.CalendarName, err)
		}
	}

	// Phase 2: Outbound — sync hub placeholders to each source calendar
	// Note: newly created/deleted hub events from Phase 1 may not be visible
	// to the API yet due to eventual consistency. They will propagate on the
	// next sync pass. This is an accepted trade-off.
	if err := syncHubToSources(ctx, cal, token, store, config, sources, dryRun, result); err != nil {
		result.addError("outbound sync error: %v", err)
	}

	// Phases 3 & 4: Cleanup. Both scan the user's full synced-event set, so load
	// it once and share it instead of scanning twice.
	if !dryRun {
		allSynced, err := store.GetSyncedEventsForUser(config.UserID)
		if err != nil {
			// Surface as a sync error (counts toward result.Errors and shows in the
			// log as completed_with_errors) rather than only emitting a log line.
			result.addError("cleanup: failed to load synced events: %v", err)
		} else {
			// Phase 3: delete placeholders for removed source calendars
			cleanupRemovedSources(ctx, cal, token, store, sources, allSynced, result)
			// Phase 4: delete placeholders for past events. allSynced is the
			// pre-Phase-3 snapshot, so it may still list records Phase 3 just
			// deleted — harmless: the placeholder scan comes fresh from the API,
			// and DeleteSyncedEvent on an already-removed ID is a no-op.
			cleanupPastEvents(ctx, cal, token, store, config, sources, allSynced, result)
		}
	}

	// Complete sync log — every pass persists a durable row so the UI's log table
	// stays a live heartbeat. Growth is bounded on view; see GetRecentSyncLogs.
	if !dryRun {
		syncLog.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		syncLog.Created = result.Created
		syncLog.Updated = result.Updated
		syncLog.Deleted = result.Deleted
		syncLog.Errors = result.Errors
		syncLog.Status = "completed"
		if result.Errors > 0 {
			syncLog.Status = "completed_with_errors"
			if errJSON, err := json.Marshal(result.ErrorDetails); err == nil {
				syncLog.ErrorDetails = string(errJSON)
			}
		}
		if result.PerCalendar != nil {
			if detailsJSON, err := json.Marshal(result.PerCalendar); err == nil {
				syncLog.Details = string(detailsJSON)
			}
		}
		store.UpdateSyncLog(syncLog)
		store.UpdateLastSyncAt(config.UserID)
	}

	prefix := "Sync completed"
	if dryRun {
		prefix = "Dry run"
	}
	result.Message = fmt.Sprintf("%s: %d created, %d updated, %d deleted",
		prefix, result.Created, result.Updated, result.Deleted)
	if result.Errors > 0 {
		result.Message += fmt.Sprintf(", %d errors", result.Errors)
	}

	// Cost instrumentation: approximate Firestore documents read during this pass,
	// from the store's process-wide counter. It covers the collection scans (the
	// material cost) but not point lookups, and is only approximate if any other
	// pass runs concurrently (the counter is process-wide, the guard only per-user).
	// Enough to confirm the read-reduction work and flag regressions like unbounded growth.
	log.Printf("sync user=%s firestore_reads~=%d created=%d updated=%d deleted=%d errors=%d",
		config.UserID, store.Reads()-readsBefore, result.Created, result.Updated, result.Deleted, result.Errors)

	return result, nil
}

func syncSourceToHub(ctx context.Context, cal *calendar.Client, token string, store *Store, config *SyncConfig, source *SourceCalendar, syncDays int, dryRun bool, result *SyncResult) error {
	hubCalID := config.HubCalendarID

	now := time.Now().UTC()
	timeMin := now
	timeMax := now.Add(time.Duration(syncDays) * 24 * time.Hour)

	// 1. Fetch source events (incremental if syncToken available, else full)
	var sourceEvents []GCalEvent
	var newSyncToken string

	if source.SyncToken != "" {
		res, err := cal.ListEventsIncremental(ctx, token, source.CalendarID, source.SyncToken)
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
		res, err := cal.ListEvents(ctx, token, source.CalendarID, timeMin, timeMax)
		if err != nil {
			return fmt.Errorf("fetching events from %s: %w", source.CalendarName, err)
		}
		sourceEvents = res.Events
		newSyncToken = res.SyncToken
	}

	// 2. Fetch existing placeholders on hub for this source
	placeholders, err := ListPlaceholders(ctx, cal, token, hubCalID, source.CalendarID)
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

		// Inbound to hub: no per-calendar visual options. Stamp the source event's
		// Updated so outbound and the two-tier fast pass compare the same value.
		placeholder := BuildPlaceholder(event, source.CalendarID, event.ID, event.Updated, PlaceholderOptions{})
		existingSynced, hasSynced := syncedBySourceID[event.ID]

		if !hasSynced {
			// Check if a placeholder already exists on the hub (e.g. after DB wipe).
			if _, found := placeholderBySourceID[event.ID]; found {
				if !dryRun {
					err := store.CreateSyncedEvent(&SyncedEvent{
						UserID:           config.UserID,
						SourceCalendarID: source.CalendarID,
						SourceEventID:    event.ID,
						TargetCalendarID: hubCalID,
						TargetEventID:    placeholderBySourceID[event.ID].ID,
						SourceUpdated:    event.Updated,
					})
					if err != nil {
						result.addError("failed to adopt existing placeholder: %v", err)
					}
				}
				delete(syncedBySourceID, event.ID)
				continue
			}

			// CREATE
			if dryRun {
				result.Created++
				continue
			}
			created, err := cal.CreateEvent(ctx, token, hubCalID, &placeholder)
			if err != nil {
				result.addError("failed to create placeholder for %s: %v", event.Summary, err)
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
				result.addError("failed to store synced event: %v", err)
				continue
			}
			result.Created++
		} else if event.Updated > existingSynced.SourceUpdated {
			// UPDATE
			if dryRun {
				result.Updated++
				continue
			}
			_, err := cal.UpdateEvent(ctx, token, hubCalID, existingSynced.TargetEventID, &placeholder)
			if err != nil {
				if isNotFoundError(err) {
					log.Printf("placeholder for %s was deleted, will recreate next pass", event.Summary)
					store.DeleteSyncedEvent(existingSynced.ID)
				} else {
					result.addError("failed to update placeholder for %s: %v", event.Summary, err)
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

	// 6. Batch delete orphaned placeholders (source event no longer exists)
	if len(syncedBySourceID) > 0 {
		if dryRun {
			result.Deleted += len(syncedBySourceID)
		} else {
			var deleteIDs []string
			var deleteRecords []SyncedEvent
			for _, se := range syncedBySourceID {
				deleteIDs = append(deleteIDs, se.TargetEventID)
				deleteRecords = append(deleteRecords, se)
			}
			res := cal.BatchDeleteEvents(ctx, token, hubCalID, deleteIDs)
			result.Deleted += len(res.Gone)
			result.Errors += len(res.Failed)
			// Drop only the mappings whose placeholder is actually gone; a failed
			// delete keeps its record so the next pass retries (no orphaned placeholder).
			for _, se := range deleteRecords {
				if res.Gone[se.TargetEventID] {
					store.DeleteSyncedEvent(se.ID)
				}
			}
		}
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
func syncHubToSources(ctx context.Context, cal *calendar.Client, token string, store *Store, config *SyncConfig, sources []SourceCalendar, dryRun bool, result *SyncResult) error {
	hubCalID := config.HubCalendarID

	// Fetch ALL hub placeholders as ground truth for what should exist outbound.
	hubPlaceholders, err := ListSyncPlaceholders(ctx, cal, token, hubCalID)
	if err != nil {
		return fmt.Errorf("fetching hub placeholders: %w", err)
	}

	// Load the user's synced events once and reuse across every target calendar,
	// instead of scanning the full collection once per source.
	allSynced, err := store.GetSyncedEventsForUser(config.UserID)
	if err != nil {
		return fmt.Errorf("loading outbound synced events: %w", err)
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
		c0, u0, d0 := result.Created, result.Updated, result.Deleted
		if err := syncOutboundToSource(ctx, cal, token, store, config, &source, hubEvents, allSynced, dryRun, result); err != nil {
			result.addError("outbound sync error for %s: %v", source.CalendarName, err)
		}
		cc := result.calCounts(source.CalendarName)
		cc.Created += result.Created - c0
		cc.Updated += result.Updated - u0
		cc.Deleted += result.Deleted - d0
	}

	return nil
}

func syncOutboundToSource(ctx context.Context, cal *calendar.Client, token string, store *Store, config *SyncConfig, source *SourceCalendar, hubEvents []hubEvent, allSynced []SyncedEvent, dryRun bool, result *SyncResult) error {
	targetCalID := source.CalendarID

	// Filter the preloaded, user-scoped synced-event set to those targeting this
	// calendar, instead of running a per-source full-collection scan.
	var existingSynced []SyncedEvent
	for _, se := range allSynced {
		if se.TargetCalendarID == targetCalID {
			existingSynced = append(existingSynced, se)
		}
	}

	// Fetch existing placeholders on this target calendar for adoption after DB wipe
	existingPlaceholders, err := ListSyncPlaceholders(ctx, cal, token, targetCalID)
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
		// Outbound to source calendar: apply target calendar's visual options. The
		// change-detection value is the source's Updated as stamped on the hub
		// placeholder — NOT he.event.Updated (the hub placeholder's own timestamp, which
		// changes every time the hub placeholder is rewritten and would cause churn).
		srcUpdated := SourceUpdated(he.event)
		opts := PlaceholderOptions{
			EmojiPrefix: source.EmojiPrefix,
			ColorID:     source.ColorID,
		}
		placeholder := BuildPlaceholder(he.event, he.sourceCalID, he.sourceEventID, srcUpdated, opts)

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
					SourceUpdated:    srcUpdated,
				})
				if err != nil {
					result.addError("failed to adopt outbound placeholder: %v", err)
				}
				delete(syncedByKey, key)
				continue
			}

			// CREATE outbound placeholder
			if dryRun {
				result.Created++
				continue
			}
			created, err := cal.CreateEvent(ctx, token, targetCalID, &placeholder)
			if err != nil {
				// Silently skip read-only calendars (UC-0047)
				if isPermissionError(err) {
					log.Printf("skipping read-only calendar %s", source.CalendarName)
					return nil // skip entire calendar
				}
				result.addError("failed to create outbound placeholder on %s: %v", source.CalendarName, err)
				continue
			}
			err = store.CreateSyncedEvent(&SyncedEvent{
				UserID:           config.UserID,
				SourceCalendarID: he.sourceCalID,
				SourceEventID:    he.sourceEventID,
				TargetCalendarID: targetCalID,
				TargetEventID:    created.ID,
				SourceUpdated:    srcUpdated,
			})
			if err != nil {
				result.addError("failed to store outbound synced event: %v", err)
				continue
			}
			result.Created++
		} else if srcUpdated != "" && srcUpdated != existingSe.SourceUpdated {
			// UPDATE outbound placeholder. Compare by inequality, not ">": on the M8
			// transition, existing rows hold the old semantics (the hub placeholder's
			// Updated, always ≥ the source's), so ">" would never fire; "!=" rewrites the
			// row once with srcUpdated and then converges. The srcUpdated != "" guard
			// keeps pre-stamp hub placeholders a no-op until inbound re-stamps them.
			if dryRun {
				result.Updated++
				delete(syncedByKey, key)
				continue
			}
			_, err := cal.UpdateEvent(ctx, token, targetCalID, existingSe.TargetEventID, &placeholder)
			if err != nil {
				if isPermissionError(err) {
					return nil
				}
				if isNotFoundError(err) {
					log.Printf("outbound placeholder on %s was deleted, will recreate next pass", source.CalendarName)
					store.DeleteSyncedEvent(existingSe.ID)
					continue
				}
				result.addError("failed to update outbound placeholder on %s: %v", source.CalendarName, err)
				continue
			}
			existingSe.SourceUpdated = srcUpdated
			if err := store.UpdateSyncedEvent(&existingSe); err != nil {
				log.Printf("failed to update outbound synced event: %v", err)
			}
			result.Updated++
		}

		// Remove from map so orphan detection works
		delete(syncedByKey, key)
	}

	// Batch delete orphaned outbound placeholders
	var outboundDeleteIDs []string
	var outboundDeleteRecords []SyncedEvent
	for _, se := range syncedByKey {
		if se.TargetCalendarID != targetCalID {
			continue
		}
		outboundDeleteIDs = append(outboundDeleteIDs, se.TargetEventID)
		outboundDeleteRecords = append(outboundDeleteRecords, se)
	}
	if len(outboundDeleteIDs) > 0 {
		if dryRun {
			result.Deleted += len(outboundDeleteIDs)
		} else {
			res := cal.BatchDeleteEvents(ctx, token, targetCalID, outboundDeleteIDs)
			result.Deleted += len(res.Gone)
			result.Errors += len(res.Failed)
			for _, se := range outboundDeleteRecords {
				if res.Gone[se.TargetEventID] {
					store.DeleteSyncedEvent(se.ID)
				}
			}
		}
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
func cleanupRemovedSources(ctx context.Context, cal *calendar.Client, token string, store *Store, activeSources []SourceCalendar, allSynced []SyncedEvent, result *SyncResult) {
	// Build set of active source calendar IDs
	activeIDs := make(map[string]bool, len(activeSources))
	for _, s := range activeSources {
		activeIDs[s.CalendarID] = true
	}

	// Group deletions by target calendar for batching
	byTarget := make(map[string][]SyncedEvent)
	for _, se := range allSynced {
		if activeIDs[se.SourceCalendarID] {
			continue
		}
		byTarget[se.TargetCalendarID] = append(byTarget[se.TargetCalendarID], se)
	}

	for calID, records := range byTarget {
		var ids []string
		for _, se := range records {
			ids = append(ids, se.TargetEventID)
		}
		res := cal.BatchDeleteEvents(ctx, token, calID, ids)
		result.Deleted += len(res.Gone)
		result.Errors += len(res.Failed)
		for _, se := range records {
			if res.Gone[se.TargetEventID] {
				store.DeleteSyncedEvent(se.ID)
			}
			// A permanently-failing delete here retries every pass with no backstop
			// (the source is already out of config). Bounded retry via a DeleteAttempts
			// counter is deferred to the Phase 2/3 rework — see docs/M8-plan.md Phase 4.
		}
	}
}

// cleanupPastEvents deletes placeholder events whose end date is before today.
// Placeholders only exist to prevent meeting conflicts — past events don't need them.
func cleanupPastEvents(ctx context.Context, cal *calendar.Client, token string, store *Store, config *SyncConfig, sources []SourceCalendar, allSynced []SyncedEvent, result *SyncResult) {
	today := time.Now().UTC().Truncate(24 * time.Hour).Format("2006-01-02")

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
		placeholders, err := ListSyncPlaceholders(ctx, cal, token, calID)
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
			err := cal.DeleteEvent(ctx, token, calID, p.ID)
			if err != nil && !isNotFoundError(err) {
				result.addError("past cleanup: failed to delete %s: %v", p.ID, err)
				continue
			}

			// Remove the SyncedEvent record if one exists. Prefer the exact
			// placeholder match (TargetEventID == p.ID) over the (calendar, source
			// event) heuristic: allSynced is a pre-cleanup snapshot that may still
			// list a same-key record whose placeholder was already removed, and the
			// heuristic could match that ghost instead of the live record.
			srcEventID := SourceEventID(p)
			var match *SyncedEvent
			for i := range allSynced {
				se := &allSynced[i]
				if se.TargetEventID == p.ID {
					match = se
					break
				}
				if match == nil && se.TargetCalendarID == calID && se.SourceEventID == srcEventID {
					match = se
				}
			}
			if match != nil {
				store.DeleteSyncedEvent(match.ID)
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
