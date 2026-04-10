---
name: appbase-api-reference
description: "appbase API reference — read this FIRST instead of exploring appbase source code. Covers all exported types, functions, methods, and constants."
trigger: When you need to understand appbase types, functions, or methods. Read this before exploring source code.
---

# appbase API Reference

Quick reference for the public API. For concepts and patterns, see the README.

## appbase (root package)

```go
import "github.com/michaelwinser/appbase"
```

### App

```go
app, err := appbase.New(appbase.Config{
    Name:      "myapp",
    Quiet:     !appcli.IsServeCommand,
    LocalMode: appcli.IsLocalMode,
    // Optional:
    GoogleAuth:     &auth.GoogleAuthConfig{...},
    TokenAuth:      &auth.TokenAuthConfig{...},
    AllowedOrigins: []string{"http://localhost:3000"},
    Port:           "3000",
    DB:             db.DBConfig{StoreType: "sqlite", SQLitePath: "data/app.db"},
})

app.DB()            // *db.DB — database connection
app.Sessions()      // *auth.SessionStore
app.Google()        // *auth.GoogleAuth (nil if not configured)
app.Server()        // *server.Server
app.Router()        // chi.Router — register routes
app.Handler()       // http.Handler — for CLI transport
app.LocalHandler()  // http.Handler — for Wails desktop (injects identity)
app.Serve()         // Start HTTP server (blocks)
app.Close()         // Clean up resources
app.Migrate(sql)    // Run SQL migration
app.LoginPage(next) // Show login page when unauthenticated, next when authenticated
```

### Request helpers

```go
appbase.UserID(r)      // string — authenticated user's ID
appbase.Email(r)       // string — authenticated user's email
appbase.AccessToken(r) // string — OAuth access token (empty in local mode)
```

## auth

```go
import "github.com/michaelwinser/appbase/auth"
```

### Middleware

```go
// Auth middleware — registered automatically by appbase.New()
auth.Middleware(sessions, exemptPrefixes) // func(http.Handler) http.Handler

// When APPBASE_TEST_MODE=true, accepts X-Test-User header
auth.TestMode() // bool
```

### Context accessors

```go
auth.UserID(r)       // string
auth.Email(r)        // string
auth.AccessToken(r)  // string
auth.RefreshToken(r) // string
auth.TokenExpiry(r)  // time.Time
auth.WithIdentity(ctx, userID, email) // context.Context — for transports
```

### Session

```go
type Session struct {
    ID, UserID, Email string
    ExpiresAt, CreatedAt time.Time
    AccessToken, RefreshToken string
    TokenExpiry time.Time
}

session.IsExpired()    // bool
session.TokenExpired() // bool

store, _ := auth.NewSessionStore(db)
session, _ := store.Create(userID, email, 30*24*time.Hour)
session, _ := store.Get(id)        // nil if not found
store.UpdateTokens(id, access, refresh, expiry)
store.Delete(id)
store.DeleteExpired()
store.DeleteByUser(userID)
```

### Google OAuth

```go
type GoogleAuthConfig struct {
    ClientID, ClientSecret, RedirectURL string
    ExtraScopes  []string  // e.g., "https://www.googleapis.com/auth/tasks"
    AllowedUsers []string  // empty = allow all
}

google := auth.NewGoogleAuth(sessions, config) // nil if not configured
google.IsConfigured()                          // bool
google.LoginURL(w, r)                          // string — sets state cookie
google.HandleCallback(r, code)                 // *LoginResult, error
google.RefreshAccessToken(ctx, session)        // string, error
google.SetSessionCookie(w, r, sessionID)
auth.ClearSessionCookie(w)
```

### Token auth

```go
type TokenAuthConfig struct {
    Tokens map[string]string // token -> email
}

ta := auth.NewTokenAuth(sessions, config) // nil if not configured
ta.IsConfigured()                         // bool
ta.HandleLogin(token)                     // *LoginResult, error
```

Endpoint: `POST /api/auth/token-login` with `{"token": "..."}` or form-encoded.

### LoginResult

```go
type LoginResult struct {
    Session      *Session
    Email        string
    AccessToken  string  // OAuth token (empty for token auth)
    RefreshToken string
    ExpiresAt    time.Time
    Scopes       string
}
```

