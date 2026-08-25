package calendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		status int
		reason string
		want   bool
	}{
		{429, "", true},
		{500, "", true},
		{503, "backendError", true},
		{403, "rateLimitExceeded", true},
		{403, "userRateLimitExceeded", true},
		{403, "quotaExceeded", true},
		{403, "RESOURCE_EXHAUSTED", true}, // status fallback when errors[] absent
		{403, "forbidden", false},         // permanent — must fail fast
		{403, "", false},                  // no reason — treat as permanent
		{404, "", false},
		{400, "badRequest", false},
		{200, "", false},
	}
	for _, c := range cases {
		if got := isRetryable(c.status, c.reason); got != c.want {
			t.Errorf("isRetryable(%d, %q) = %v, want %v", c.status, c.reason, got, c.want)
		}
	}
}

func TestErrorReason(t *testing.T) {
	body := []byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","errors":[{"reason":"rateLimitExceeded","message":"slow down"}]}}`)
	if got := errorReason(body); got != "rateLimitExceeded" {
		t.Errorf("errorReason = %q, want rateLimitExceeded", got)
	}
	if got := errorReason([]byte(`not json`)); got != "" {
		t.Errorf("errorReason(non-json) = %q, want empty", got)
	}
}

// A permanent 403 must fail fast — one request, no retries — so a bulk operation
// against a read-only calendar can't hang.
func TestPermanent403FailsFast(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"code":403,"errors":[{"reason":"forbidden"}]}}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.ListCalendars(context.Background(), "tok")
	if err == nil {
		t.Fatal("expected error on permanent 403")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("expected 1 request (no retries), got %d", n)
	}
}

// A rate-limit 403 is retried and can then succeed.
func TestRateLimit403Retries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"code":403,"errors":[{"reason":"rateLimitExceeded"}]}}`))
			return
		}
		w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	if _, err := c.ListCalendars(context.Background(), "tok"); err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("expected 2 requests (one retry), got %d", n)
	}
}

// The multipart boundary must stay a valid MIME token even when the calendar ID
// is email-shaped (contains '@', an RFC 2045 tspecial).
func TestBatchDeleteBoundaryIsValid(t *testing.T) {
	var boundary string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); strings.Contains(ct, "boundary=") {
			boundary = ct[strings.Index(ct, "boundary=")+len("boundary="):]
		}
		w.Header().Set("Content-Type", "multipart/mixed; boundary=resp")
		w.Write([]byte("--resp\r\n\r\nHTTP/1.1 204 No Content\r\n\r\n--resp\r\n\r\nHTTP/1.1 204 No Content\r\n\r\n--resp--\r\n"))
	}))
	defer srv.Close()

	c := New(srv.URL) // BatchURL = srv.URL + "/batch"
	deleted, errs := c.BatchDeleteEvents(context.Background(), "tok", "michaelw@xwind.io", []string{"a", "b"})
	if deleted != 2 || errs != 0 {
		t.Fatalf("deleted=%d errs=%d, want 2/0", deleted, errs)
	}
	if boundary == "" || len(boundary) > 70 || strings.ContainsAny(boundary, "@()<>,;:\\\"/[]?= \t") {
		t.Fatalf("invalid MIME boundary: %q", boundary)
	}
}

// Deleting an already-gone event (404/410) is success; a real error is not.
func TestDeleteEventIdempotent(t *testing.T) {
	cases := []struct {
		status  int
		wantErr bool
	}{
		{http.StatusNoContent, false},
		{http.StatusOK, false},
		{http.StatusGone, false},
		{http.StatusNotFound, false},
		{http.StatusForbidden, true},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			if tc.status >= 400 {
				w.Write([]byte(`{"error":{"errors":[{"reason":"forbidden"}]}}`))
			}
		}))
		c := New(srv.URL)
		err := c.DeleteEvent(context.Background(), "tok", "cal", "evt")
		srv.Close()
		if tc.wantErr && err == nil {
			t.Errorf("status %d: expected error", tc.status)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("status %d: expected success, got %v", tc.status, err)
		}
	}
}
