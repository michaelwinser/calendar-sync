---
name: scaffold-app
description: Create a new application that uses appbase
trigger: When the user wants to create a new app built on the appbase module
---

# Scaffolding a New App on appbase

## What You'll Create

```
myapp/
├── CLAUDE.md              # AI session instructions
├── .mise.toml             # Toolchain (Go, Node, pnpm)
├── app.yaml               # App config (name, port, environments, secrets)
├── app.json               # Deploy script compat (name, gcpProject, region)
├── go.mod                 # Depends on github.com/michaelwinser/appbase
├── openapi.yaml           # API spec (source of truth)
├── main.go                # Server + CLI entry point
├── store.go               # Re-exports from internal/app
├── internal/app/          # Shared code (imported by both server and desktop)
│   ├── store.go           # Entity store (store.Collection)
│   └── server.go          # Implements api.ServerInterface
├── api/                   # Generated from openapi.yaml
│   ├── server.gen.go
│   └── client.gen.go
├── frontend/              # Svelte app
│   ├── package.json
│   ├── src/
│   │   ├── App.svelte
│   │   └── lib/
│   │       ├── api.ts           # Typed fetch wrappers
│   │       └── api-types.ts     # Generated from openapi.yaml
│   └── dist/              # Embedded in Go binary
├── cmd/desktop/           # Wails desktop entry point (optional)
├── usecases_test.go       # Use case tests (UC-XXXX)
├── dev                    # Project command script
├── sandbox                # nono sandbox for AI sessions
└── .claude/
    └── settings.local.json
```

## Steps

### 1. Initialize the Module

```bash
mkdir myapp && cd myapp
go mod init github.com/michaelwinser/myapp
go get github.com/michaelwinser/appbase@latest
```

### 2. Create .mise.toml

```toml
[tools]
node = "22"
"npm:pnpm" = "9"
```

Then `mise install` to get the toolchain. Go and codegen tools inherit from the parent appbase `.mise.toml` if this is a sibling project, or add them explicitly:

```toml
[tools]
go = "1.25"
node = "22"
"npm:pnpm" = "9"
"go:github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen" = "latest"
```

### 3. Create CLAUDE.md

```markdown
# myapp

An appbase application. See the appbase README for framework reference.

## Development

All tasks go through `./dev`:
- `./dev serve` — Start web server
- `./dev build` — Build binary
- `./dev test` — Run tests
- `./dev codegen` — Generate server/client + frontend types from openapi.yaml

## Frontend

Uses mise for toolchain (`mise install`). Run `pnpm` commands via `./dev frontend <cmd>` or directly after `mise install`.

## Architecture

- **Config:** `appbase.Config{LocalMode: appcli.IsLocalMode}` for CLI
- **CLI commands:** Use `appcli.ClientForCommand(cmd, "myapp", app.Handler())`
- **Auth in handlers:** `appbase.UserID(r)`, `appbase.Email(r)`
- **Testing:** Set `APPBASE_TEST_MODE=true`, use `X-Test-User` header
- **Desktop:** Use `app.LocalHandler()` with Wails

## appbase Reference

For appbase API documentation, read these files (do NOT explore appbase source code):
- `docs/api-reference.md` in the appbase repo — complete API surface
- `docs/app-yaml-reference.md` in the appbase repo — app.yaml configuration
- Run `appbase help` for available CLI commands
- Use `/api-first`, `/add-entity`, `/scaffold-app` skills for guided workflows
```

### 4. Create app.yaml

```yaml
name: myapp
port: 3000

store:
  type: sqlite
  path: data/app.db

# Uncomment to enable app-specific GCP APIs during provisioning:
# gcp:
#   apis:
#     - tasks.googleapis.com

environments:
  local:
    url: http://localhost:3000
    auth:
      client_id: ${secret:google-client-id}
      client_secret: ${secret:google-client-secret}
      # extra_scopes:
      #   - https://www.googleapis.com/auth/tasks

  production:
    url: https://myapp.run.app
    store:
      type: firestore
      gcp_project: my-gcp-project
    auth:
      client_id: ${secret:google-client-id}
      client_secret: ${secret:google-client-secret}
```

### 5. Create app.json

```json
{
  "name": "myapp",
  "gcpProject": "",
  "region": "us-central1"
}
```

### 6. Create main.go

