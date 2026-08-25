package calendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newDeleteStub(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL)
}

func assertSum(t *testing.T, res DeleteResult, n int) {
	t.Helper()
	if got := res.Deleted + res.Failed + len(res.Unprocessed); got != n {
		t.Fatalf("counts sum to %d, want %d: %+v", got, n, res)
	}
}

func TestDeleteEventsAllSucceed(t *testing.T) {
	c := newDeleteStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	ids := []string{"a", "b", "c", "d", "e"}
	res := c.DeleteEvents(context.Background(), "tok", "cal", ids, DeleteEventsOptions{RateLimit: 1000})
	if res.Deleted != len(ids) || res.Failed != 0 || len(res.Unprocessed) != 0 {
		t.Fatalf("got %+v, want all deleted", res)
	}
	assertSum(t, res, len(ids))
}

// 410/404 count as deleted (idempotent); a permanent 403 counts as failed.
func TestDeleteEventsMixedAndIdempotent(t *testing.T) {
	c := newDeleteStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "gone"):
			w.WriteHeader(http.StatusGone)
		case strings.Contains(r.URL.Path, "forbidden"):
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"errors":[{"reason":"forbidden"}]}}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	ids := []string{"ok1", "gone1", "forbidden1", "ok2", "gone2"}
	res := c.DeleteEvents(context.Background(), "tok", "cal", ids, DeleteEventsOptions{RateLimit: 1000})
	if res.Deleted != 4 { // ok1, ok2, gone1, gone2
		t.Errorf("Deleted = %d, want 4", res.Deleted)
	}
	if res.Failed != 1 { // forbidden1
		t.Errorf("Failed = %d, want 1", res.Failed)
	}
	if res.SampleError == "" {
		t.Errorf("expected a SampleError when a delete fails")
	}
	assertSum(t, res, len(ids))
}

// A tight deadline against a slow server leaves some IDs unprocessed (retryable),
// and the counts still fully account for every input ID.
func TestDeleteEventsDeadlinePartial(t *testing.T) {
	c := newDeleteStub(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	})
	ids := make([]string, 50)
	for i := range ids {
		ids[i] = "e" + strconv.Itoa(i)
	}
	res := c.DeleteEvents(context.Background(), "tok", "cal", ids,
		DeleteEventsOptions{Concurrency: 2, RateLimit: 1000, Deadline: 60 * time.Millisecond})
	if len(res.Unprocessed) == 0 {
		t.Fatalf("expected unprocessed IDs under a tight deadline, got %+v", res)
	}
	assertSum(t, res, len(ids))
}
