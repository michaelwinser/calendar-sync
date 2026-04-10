---
name: migrate-appbase
description: Migrate a consumer app to latest appbase patterns
trigger: When the user wants to update their app to use current appbase patterns, after running 'appbase update'
---

# Migrating to Latest appbase Patterns

Run `appbase update --dry-run` first to see what's outdated. Then use this skill to make the code-level changes that require human review.

## Pre-check

1. Run `appbase update --dry-run` and note the suggestions
2. Ensure the appbase dependency is bumped: `go get github.com/michaelwinser/appbase@latest`
3. Read the project's current code before making changes

## Migration Checklist

Work through each applicable item. Skip items that don't apply. Show the user what you're changing and why.

### 1. CLI auth pattern

**Old (deprecated):**
```go
serverURL, cleanup, _ := appcli.ResolveServerWithAutoServe(cmd, appName)
defer cleanup()
httpClient, err := appcli.AuthenticatedClient(appName)
if err != nil { httpClient = http.DefaultClient }
client, _ := api.NewClientWithResponses(serverURL, api.WithHTTPClient(httpClient))
```

**New:**
```go
httpClient, baseURL, cleanup, err := appcli.ClientForCommand(cmd, "myapp", app.Handler())
if err != nil { return err }
defer cleanup()
client, _ := api.NewClientWithResponses(baseURL, api.WithHTTPClient(httpClient))
```

`ClientForCommand` handles local vs remote mode automatically based on `IsLocalMode`.

### 2. DevAuth → LocalMode

**Old:** `AUTH_MODE=dev` environment variable + `DevAuthMiddleware`

**New:** `appbase.Config{LocalMode: appcli.IsLocalMode}` in setup(). No env var needed. Identity injected at the transport layer, not via middleware.

### 3. Test auth

**Old:**
```go
session, _ := testSessions.Create("test@example.com", "test@example.com", 1*time.Hour)
c.SetCookie(auth.CookieName, session.ID)
```

**New:**
```go
// In setup:
os.Setenv("APPBASE_TEST_MODE", "true")

// In each test:
c.SetHeader("X-Test-User", "test@example.com")
```

Remove any `testSessions` variable and `auth` import used only for test cookies.

### 4. Manual SQL/Firestore backends → store.Collection

**Old:** Separate `store.go`, `store_sql.go`, `store_firestore.go` with manual `activityBackend` interface, hand-written SQL queries, and Firestore document mapping.

**New:**
```go
type ActivityEntity struct {
    ID        string `store:"id,pk"`
    UserID    string `store:"user_id,index"`
    Title     string `store:"title"`
    CreatedAt string `store:"created_at"`
}

coll, _ := store.NewCollection[ActivityEntity](db, "activities")
```

**When to migrate:** Only when the entity's queries are simple (Where, OrderBy, Limit). Keep manual backends for entities with complex queries (joins, aggregations, custom Firestore indexes).

### 5. ./dev script

**Old:**
```sh
. "../appbase/deploy/dev-template.sh"
```

**New:**
```sh
eval "$(appbase dev-template)"
```

### 6. CLAUDE.md updates

Replace devcontainer-only frontend rules with mise-aware guidance:

**Old:**
```markdown
**Do not install Node.js, npm, pnpm, or frontend build tools on the host.**
All frontend tooling runs inside the project's devcontainer.
```

**New:**
```markdown
## Toolchain

Run `mise install` to set up Go, Node, pnpm, and codegen tools.
Alternatively, use the devcontainer in `.devcontainer/`.

Frontend commands can run directly after `mise install`:
- `pnpm install` — install dependencies
- `pnpm build` — build frontend
- `./dev codegen` — generate Go + TypeScript types
```

### 7. Add .mise.toml

If missing, create:

```toml
[tools]
go = "1.25"
node = "22"
"npm:pnpm" = "9"
```

Adjust versions to match the project's current requirements.

### 8. Add ./sandbox

If missing, generate and customize:

```bash
appbase sandbox-template > sandbox
chmod +x sandbox
```

Then edit to add project-specific capabilities:
- `--allow $HOME/.config/<appname>` for data directory
- `--allow-bind <port>` for dev server port

### 9. Add extra_scopes to app.yaml

If the app uses Google APIs (Calendar, Tasks, Drive), move scope configuration from code to app.yaml:

**Old (in code):**
```go
cfg := appbase.Config{
    GoogleAuth: &auth.GoogleAuthConfig{
        ExtraScopes: []string{"https://www.googleapis.com/auth/calendar"},
    },
}
```

**New (in app.yaml):**
```yaml
auth:
  client_id: ${secret:google-client-id}
  client_secret: ${secret:google-client-secret}
  extra_scopes:
    - https://www.googleapis.com/auth/calendar
```

Remove the `GoogleAuth` field from the Config struct in code.

### 10. Verify

After all changes:

```bash
go build ./...
go test -v ./...
./dev lint-api        # if using OpenAPI
./dev serve           # smoke test the web UI
myapp --local list    # smoke test CLI local mode
```

## What NOT to change

- **Don't touch generated files** (`api/server.gen.go`, `api/client.gen.go`) — run `./dev codegen` instead
- **Don't change the OpenAPI spec** unless the migration requires it
- **Don't modify handler logic** — this migration is about infrastructure patterns, not business logic
- **Don't remove devcontainer config** — it still works alongside mise for contributors who prefer it
- **Don't force-migrate store.Collection** on entities with complex queries — manual backends are valid for advanced use cases
