package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/michaelwinser/appbase/db"
	"github.com/michaelwinser/appbase/store"
)

// M8 migration: move the sync collections to the module-namespaced names and re-key
// synced_events by a deterministic point-lookup key. See docs/M8-plan.md Phase 2.
//
// The old collections are read-only here and untouched until DeleteOld runs — a copy
// into new collections, so the old data is the rollback until the destructive step.
const (
	oldSyncedEvents    = "synced_events"
	newSyncedEvents    = "sync_synced_events"
	oldSourceCalendars = "source_calendars"
	newSourceCalendars = "sync_source_calendars"
)

// SyncedEventKey is the deterministic point-lookup key for a mapping: a SHA-256 of the
// (user, source calendar, source event, target calendar) 4-tuple. Using it as the
// Firestore doc id lets a changed event's record be fetched with one Get instead of a
// full-collection scan — the enabler for two-tier sync. NUL separators can't appear in
// the parts (Google ids / emails), so the join is unambiguous.
func SyncedEventKey(userID, sourceCalID, sourceEventID, targetCalID string) string {
	h := sha256.New()
	for _, p := range []string{userID, sourceCalID, sourceEventID, targetCalID} {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (se SyncedEvent) key() string {
	return SyncedEventKey(se.UserID, se.SourceCalendarID, se.SourceEventID, se.TargetCalendarID)
}

// upsert writes entity under id, updating if it already exists and creating otherwise.
// This makes the copy idempotent on both backends: Firestore's create is already an
// upsert (Doc.Set), but SQLite's is an INSERT that fails on a duplicate pk, so a re-run
// after a partial failure would error without this. Get returns (nil, nil) for a missing
// id on both backends.
func upsert[T any](col *store.Collection[T], id string, entity *T) error {
	existing, err := col.Get(id)
	if err != nil {
		return err
	}
	if existing != nil {
		return col.Update(id, entity)
	}
	return col.Create(entity)
}

// Collision is one 4-tuple that several old records map to. Under the deterministic key
// they'd overwrite each other; the migration keeps the newest by UpdatedAt and reports
// each loser whose TargetEventID differs (a real, now-untracked placeholder) for manual
// review — never dropping a mapping silently.
type Collision struct {
	Key      string
	WinnerID string // old record id kept
	Losers   []LoserPlaceholder
}

// LoserPlaceholder is a placeholder that will be orphaned by a collision (its mapping
// loses to a newer record with a different TargetEventID). Listed for the operator to
// delete on Google; the migration itself makes no Google writes.
type LoserPlaceholder struct {
	OldRecordID      string
	TargetCalendarID string
	TargetEventID    string
}

// MigrationReport summarizes a dry-run or copy.
type MigrationReport struct {
	SyncedOldCount    int
	SyncedDistinctKey int // = records written on copy
	SyncedWritten     int // 0 on dry-run
	Collisions        []Collision
	SourceOldCount    int
	SourceWritten     int
}

// planSyncedEvents buckets records by deterministic key, picks the newest-UpdatedAt
// winner per key (id as a deterministic tie-break), and returns the re-keyed winners
// plus the collisions that lose a distinct placeholder.
func planSyncedEvents(recs []SyncedEvent) (winners map[string]SyncedEvent, collisions []Collision) {
	buckets := map[string][]SyncedEvent{}
	for _, r := range recs {
		k := r.key()
		buckets[k] = append(buckets[k], r)
	}
	winners = make(map[string]SyncedEvent, len(buckets))
	// Iterate keys in sorted order so the collision report is deterministic.
	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		group := buckets[k]
		sort.Slice(group, func(i, j int) bool {
			if group[i].UpdatedAt != group[j].UpdatedAt {
				return group[i].UpdatedAt > group[j].UpdatedAt // newest first
			}
			return group[i].ID > group[j].ID
		})
		winner := group[0]
		oldWinnerID := winner.ID
		winner.ID = k // re-key to the deterministic id
		winners[k] = winner

		var losers []LoserPlaceholder
		for _, loser := range group[1:] {
			if loser.TargetEventID != winner.TargetEventID {
				losers = append(losers, LoserPlaceholder{
					OldRecordID:      loser.ID,
					TargetCalendarID: loser.TargetCalendarID,
					TargetEventID:    loser.TargetEventID,
				})
			}
		}
		if len(losers) > 0 {
			collisions = append(collisions, Collision{Key: k, WinnerID: oldWinnerID, Losers: losers})
		}
	}
	return winners, collisions
}

// MigrateSyncedEvents plans (and, when apply is true, performs) the synced_events copy
// into the re-keyed sync_synced_events collection. Idempotent: the deterministic id +
// upsert means a re-run after a partial failure converges. Old collection untouched.
func MigrateSyncedEvents(d *db.DB, apply bool) (*MigrationReport, error) {
	old, err := store.NewCollection[SyncedEvent](d, oldSyncedEvents)
	if err != nil {
		return nil, err
	}
	recs, err := old.All()
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", oldSyncedEvents, err)
	}
	winners, collisions := planSyncedEvents(recs)
	rep := &MigrationReport{
		SyncedOldCount:    len(recs),
		SyncedDistinctKey: len(winners),
		Collisions:        collisions,
	}
	if !apply {
		return rep, nil
	}
	newCol, err := store.NewCollection[SyncedEvent](d, newSyncedEvents)
	if err != nil {
		return nil, err
	}
	for _, w := range winners {
		if err := upsert(newCol, w.ID, &w); err != nil { // idempotent by deterministic id
			return rep, fmt.Errorf("writing %s: %w", w.ID, err)
		}
		rep.SyncedWritten++
	}
	return rep, nil
}

