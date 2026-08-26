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

	cliApp.Execute()
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