## cli

```go
import appcli "github.com/michaelwinser/appbase/cli"
```

### Setup

```go
cli := appcli.New("myapp", "description", setupFn)
cli.SetServeFunc(func() error { return app.Serve() })
cli.AddCommand(cmd)
cli.Execute()
```

### Mode detection (set by PersistentPreRun)

```go
appcli.IsServeCommand // bool — true for "serve" command
appcli.IsLocalMode    // bool — true when no --server flag
appcli.LocalDataPath  // string — e.g., ~/.config/myapp
```

### HTTP client for CLI commands

```go
httpClient, baseURL, cleanup, err := appcli.ClientForCommand(cmd, "myapp", app.Handler())
defer cleanup()
// Local mode: in-process transport (no TCP)
// Remote mode: real HTTP with keychain session
```

### Flags (automatic)

```
--server URL   Remote server URL
--local        Force local mode
--data PATH    Custom data directory
```

### Auth helpers

```go
appcli.LocalUserID()                    // "dev@localhost" or DEV_USER_EMAIL
appcli.LoginBrowser(serverURL, appName) // Browser OAuth login
appcli.Logout(appName)                  // Clear session
appcli.Whoami(serverURL, appName)       // Show current user
```

## store

```go
import "github.com/michaelwinser/appbase/store"
```

### Entity definition

```go
type Todo struct {
    ID        string `store:"id,pk"`       // primary key (required)
    UserID    string `store:"user_id,index"` // indexed column
    Title     string `store:"title"`
    Done      bool   `store:"done"`
    CreatedAt string `store:"created_at"`
}
```

**Store tags:** `store:"column_name"` with options:
- `pk` — primary key (exactly one required)
- `index` — create SQL index

**Supported types:** `string`, `bool`, `int`, `int64`, `float64`

### Collection

```go
coll, _ := store.NewCollection[Todo](db, "todos")

coll.Get(id)                   // *Todo, error (nil if not found)
coll.Create(&todo)             // error
coll.Update(id, &todo)         // error
coll.Delete(id)                // error
coll.All()                     // []Todo, error

// Auto-creates table and indexes. Auto-migrates new columns.
```

### Query builder

```go
coll.Where("user_id", "==", uid).All()                           // []Todo, error
coll.Where("user_id", "==", uid).OrderBy("created_at", store.Desc).All()
coll.Where("user_id", "==", uid).Where("done", "==", false).All()
coll.Where("user_id", "==", uid).Limit(10).All()
coll.Where("user_id", "==", uid).First()                        // *Todo, error
```

**Operators:** `==`, `!=`, `<`, `>`, `<=`, `>=`

### Read-only

```go
ro := coll.ReadOnly() // *ReadOnlyCollection[Todo]
ro.Get(id)            // works
ro.Where(...).All()   // works
// ro.Create(...)     // does not compile
// ro.Delete(...)     // does not compile
```

## server

```go
import "github.com/michaelwinser/appbase/server"
```

```go
srv := server.New(server.Config{
    Port:           "3000",
    AllowedOrigins: []string{"http://localhost:3000"},
    Quiet:          true,
})

srv.Router() // chi.Router
srv.Serve()  // Start with graceful shutdown
srv.Port()   // string

// Response helpers (use in handlers)
server.RespondJSON(w, http.StatusOK, data)
server.RespondError(w, http.StatusBadRequest, "message")
```

## db

```go
import "github.com/michaelwinser/appbase/db"
```

```go
database, _ := db.New(db.DBConfig{
    StoreType:  "sqlite",     // or "firestore"
    SQLitePath: "data/app.db",
    GCPProject: "my-project", // required for firestore
})

database.IsSQL()        // bool
database.SQL()          // *sql.DB (nil for Firestore)
database.Firestore()    // *firestore.Client (nil for SQL)
database.Exec(sql, args...)
database.Query(sql, args...)
database.QueryRow(sql, args...)
database.Begin()        // *sql.Tx
database.Migrate(sql)   // Run DDL (no-op for Firestore)
database.Preflight()    // Verify backend works
database.Close()
```

## config

