package app

import (
	"testing"
	"time"

	"github.com/michaelwinser/appbase/db"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.New(db.DBConfig{StoreType: "sqlite", SQLitePath: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	s, err := NewStore(d)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// GetRunningSyncLog must find a user's running log while ignoring completed logs
// and other users' running logs — the query filters by status, user is in-memory.
func TestGetRunningSyncLog(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetRunningSyncLog("u1")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("no logs: expected nil, got %+v", got)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	mustCreateLog(t, s, "u1", now, "running")
	mustCreateLog(t, s, "u2", now, "running")   // other user — must not match
	mustCreateLog(t, s, "u1", now, "completed") // completed — must not match

	got, err = s.GetRunningSyncLog("u1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.UserID != "u1" || got.Status != "running" {
		t.Fatalf("expected u1 running log, got %+v", got)
	}
}

// A running log older than the stale threshold is marked failed and treated as
// not running, so the guard doesn't block forever on a crashed sync.
func TestGetRunningSyncLogStale(t *testing.T) {
	s := newTestStore(t)
	stale := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	mustCreateLog(t, s, "u1", stale, "running")

	got, err := s.GetRunningSyncLog("u1")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("stale running log should not count, got %+v", got)
	}

	// It should now be marked failed, so a repeat call is still nil.
	if got, err = s.GetRunningSyncLog("u1"); err != nil || got != nil {
		t.Fatalf("after stale-marking: expected nil,nil got %+v,%v", got, err)
	}
}

// GetRecentSyncLogs returns at most `limit` newest-first, and trims the stored
// collection toward syncLogRetention — but no more than syncLogTrimBatch per call,
// so a large backlog drains over several calls instead of blocking one.
func TestSyncLogRetention(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UTC()
	total := syncLogRetention + syncLogTrimBatch + 20 // two calls' worth of excess
	for i := range total {
		ts := base.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		mustCreateLog(t, s, "u1", ts, "completed")
	}

	logs, err := s.GetRecentSyncLogs("u1", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 20 {
		t.Fatalf("expected 20 returned, got %d", len(logs))
	}
	if logs[0].StartedAt < logs[1].StartedAt {
		t.Fatalf("expected newest-first ordering")
	}

	// One call trims at most a batch, so excess remains.
	if got, want := countLogs(t, s, "u1"), syncLogRetention+20; got != want {
		t.Fatalf("after one trim: expected %d remaining, got %d", want, got)
	}

	// Subsequent calls converge to the retention cap.
	if _, err := s.GetRecentSyncLogs("u1", 20); err != nil {
		t.Fatal(err)
	}
	if got := countLogs(t, s, "u1"); got != syncLogRetention {
		t.Fatalf("after convergence: expected %d remaining, got %d", syncLogRetention, got)
	}
}

func mustCreateLog(t *testing.T, s *Store, userID, startedAt, status string) {
	t.Helper()
	if err := s.CreateSyncLog(&SyncLog{UserID: userID, StartedAt: startedAt, Status: status}); err != nil {
		t.Fatalf("CreateSyncLog: %v", err)
	}
}

// countLogs reads the raw log count without triggering the retention trim.
func countLogs(t *testing.T, s *Store, userID string) int {
	t.Helper()
	all, err := s.SyncLogs.Where("user_id", "==", userID).All()
	if err != nil {
		t.Fatalf("countLogs: %v", err)
	}
	return len(all)
}
