package heatmap

import (
	_ "embed"
	"net/http"
)

//go:embed page.html
var pageHTML []byte

// page serves the heatmap UI. It's a static asset (go:embed) that fetches data at
// runtime from /api/calendars and /api/heatmap/events.
func page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(pageHTML)
}
