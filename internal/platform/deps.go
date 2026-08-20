// Package platform holds the shared infrastructure that tool modules build on:
// the Deps they register against, and (in subpackages) the Calendar client.
package platform

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/michaelwinser/appbase/auth"
	"github.com/michaelwinser/appbase/db"

	"github.com/michaelwinser/calendar-sync/internal/platform/calendar"
)

// Deps is the shared infrastructure a module registers itself against. A module
// depends only on Deps — plus, when unavoidable, another module's exported API —
// never another module's routes, handlers, or store collections.
//
// The module contract is two package-level functions (no interface required):
//
//	func RegisterRoutes(deps platform.Deps) error  // API routes, called at setup
//	func RegisterPages(deps platform.Deps)          // authenticated pages, called at serve
//
// Conventions:
//   - API routes mount under /api/<module>/... ; appbase applies session auth by the
//     /api/ prefix. A route mounted outside /api/ (e.g. /sync/nudge) is unauthenticated
//     by design and must do its own auth.
//   - Pages register via LoginPage, which gates them behind login.
//   - A module owns its own store collections (built from DB) under a module prefix.
//   - The root catch-all page ("/*") is owned by exactly one module (currently
//     internal/app) — chi panics on a duplicate wildcard at the same level.
type Deps struct {
	Router    chi.Router                              // register API routes and pages here
	LoginPage func(http.HandlerFunc) http.HandlerFunc // wrap a page handler with the login gate
	DB        *db.DB
	Cal       *calendar.Client
	Google    *auth.GoogleAuth
}
