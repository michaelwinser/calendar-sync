# M1 Implementation Plan

## Goal

Basic app that can login via a Google Account and get Google Calendar scopes. Sandbox, dev tooling, and project scaffold in place.

## Use Cases Covered

UC-0001 through UC-0005 (app starts, login page, Google OAuth, auth status, logout).

## Deliverables

### 1. Project scaffold

**Files to create:**

| File | Purpose |
|------|---------|
| `go.mod` / `go.sum` | Go module (`github.com/michaelwinser/calendar-sync`), depends on appbase |
| `main.go` | Entry point: setup app, register routes, serve. CLI commands: `serve`. No `LocalMode`. |
| `app.yaml` | Config: name, port, store (sqlite), auth (client_id, client_secret, calendar scopes) |
| `app.json` | Project identity for appbase CLI (name, gcpProject, region, urls) |
| `.gitignore` | Standard Go + appbase ignores (data/, .env, .docker/, *.db) |
| `dev` | Project dev script sourcing `appbase dev-template` |
| `sandbox` | nono sandbox wrapper customized for this project |
| `CLAUDE.md` | Already exists — add sandbox section |

**main.go structure** (following todo-api pattern):
```go
func setup() error {
    cfg := appbase.Config{
        Name: "calendar-sync",
        Quiet: !appcli.IsServeCommand,
        // No LocalMode — this app always requires Google OAuth
    }
    app, err = appbase.New(cfg)
    // ... register routes
}

func main() {
    cliApp := appcli.New("calendar-sync", "Google Calendar synchronization", setup)
    cliApp.SetServeFunc(func() error {
        // Serve login page for unauthenticated, main page for authenticated
        return app.Serve()
    })
    cliApp.Execute()
}
```

### 2. Sandbox setup

**`./sandbox`** based on `appbase sandbox-template`:
- `--profile claude-code`
- `--allow $PROJECT_DIR` (read-write)
- `--allow $HOME/.config/calendar-sync` (app data — SQLite when running locally)
- `--allow $HOME/go` (Go module cache)
- `--allow-bind <PORT>` (dev server, from appconfig port allocation)
- `--read $GO_BIN_DIR` (Go toolchain)
- Graceful degradation if nono not installed
- Pre-sandbox GOROOT resolution and PATH setup

**Deploy gate** in `./dev`:
```sh
dev_deploy() {
    if [ -n "$NONO_CAP_FILE" ]; then
        echo "Error: deploy cannot run inside a nono sandbox."
        echo "Run ./dev deploy directly, outside the sandbox."
        return 1
    fi
    ...
}
```

### 3. Auth and OAuth

appbase handles all OAuth plumbing. We configure it via `app.yaml`:

```yaml
auth:
  client_id: ${secret:google-client-id}
  client_secret: ${secret:google-client-secret}
  extra_scopes:
    - https://www.googleapis.com/auth/calendar.events
```

Routes provided automatically by appbase:
- `GET /api/auth/login` — redirects to Google OAuth
- `GET /api/auth/callback` — handles OAuth callback
- `GET /api/auth/status` — returns login status
- `POST /api/auth/logout` — clears session

### 4. Frontend (M1 minimal)

For M1, the frontend is a simple HTML page served by Go templates:

- **Unauthenticated**: appbase's built-in login page (via `app.LoginPage()`)
- **Authenticated**: A minimal page showing the user's email and a logout button. This page will grow in M2+ to show calendar config and sync controls.

No JavaScript needed for M1. Server-rendered HTML only.

### 5. appconfig and secrets

```bash
appconfig init
appconfig ports allocate 1          # One port for the web server
appconfig set google-client-id=...
appconfig set google-client-secret=...
appconfig env                       # Generates .env
```

### 6. E2E tests

E2e test scripts in `e2e/` directory, run via `./dev e2e`:

| Test | Use Case | What it validates |
|------|----------|-------------------|
| `e2e/01-health.sh` | UC-0001 | App starts, `GET /health` returns 200 |
| `e2e/02-auth-status-unauthed.sh` | UC-0002, UC-0004 | `GET /api/auth/status` returns `loggedIn: false` without a session |

UC-0003 (full OAuth login) and UC-0005 (logout) require a browser flow and are validated manually for M1. We can add a test-mode bypass in a later milestone if needed.

## Build order

1. `go.mod`, `.gitignore`, `app.yaml`, `app.json`
2. `dev` script and `sandbox` script
3. `main.go` with setup, serve, and minimal authenticated page
4. `e2e/` test scripts
5. Run `./dev ci` to validate
6. Update `CLAUDE.md` with sandbox section
7. Architecture review, then commit

## Dependencies

- `appbase` CLI installed (`go install github.com/michaelwinser/appbase/cmd/appbase@latest`)
- Google OAuth client ID and secret (existing or new GCP project)
- `nono` installed (graceful degradation if absent)
- `appconfig` for secrets/port management
