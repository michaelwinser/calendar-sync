package sync

import "github.com/michaelwinser/calendar-sync/internal/platform"

// RegisterRoutes wires the sync module's API routes (sync, config, status) plus the
// unauthenticated nudge endpoint. Called at setup. It builds the module's store
// from the shared DB.
func RegisterRoutes(deps platform.Deps) error {
	store, err := NewStore(deps.DB)
	if err != nil {
		return err
	}
	s := &Server{Store: store, Google: deps.Google, Cal: deps.Cal}
	s.registerAPI(deps.Router)
	// Nudge is mounted outside /api/ to bypass session auth; it does its own auth.
	deps.Router.Post("/sync/nudge", s.NudgeSync)
	return nil
}

// RegisterPages wires the app's authenticated HTML pages. Called at serve.
// The root catch-all ("/*") is owned here; the Tools page is owned by internal/tools.
func RegisterPages(deps platform.Deps) {
	deps.Router.Get("/*", deps.LoginPage(homeHandler))
}
