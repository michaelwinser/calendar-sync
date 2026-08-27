package sync

import (
	"testing"

	"github.com/michaelwinser/appbase/db"
	"github.com/michaelwinser/appbase/store"
)

func newTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.New(db.DBConfig{StoreType: "sqlite", SQLitePath: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func seedOldSynced(t *testing.T, d *db.DB, recs ...SyncedEvent) {
	t.Helper()
	col, err := store.NewCollection[SyncedEvent](d, oldSyncedEvents)
	if err != nil {
		t.Fatalf("old collection: %v", err)
	}
	for i := range recs {
		if err := col.Create(&recs[i]); err != nil {
			t.Fatalf("seed %s: %v", recs[i].ID, err)
		}
	}
}

func newSyncedByKey(t *testing.T, d *db.DB) map[string]SyncedEvent {
	t.Helper()
	col, err := store.NewCollection[SyncedEvent](d, newSyncedEvents)
	if err != nil {
		t.Fatalf("new collection: %v", err)
	}
	recs, err := col.All()
	if err != nil {
		t.Fatalf("new All: %v", err)
	}
	m := make(map[string]SyncedEvent, len(recs))
	for _, r := range recs {
		m[r.ID] = r
	}
	return m
}

func TestSyncedEventKeyIsDeterministic(t *testing.T) {
	a := SyncedEventKey("u", "src", "ev", "tgt")
	b := SyncedEventKey("u", "src", "ev", "tgt")
	if a != b {
		t.Fatal("same inputs must yield same key")
	}
	// A field boundary shift must change the key (NUL separators prevent collisions).
	if SyncedEventKey("u", "src", "ev", "tgt") == SyncedEventKey("u", "sr", "cev", "tgt") {
		t.Fatal("different field boundaries must yield different keys")
	}
}

func TestMigrationHappyPath(t *testing.T) {
	d := newTestDB(t)
	seedOldSynced(t, d,
		SyncedEvent{ID: "r1", UserID: "u", SourceCalendarID: "a", SourceEventID: "e1", TargetCalendarID: "hub", TargetEventID: "p1", UpdatedAt: "2026-01-01T00:00:00Z"},
		SyncedEvent{ID: "r2", UserID: "u", SourceCalendarID: "a", SourceEventID: "e2", TargetCalendarID: "hub", TargetEventID: "p2", UpdatedAt: "2026-01-01T00:00:00Z"},
		SyncedEvent{ID: "r3", UserID: "u", SourceCalendarID: "b", SourceEventID: "e1", TargetCalendarID: "hub", TargetEventID: "p3", UpdatedAt: "2026-01-01T00:00:00Z"},
	)

	dry, err := MigrateSyncedEvents(d, false)
	if err != nil {
		t.Fatal(err)
	}
	if dry.SyncedOldCount != 3 || dry.SyncedDistinctKey != 3 || len(dry.Collisions) != 0 || dry.SyncedWritten != 0 {
		t.Fatalf("dry-run: %+v", dry)
	}

	rep, err := MigrateSyncedEvents(d, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SyncedWritten != 3 {
		t.Fatalf("want 3 written, got %d", rep.SyncedWritten)
	}
	if err := VerifyMigration(d); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Every new doc is keyed by its 4-tuple hash and preserves its placeholder.
	byKey := newSyncedByKey(t, d)
	want := SyncedEventKey("u", "a", "e1", "hub")
	if r, ok := byKey[want]; !ok || r.TargetEventID != "p1" {
		t.Fatalf("re-keyed record for (u,a,e1,hub) missing/wrong: %+v", byKey)
	}
}

func TestMigrationCollisionKeepsNewestAndListsLoser(t *testing.T) {
	d := newTestDB(t)
	// Two records for the SAME 4-tuple with DIFFERENT placeholders — a real collision.
	seedOldSynced(t, d,
		SyncedEvent{ID: "old", UserID: "u", SourceCalendarID: "a", SourceEventID: "e1", TargetCalendarID: "hub", TargetEventID: "p_old", UpdatedAt: "2026-01-01T00:00:00Z"},
		SyncedEvent{ID: "new", UserID: "u", SourceCalendarID: "a", SourceEventID: "e1", TargetCalendarID: "hub", TargetEventID: "p_new", UpdatedAt: "2026-06-01T00:00:00Z"},
	)

	dry, err := MigrateSyncedEvents(d, false)
	if err != nil {
		t.Fatal(err)
	}
	if dry.SyncedDistinctKey != 1 {
		t.Fatalf("want 1 distinct key, got %d", dry.SyncedDistinctKey)
	}
	if len(dry.Collisions) != 1 {
		t.Fatalf("want 1 collision reported, got %d", len(dry.Collisions))
	}
	col := dry.Collisions[0]
	if col.WinnerID != "new" {
		t.Fatalf("newest UpdatedAt should win, got winner %q", col.WinnerID)
	}
	if len(col.Losers) != 1 || col.Losers[0].TargetEventID != "p_old" {
		t.Fatalf("loser placeholder p_old should be listed for review, got %+v", col.Losers)
	}

	if _, err := MigrateSyncedEvents(d, true); err != nil {
		t.Fatal(err)
	}
	byKey := newSyncedByKey(t, d)
	if len(byKey) != 1 {
		t.Fatalf("collision should collapse to 1 record, got %d", len(byKey))
	}
	for _, r := range byKey {
		if r.TargetEventID != "p_new" {
			t.Fatalf("winner should keep newest placeholder p_new, got %q", r.TargetEventID)
		}
	}
}

func TestMigrationExactDuplicateIsNotACollision(t *testing.T) {
	d := newTestDB(t)
	// Same 4-tuple AND same placeholder → a harmless dupe, not a data-loss collision.
	seedOldSynced(t, d,
		SyncedEvent{ID: "r1", UserID: "u", SourceCalendarID: "a", SourceEventID: "e1", TargetCalendarID: "hub", TargetEventID: "p1", UpdatedAt: "2026-01-01T00:00:00Z"},
		SyncedEvent{ID: "r2", UserID: "u", SourceCalendarID: "a", SourceEventID: "e1", TargetCalendarID: "hub", TargetEventID: "p1", UpdatedAt: "2026-02-01T00:00:00Z"},
	)
	dry, err := MigrateSyncedEvents(d, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.Collisions) != 0 {
		t.Fatalf("exact duplicate should not be reported as a collision, got %+v", dry.Collisions)
	}
	if dry.SyncedDistinctKey != 1 {
		t.Fatalf("want 1 distinct key, got %d", dry.SyncedDistinctKey)
	}
}

func TestMigrationIsIdempotent(t *testing.T) {
	d := newTestDB(t)
	seedOldSynced(t, d,
		SyncedEvent{ID: "r1", UserID: "u", SourceCalendarID: "a", SourceEventID: "e1", TargetCalendarID: "hub", TargetEventID: "p1", UpdatedAt: "2026-01-01T00:00:00Z"},
	)
	if _, err := MigrateSyncedEvents(d, true); err != nil {
		t.Fatal(err)
	}
	// Re-run (simulating recovery after a partial failure): upsert must converge, not
	// duplicate or error.
	rep, err := MigrateSyncedEvents(d, true)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if rep.SyncedWritten != 1 {
		t.Fatalf("re-run should rewrite the 1 record, got %d", rep.SyncedWritten)
	}
	if byKey := newSyncedByKey(t, d); len(byKey) != 1 {
		t.Fatalf("re-run must not duplicate, got %d records", len(byKey))
	}
	if err := VerifyMigration(d); err != nil {
		t.Fatalf("verify after re-run: %v", err)
	}
}

func seedOldSource(t *testing.T, d *db.DB, recs ...SourceCalendar) {
	t.Helper()
	col, err := store.NewCollection[SourceCalendar](d, oldSourceCalendars)
	if err != nil {
		t.Fatalf("old source collection: %v", err)
	}
	for i := range recs {
		if err := col.Create(&recs[i]); err != nil {
			t.Fatalf("seed source %s: %v", recs[i].ID, err)
		}
	}
}

func TestMigrateSourceCalendarsPreservesSyncTokenAndIsIdempotent(t *testing.T) {
	d := newTestDB(t)
	seedOldSource(t, d, SourceCalendar{ID: "s1", UserID: "u", CalendarID: "a", SyncToken: "tok-123"})

	rep := &MigrationReport{}
	if err := MigrateSourceCalendars(d, true, rep); err != nil {
		t.Fatal(err)
	}
	if rep.SourceWritten != 1 {
		t.Fatalf("want 1 source written, got %d", rep.SourceWritten)
	}
	newCol, err := store.NewCollection[SourceCalendar](d, newSourceCalendars)
	if err != nil {
		t.Fatal(err)
	}
	got, err := newCol.Get("s1")
	if err != nil || got == nil {
		t.Fatalf("copied source calendar missing: %v", err)
	}
	if got.SyncToken != "tok-123" {
		t.Fatalf("SyncToken must survive the copy, got %q", got.SyncToken)
	}
	// Re-run: idempotent, no duplication.
	if err := MigrateSourceCalendars(d, true, &MigrationReport{}); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if all, _ := newCol.All(); len(all) != 1 {
		t.Fatalf("re-run must not duplicate source calendars, got %d", len(all))
	}
}

func TestDeleteOldRefusesWithoutVerifiedCopy(t *testing.T) {
	d := newTestDB(t)
	seedOldSynced(t, d,
		SyncedEvent{ID: "r1", UserID: "u", SourceCalendarID: "a", SourceEventID: "e1", TargetCalendarID: "hub", TargetEventID: "p1", UpdatedAt: "2026-01-01T00:00:00Z"},
	)
	// copy never ran → new collection empty → delete-old must refuse (no rollback loss).
	if _, _, err := DeleteOld(d); err == nil {
		t.Fatal("delete-old should refuse when the copy has not run")
	}
	old, _ := store.NewCollection[SyncedEvent](d, oldSyncedEvents)
	if recs, _ := old.All(); len(recs) != 1 {
		t.Fatalf("old data must be intact after a refused delete-old, got %d", len(recs))
	}
}

func TestDeleteOldRemovesOldCollectionsAfterCopy(t *testing.T) {
	d := newTestDB(t)
	seedOldSynced(t, d,
		SyncedEvent{ID: "r1", UserID: "u", SourceCalendarID: "a", SourceEventID: "e1", TargetCalendarID: "hub", TargetEventID: "p1", UpdatedAt: "2026-01-01T00:00:00Z"},
	)
	seedOldSource(t, d, SourceCalendar{ID: "s1", UserID: "u", CalendarID: "a", SyncToken: "tok"})

	rep, err := MigrateSyncedEvents(d, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := MigrateSourceCalendars(d, true, rep); err != nil {
		t.Fatal(err)
	}

	syncedDeleted, sourceDeleted, err := DeleteOld(d)
	if err != nil {
		t.Fatalf("delete-old after verified copy: %v", err)
	}
	if syncedDeleted != 1 || sourceDeleted != 1 {
		t.Fatalf("want 1/1 deleted, got %d/%d", syncedDeleted, sourceDeleted)
	}
	old, _ := store.NewCollection[SyncedEvent](d, oldSyncedEvents)
	if recs, _ := old.All(); len(recs) != 0 {
		t.Fatalf("old synced_events should be empty, got %d", len(recs))
	}
	// New collection survives the delete of the old.
	newCol, _ := store.NewCollection[SyncedEvent](d, newSyncedEvents)
	if recs, _ := newCol.All(); len(recs) != 1 {
		t.Fatalf("new collection must survive delete-old, got %d", len(recs))
	}
}
