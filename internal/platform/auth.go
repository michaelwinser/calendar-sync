package platform

import (
	"fmt"
	"net/http"
	"time"

	"github.com/michaelwinser/appbase"
	"github.com/michaelwinser/appbase/auth"
)

// tokenSkew is the grace allowed past a token's expiry before we treat it as dead,
// absorbing clock skew and the sub-second gap between the middleware's refresh check
// and this one. Google tolerates a small skew of its own.
const tokenSkew = 30 * time.Second

// AccessToken returns the caller's Google access token for a Calendar API call.
//
// appbase's auth middleware already refreshes an expired token and puts the fresh
// value + expiry in the request context for /api/ routes (where our handlers live),
// so this does not refresh itself — it reads that token and validates it. If the
// token is missing or still expired here, the upstream refresh failed (or there is
// no refresh token), so it returns an error rather than sending Google a token we
// know is dead — which the old code did silently, turning every request into a
// misleading 401.
func AccessToken(r *http.Request) (string, error) {
	return usableToken(appbase.AccessToken(r), auth.TokenExpiry(r))
}

// usableToken validates a token+expiry pair. Split out from AccessToken so the
// decision is unit-testable (appbase's context-token setter is unexported).
func usableToken(token string, expiry time.Time) (string, error) {
	if token == "" {
		return "", fmt.Errorf("no Google API access token — re-login to grant Calendar permission")
	}
	// A zero expiry means the lifetime is unknown (refresh never established one);
	// a past expiry (beyond skew) means the upstream refresh failed. Either way the
	// token can't be trusted.
	if expiry.IsZero() || time.Now().After(expiry.Add(tokenSkew)) {
		return "", fmt.Errorf("Google access token expired and could not be refreshed — re-login")
	}
	return token, nil
}
