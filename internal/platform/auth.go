package platform

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/michaelwinser/appbase"
	"github.com/michaelwinser/appbase/auth"
)

// AccessToken returns the caller's Google access token, refreshing it if expired.
// Shared by every module that calls the Calendar API on behalf of the logged-in user.
func AccessToken(r *http.Request, google *auth.GoogleAuth) (string, error) {
	token := appbase.AccessToken(r)
	if token == "" {
		return "", fmt.Errorf("no Google API access token — re-login to grant Calendar permission")
	}

	// Attempt refresh if expired.
	expiry := auth.TokenExpiry(r)
	if !expiry.IsZero() && time.Now().After(expiry) && google != nil {
		refreshToken := auth.RefreshToken(r)
		if refreshToken != "" {
			session := &auth.Session{
				AccessToken:  token,
				RefreshToken: refreshToken,
				TokenExpiry:  expiry,
			}
			newToken, err := google.RefreshAccessToken(r.Context(), session)
			if err != nil {
				log.Printf("token refresh failed: %v", err)
				// Return expired token; caller will get 401 from Google.
				return token, nil
			}
			return newToken, nil
		}
	}

	return token, nil
}