// MigrateSourceCalendars copies source_calendars into sync_source_calendars unchanged
// (no re-key — they're few and looked up by user, not point-fetched). Idempotent.
func MigrateSourceCalendars(d *db.DB, apply bool, rep *MigrationReport) error {
	old, err := store.NewCollection[SourceCalendar](d, oldSourceCalendars)
	if err != nil {
		return err
	}
	recs, err := old.All()
	if err != nil {
		return fmt.Errorf("reading %s: %w", oldSourceCalendars, err)
	}
	rep.SourceOldCount = len(recs)
	if !apply {
		return nil
	}
	newCol, err := store.NewCollection[SourceCalendar](d, newSourceCalendars)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if err := upsert(newCol, r.ID, &r); err != nil { // keeps the existing id
			return fmt.Errorf("writing source calendar %s: %w", r.ID, err)
		}
		rep.SourceWritten++
	}
	return nil
}

// VerifyMigration checks the new collections structurally against the old: for
// synced_events every new doc's id equals its own computed key, the new key-set equals
// the expected winner key-set (not just len==len), and each new doc's TargetEventID
// matches the chosen winner (a field-value check, not just key presence); for
// source_calendars the id-set is copied verbatim. Returns a descriptive error on any
// mismatch. It is the delete-old safety gate, so it also fails when the copy never ran
// (new empty, old non-empty → count mismatch).
//
// Two caveats for the operator: it does NOT confirm each TargetEventID still resolves on
// Google (that needs a user token — a separate online spot-check), and it recomputes
// winners from the old collections, which the app may still be writing to. So verify is
// only meaningful immediately after a `copy`, ideally with sync paused (see the runbook).
func VerifyMigration(d *db.DB) error {
	old, err := store.NewCollection[SyncedEvent](d, oldSyncedEvents)
	if err != nil {
		return err
	}
	oldRecs, err := old.All()
	if err != nil {
		return err
	}
	winners, _ := planSyncedEvents(oldRecs)

	newCol, err := store.NewCollection[SyncedEvent](d, newSyncedEvents)
	if err != nil {
		return err
	}
	newRecs, err := newCol.All()
	if err != nil {
		return err
	}

	newByKey := make(map[string]SyncedEvent, len(newRecs))
	for _, r := range newRecs {
		if r.ID != r.key() {
			return fmt.Errorf("verify: doc %q id does not match its computed key %q", r.ID, r.key())
		}
		newByKey[r.ID] = r
	}
	if len(newByKey) != len(winners) {
		return fmt.Errorf("verify: new synced_events has %d distinct keys, expected %d (was copy run?)", len(newByKey), len(winners))
	}
	for k, w := range winners {
		nr, ok := newByKey[k]
		if !ok {
			return fmt.Errorf("verify: expected key %q missing from new collection", k)
		}
		if nr.TargetEventID != w.TargetEventID {
			return fmt.Errorf("verify: doc %q has TargetEventID %q, expected winner's %q", k, nr.TargetEventID, w.TargetEventID)
		}
	}

	// source_calendars: copied verbatim, ids preserved.
	oldSrc, err := store.NewCollection[SourceCalendar](d, oldSourceCalendars)
	if err != nil {
		return err
	}
	oldSrcRecs, err := oldSrc.All()
	if err != nil {
		return err
	}
	newSrc, err := store.NewCollection[SourceCalendar](d, newSourceCalendars)
	if err != nil {
		return err
	}
	newSrcRecs, err := newSrc.All()
	if err != nil {
		return err
	}
	newSrcIDs := make(map[string]bool, len(newSrcRecs))
	for _, s := range newSrcRecs {
		newSrcIDs[s.ID] = true
	}
	if len(newSrcRecs) != len(oldSrcRecs) {
		return fmt.Errorf("verify: new source_calendars has %d records, expected %d", len(newSrcRecs), len(oldSrcRecs))
	}
	for _, s := range oldSrcRecs {
		if !newSrcIDs[s.ID] {
			return fmt.Errorf("verify: source calendar %q missing from new collection", s.ID)
		}
	}
	return nil
}

// DeleteOld removes every document from the old synced_events and source_calendars
// collections — the destructive final step, run days after the read-switch once the new
// collections are confirmed healthy. Returns the counts deleted.
func DeleteOld(d *db.DB) (syncedDeleted, sourceDeleted int, err error) {
	// Safety gate: never drop the rollback unless the new collections are a verified
	// copy. If copy never ran, verify fails (new empty, old non-empty) and we refuse.
	if err := VerifyMigration(d); err != nil {
		return 0, 0, fmt.Errorf("refusing delete-old: run copy + verify first: %w", err)
	}

	oldSynced, err := store.NewCollection[SyncedEvent](d, oldSyncedEvents)
	if err != nil {
		return 0, 0, err
	}
	syncedRecs, err := oldSynced.All()
	if err != nil {
		return 0, 0, err
	}
	for _, r := range syncedRecs {
		if err := oldSynced.Delete(r.ID); err != nil {
			return syncedDeleted, 0, fmt.Errorf("deleting old synced_event %s: %w", r.ID, err)
		}
		syncedDeleted++
	}

	oldSrc, err := store.NewCollection[SourceCalendar](d, oldSourceCalendars)
	if err != nil {
		return syncedDeleted, 0, err
	}
	srcRecs, err := oldSrc.All()
	if err != nil {
		return syncedDeleted, 0, err
	}
	for _, r := range srcRecs {
		if err := oldSrc.Delete(r.ID); err != nil {
			return syncedDeleted, sourceDeleted, fmt.Errorf("deleting old source_calendar %s: %w", r.ID, err)
		}
		sourceDeleted++
	}
	return syncedDeleted, sourceDeleted, nil
}