```go
package main

import (
    "context"
    "embed"
    "fmt"
    "io/fs"
    "net/http"

    "github.com/spf13/cobra"

    "github.com/michaelwinser/appbase"
    appcli "github.com/michaelwinser/appbase/cli"
    "github.com/michaelwinser/myapp/api"
)

//go:embed frontend/dist/*
var frontendDist embed.FS

var (
    app       *appbase.App
    thingStore *ThingStore
)

func setup() error {
    var err error
    cfg := appbase.Config{
        Name:      "myapp",
        Quiet:     !appcli.IsServeCommand,
        LocalMode: appcli.IsLocalMode,
    }
    if appcli.LocalDataPath != "" {
        cfg.DB.SQLitePath = appcli.LocalDataPath + "/app.db"
    }
    app, err = appbase.New(cfg)
    if err != nil {
        return err
    }
    thingStore, err = NewThingStore(app.DB())
    if err != nil {
        return err
    }

    thingServer := &ThingServer{Store: thingStore}
    api.HandlerFromMux(thingServer, app.Server().Router())
    return nil
}

func main() {
    cliApp := appcli.New("myapp", "My application", setup)

    cliApp.SetServeFunc(func() error {
        r := app.Server().Router()
        distFS, _ := fs.Sub(frontendDist, "frontend/dist")
        fileServer := http.FileServer(http.FS(distFS))
        r.Handle("/assets/*", fileServer)
        r.Get("/*", app.LoginPage(func(w http.ResponseWriter, r *http.Request) {
            r.URL.Path = "/"
            fileServer.ServeHTTP(w, r)
        }))
        return app.Serve()
    })

    // CLI commands use the generated HTTP client
    listCmd := &cobra.Command{
        Use:   "list",
        Short: "List things",
        RunE: func(cmd *cobra.Command, args []string) error {
            if err := setup(); err != nil {
                return err
            }
            httpClient, baseURL, cleanup, err := appcli.ClientForCommand(cmd, "myapp", app.Handler())
            if err != nil {
                return err
            }
            defer cleanup()
            client, _ := api.NewClientWithResponses(baseURL, api.WithHTTPClient(httpClient))
            resp, err := client.ListThingsWithResponse(context.Background())
            if err != nil {
                return err
            }
            for _, t := range *resp.JSON200 {
                fmt.Println(t.Name)
            }
            return nil
        },
    }
    cliApp.AddCommand(listCmd)

    cliApp.Execute()
}
```

### 7. Create the store

Use `store.Collection[T]` for automatic SQL/Firestore dual-backend:

```go
// internal/app/store.go
type ThingEntity struct {
    ID        string `json:"id"        store:"id,pk"`
    UserID    string `json:"userId"    store:"user_id,index"`
    Name      string `json:"name"      store:"name"`
    CreatedAt string `json:"createdAt" store:"created_at"`
}
```

See `examples/todo-api/internal/app/store.go` for the complete pattern.

### 8. Create the `./dev` script

```sh
#!/bin/sh
set -e
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

eval "$(appbase dev-template)"

APP_BINARY_NAME="myapp"

case "${1:-help}" in
    *)  dev_dispatch "$@" ;;
esac
```

Make executable: `chmod +x dev`

### 9. Create the `./sandbox` script

```sh
appbase sandbox-template > sandbox
chmod +x sandbox
# Edit: add --allow and --allow-bind for your project
```

### 10. Create the frontend

```bash
pnpm create vite frontend --template svelte-ts
cd frontend && pnpm install
```

Add the API client pattern — see `examples/todo-api/frontend/src/lib/api.ts`.
Run `./dev codegen` to generate TypeScript types from `openapi.yaml`.

### 11. Create use case tests

```go
func setupTestApp(t *testing.T) http.Handler {
    os.Setenv("STORE_TYPE", "sqlite")
    os.Setenv("SQLITE_DB_PATH", ":memory:")
    os.Setenv("APPBASE_TEST_MODE", "true")

    a, _ := appbase.New(appbase.Config{Name: "myapp", Quiet: true})
    t.Cleanup(func() { a.Close() })

    s, _ := NewThingStore(a.DB())
    server := &ThingServer{Store: s}
    api.HandlerFromMux(server, a.Server().Router())
    return a.Server().Router()
}

func TestUseCases(t *testing.T) {
    h := harness.New(t, setupTestApp)

    h.Run("UC-001", "List returns empty", func(c *harness.Client) {
        c.SetHeader("X-Test-User", "test@example.com")
        resp := c.GET("/api/things")
        c.AssertStatus(resp, 200)
        c.AssertJSONArray(resp, 0)
    })
}
```

### 12. Configure Claude Code permissions

Create `.claude/settings.local.json`:

```json
{
  "permissions": {
    "allow": [
      "Bash(go:*)", "Bash(git:*)", "Bash(sh:*)",
      "Bash(appbase:*)", "Bash(docker:*)",
      "Bash(gh:*)", "Bash(./dev:*)", "Bash(pnpm:*)"
    ]
  }
}
```

### 13. Verify

```bash
go build ./...
go test -v ./...
./dev serve              # Server at localhost:3000
myapp list               # CLI local mode
myapp --local list       # Force local mode
./dev codegen            # Go + frontend types
```

## Choosing a Pattern

| Pattern | When to use | Example |
|---------|------------|---------|
| **API-first (recommended)** | Apps with a web UI, CLI client, or external consumers | `examples/todo-api/` |
| **Hand-written routes** | Simple apps, internal tools, prototypes | `examples/todo-store/` |
| **Desktop (Wails)** | Native desktop app, same API | `examples/todo-api/DESKTOP.md` |

## Key Patterns

- **Three runtime modes** — local CLI (in-process), web server (OAuth), remote CLI (keychain session)
- **In-process transport** — CLI commands call the handler directly, no TCP
- **Login page is built-in** — `app.LoginPage(handler)` shows Google sign-in
- **CLI uses the API** — generated HTTP client via `ClientForCommand`
- **Desktop via LocalHandler()** — `app.LocalHandler()` with Wails
- **Frontend types from spec** — `./dev codegen` generates Go + TypeScript
- **Test auth** — `APPBASE_TEST_MODE=true` + `X-Test-User` header
- **`--local` flag** — force local mode, ignore saved server URLs