```go
import appconfig "github.com/michaelwinser/appbase/config"
```

```go
appCfg, _ := appconfig.LoadAppConfig("app.yaml", secrets)
appCfg.Env()        // "local" or from APP_ENV
appCfg.URL()        // URL for active environment
appCfg.GCPProject() // string
```

### Secret resolvers

```go
// Chain: keychain → Docker secrets → .env → GCP Secret Manager
resolver := appconfig.DefaultResolver(gcpProject)

// Individual resolvers
k := &appconfig.KeychainResolver{}
k.Get(project, name)
k.Set(project, name, value)
```

## testing

```go
import harness "github.com/michaelwinser/appbase/testing"
```

```go
h := harness.New(t, setupFn)

h.Run("UC-001", "Create a todo", func(c *harness.Client) {
    c.SetHeader("X-Test-User", "test@example.com")
    resp := c.POST("/api/todos", `{"title":"Test"}`)
    c.AssertStatus(resp, 201)
    c.AssertJSONHas(resp, "title", "Test")
    c.AssertJSONArray(resp, 1)

    data := resp.JSON()       // map[string]interface{}
    items := resp.JSONArray()  // []map[string]interface{}
})

// Or with real sessions via token auth:
resp := c.POST("/api/auth/token-login", `{"token":"test-token"}`)
// Cookie auto-saved, subsequent requests authenticated
```

## Built-in HTTP endpoints

These are registered automatically by `appbase.New()`:

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/health` | Health check (`{"status":"ok"}`) |
| GET | `/api/auth/status` | Auth status (`{loggedIn, email}`) |
| GET | `/api/auth/login` | Google OAuth URL (JSON or redirect) |
| GET | `/api/auth/callback` | OAuth callback |
| POST | `/api/auth/token-login` | Token auth login |
| POST | `/api/auth/logout` | Clear session |
| POST | `/api/auth/cli-login` | CLI login initiation |
| GET | `/api/auth/cli-poll` | CLI login polling |

## Extending ./dev for custom provisioning and deploy

appbase handles infrastructure provisioning (Cloud Run, Firestore, secrets) and standard deploy. App-specific steps (Cloud Scheduler, Pub/Sub, custom DNS) are added by overriding commands in your `./dev` script:

```sh
case "${1:-help}" in
    deploy)
        _load_secrets
        appbase deploy              # standard Cloud Run deploy
        setup_scheduler             # your post-deploy step
        ;;
    provision)
        appbase provision "$2"      # standard GCP setup
        enable_extra_apis           # your extra APIs
        ;;
    *)  dev_dispatch "$@" ;;
esac
```

**What goes in appbase vs your ./dev:**

| Concern | Where | How |
|---------|-------|-----|
| Cloud Run, Firestore, secrets | `appbase deploy` | Automatic |
| App-specific GCP APIs | `app.yaml` `gcp.apis` | Declared, provisioned automatically |
| OAuth scopes | `app.yaml` `auth.extra_scopes` | Declared, requested during login |
| Cloud Scheduler, Pub/Sub, etc. | Your `./dev` script | Override `deploy` case |
| Custom provisioning | Your `./dev` script | Override `provision` case |

The `_load_secrets` helper (from dev-template) loads keychain secrets into env vars before deploy.

## Environment variables

| Variable | Purpose | Default |
|----------|---------|---------|
| `PORT` | HTTP server port | `3000` |
| `STORE_TYPE` | `sqlite` or `firestore` | `sqlite` |
| `SQLITE_DB_PATH` | SQLite file path | `data/app.db` |
| `GOOGLE_CLIENT_ID` | OAuth client ID | from app.yaml |
| `GOOGLE_CLIENT_SECRET` | OAuth client secret | from app.yaml |
| `GOOGLE_REDIRECT_URL` | OAuth callback URL | auto-detected |
| `ALLOWED_USERS` | Email allowlist (comma-separated) | allow all |
| `AUTH_TOKENS` | Static tokens (`token1=email1,...`) | from app.yaml |
| `APP_ENV` | Active environment | `local` |
| `DEV_USER_EMAIL` | Local mode identity | `dev@localhost` |
| `APPBASE_TEST_MODE` | Enable X-Test-User header | `false` |
