package platform

import (
	"testing"
	"time"
)

func TestUsableToken(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		token   string
		expiry  time.Time
		wantErr bool
	}{
		{"empty token", "", now.Add(time.Hour), true},
		{"valid, future expiry", "tok", now.Add(time.Hour), false},
		{"expired well past", "tok", now.Add(-time.Hour), true},
		{"zero expiry (unknown lifetime)", "tok", time.Time{}, true},
		{"just expired, within skew", "tok", now.Add(-5 * time.Second), false},
		{"expired beyond skew", "tok", now.Add(-2 * tokenSkew), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := usableToken(c.token, c.expiry)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got token %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.token {
				t.Fatalf("got %q, want %q", got, c.token)
			}
		})
	}
}
