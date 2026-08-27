// Calendar Sync — keeps free/busy status consistent across multiple Google Calendars.
//
// Uses a hub calendar model: events from each source calendar are synced to a
// central hub, then placeholders are synced out to all other calendars.
//
// Server:
//
//	go run . serve
//
// CLI:
//
//	go run . sync
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/michaelwinser/appbase"
	appcli "github.com/michaelwinser/appbase/cli"

	"github.com/michaelwinser/calendar-sync/internal/calendars"
	"github.com/michaelwinser/calendar-sync/internal/heatmap"
	"github.com/michaelwinser/calendar-sync/internal/platform"
	"github.com/michaelwinser/calendar-sync/internal/platform/calendar"
	"github.com/michaelwinser/calendar-sync/internal/sync"
	"github.com/michaelwinser/calendar-sync/internal/tools"
)

var (
	a    *appbase.App
	deps platform.Deps
)

func setup() error {
	var err error
	cfg := appbase.Config{
		Name:  "calendar-sync",
		Quiet: !appcli.IsServeCommand,
		// No LocalMode — this app always requires Google OAuth for calendar access.
	}
	if appcli.LocalDataPath != "" {
		cfg.DB.SQLitePath = appcli.LocalDataPath + "/app.db"
	}
	a, err = appbase.New(cfg)
	if err != nil {
		return err
	}

	// CALENDAR_API_BASE lets tests point the client at a local fake; empty = real Google.
	cal := calendar.New(os.Getenv("CALENDAR_API_BASE"))
	deps = platform.Deps{
		Router:    a.Router(),
		LoginPage: a.LoginPage,
		DB:        a.DB(),
		Cal:       cal,
		Google:    a.Google(),
	}

	// Mount modules. Each registers its own routes (API under /api/, plus any
	// non-/api endpoints such as the nudge, which does its own auth).
	if err := calendars.RegisterRoutes(deps); err != nil {
		return err
	}
	if err := sync.RegisterRoutes(deps); err != nil {
		return err
	}
	if err := tools.RegisterRoutes(deps); err != nil {
		return err
	}
	if err := heatmap.RegisterRoutes(deps); err != nil {
		return err
	}
	return nil
}

func main() {
	cliApp := appcli.New("calendar-sync", "Google Calendar synchronization", setup)

	cliApp.SetServeFunc(func() error {
		// Register each module's authenticated pages, then serve.
		sync.RegisterPages(deps)
		tools.RegisterPages(deps)
		heatmap.RegisterPages(deps)
		return a.Serve()
	})

	// CLI: sync command
	syncCmd := &cobra.Command{
		Use:   "sync",
		Short: "Run a calendar sync pass",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := setup(); err != nil {
				return err
			}
			httpClient, baseURL, cleanup, err := appcli.ClientForCommand(cmd, "calendar-sync", a.Handler())
			if err != nil {
				return err
			}
			defer cleanup()

			syncURL := baseURL + "/api/sync"
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			if dryRun {
				syncURL += "?dryRun=true"
			}

			resp, err := httpClient.Post(syncURL, "application/json", nil)
			if err != nil {
				return err
			}
			defer resp.Body.Close()

			var result sync.SyncResult
			if err := decodeJSON(resp, &result); err != nil {
				return err
			}
			fmt.Println(result.Message)
			return nil
		},
	}
	syncCmd.Flags().Bool("dry-run", false, "Report what would change without making API writes")
	cliApp.AddCommand(syncCmd)

	cliApp.AddCommand(migrateCommand())

	cliApp.Execute()
}

// migrateCommand builds the one-shot M8 data migration (namespace + point-lookup re-key
// of synced_events). Non-destructive until `delete-old`; see docs/M8-plan.md Phase 2.
func migrateCommand() *cobra.Command {
	migrateCmd := &cobra.Command{
		Use:   "migrate",
		Short: "M8 sync-collection migration (namespace + point-lookup re-key)",
	}

	plan := func(apply bool) error {
		if err := setup(); err != nil {
			return err
		}
		rep, err := sync.MigrateSyncedEvents(a.DB(), apply)
		if err != nil {
			if rep != nil { // print partial progress before failing
				printMigrationReport(rep, apply)
			}
			return err
		}
		if err := sync.MigrateSourceCalendars(a.DB(), apply, rep); err != nil {
			printMigrationReport(rep, apply)
			return err
		}
		printMigrationReport(rep, apply)
		return nil
	}

	migrateCmd.AddCommand(&cobra.Command{
		Use:   "dry-run",
		Short: "Report what the migration would copy and any collisions, without writing",
		RunE:  func(_ *cobra.Command, _ []string) error { return plan(false) },
	})
	migrateCmd.AddCommand(&cobra.Command{
		Use:   "copy",
		Short: "Copy into the re-keyed sync_* collections (idempotent; old collections untouched)",
		RunE:  func(_ *cobra.Command, _ []string) error { return plan(true) },
	})
	migrateCmd.AddCommand(&cobra.Command{
		Use:   "verify",
		Short: "Check the new collection's keys structurally match the old data",
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := setup(); err != nil {
				return err
			}
			if err := sync.VerifyMigration(a.DB()); err != nil {
				return err
			}
			fmt.Println("verify: OK — new sync_synced_events matches the old data by key-set")
			return nil
		},
	})
	deleteOld := &cobra.Command{
		Use:   "delete-old",
		Short: "DESTRUCTIVE: delete the old synced_events/source_calendars (run days after cutover)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if yes, _ := cmd.Flags().GetBool("yes"); !yes {
				return fmt.Errorf("refusing to delete without --yes (this removes the migration rollback)")
			}
			if err := setup(); err != nil {
				return err
			}
			syncedDeleted, sourceDeleted, err := sync.DeleteOld(a.DB())
			if err != nil {
				return err
			}
			fmt.Printf("Deleted %d old synced_events and %d old source_calendars.\n", syncedDeleted, sourceDeleted)
			return nil
		},
	}
	deleteOld.Flags().Bool("yes", false, "Confirm the destructive delete")
	migrateCmd.AddCommand(deleteOld)

	return migrateCmd
}

// printMigrationReport prints the plan/result, surfacing collisions the operator must
// review (a losing placeholder is a real event that will be untracked after cutover).
func printMigrationReport(rep *sync.MigrationReport, applied bool) {
	verb := "would copy"
	if applied {
		verb = "copied"
	}
	fmt.Printf("synced_events: %d old records → %d distinct keys (%s %d)\n",
		rep.SyncedOldCount, rep.SyncedDistinctKey, verb, rep.SyncedWritten)
	fmt.Printf("source_calendars: %d records (%s %d)\n", rep.SourceOldCount, verb, rep.SourceWritten)

	if len(rep.Collisions) == 0 {
		fmt.Println("No collisions.")
		return
	}
	fmt.Printf("\n%d COLLISION(S) — newest UpdatedAt kept; review these orphaned placeholders:\n", len(rep.Collisions))
	for _, c := range rep.Collisions {
		for _, l := range c.Losers {
			fmt.Printf("  key %s… : placeholder %s on calendar %s is now untracked (delete on Google if stale)\n",
				c.Key[:12], l.TargetEventID, l.TargetCalendarID)
		}
	}
}

func decodeJSON(resp *http.Response, v interface{}) error {
	if resp.StatusCode >= 400 {
		var errResp struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&errResp)
		if errResp.Error != "" {
			return fmt.Errorf("%s", errResp.Error)
		}
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
