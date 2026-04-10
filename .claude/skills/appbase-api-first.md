---
name: api-first
description: Add or modify API endpoints using the OpenAPI-first pattern
trigger: When the user wants to add an API endpoint, modify an API, add a new route, or work on the OpenAPI spec
---

# API-First Development with OpenAPI

The OpenAPI spec (`openapi.yaml`) is the single source of truth for the server API. All routes, types, and clients are generated from it. Never hand-write route registrations or request/response types.

## Adding a New Endpoint

### 1. Define it in openapi.yaml first

```yaml
paths:
  /api/things:
    get:
      operationId: listThings
      security: [session: []]
      responses:
        '200':
          content:
            application/json:
              schema:
                type: array
                items: { $ref: '#/components/schemas/Thing' }
```

**Rules:**
- Every API endpoint MUST be defined in `openapi.yaml`
- Use `operationId` on every operation (it becomes the Go method name)
- Add `security: [session: []]` for authenticated endpoints
- Define request/response types in `components/schemas`
- Use `$ref` to reference shared types

### 2. Regenerate code

```bash
./ab codegen    # or ./dev codegen
```

This regenerates:
- `api/server.gen.go` — updated `ServerInterface` with the new method
- `api/client.gen.go` — updated Go client with the new method

### 3. Implement the server method

The Go compiler will error until you implement the new method on your server struct:

```go
// server.go
func (s *MyServer) ListThings(w http.ResponseWriter, r *http.Request) {
    userID := appbase.UserID(r)
    items, err := s.store.List(userID)
    // ...
    server.RespondJSON(w, http.StatusOK, apiItems)
}
```

### 4. Add the store method if needed

If the endpoint needs a new store operation, add it to `store.go`.

### 5. Add a CLI command using the generated client

```go
listCmd := &cobra.Command{
    Use:   "list",
    Short: "List things (via API)",
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
        // ...
    },
}
```

**Three modes — no code changes needed:**
- `myapp list` — local: in-process transport, no server needed
- `myapp --server http://localhost:3000 list` — uses running server
- `myapp --server https://prod.app list` — remote, needs `myapp login` first
- `myapp --local list` — force local mode (ignores saved server URL)

### 6. Write a use case test

Use `APPBASE_TEST_MODE=true` and `X-Test-User` header — no session setup needed:

```go
h.Run("UC-XXXX", "List things", func(c *harness.Client) {
    c.SetHeader("X-Test-User", "test@example.com")
    resp := c.GET("/api/things")
    c.AssertStatus(resp, 200)
})
```

### 7. Run the linter

```bash
./ab lint-api
```

This verifies:
- Generated code is up to date with the spec
- No hand-written `/api/` route registrations
- Server struct implements the full interface

## Modifying an Existing Endpoint

1. Change the spec in `openapi.yaml`
2. Run `./ab codegen`
3. Fix any compiler errors (changed method signatures)
4. Update tests
5. Run `./ab lint-api`

## Common Patterns

### Path parameters

```yaml
/api/things/{id}:
  get:
    operationId: getThing
    parameters:
      - name: id
        in: path
        required: true
        schema: { type: string }
```

Generated method: `GetThing(w http.ResponseWriter, r *http.Request, id string)`

### Request body

```yaml
post:
  operationId: createThing
  requestBody:
    required: true
    content:
      application/json:
        schema: { $ref: '#/components/schemas/CreateThingRequest' }
```

Parse in the handler: `json.NewDecoder(r.Body).Decode(&req)`

### Converting between store and API types

Store entities use `store:` tags, API types are generated from the spec. Convert between them in the server methods:

```go
func entityToAPI(e store.Entity) api.Entity {
    return api.Entity{Id: e.ID, Title: e.Title, ...}
}
```

This separation is intentional — the API contract and storage schema evolve independently.

## Anti-Patterns

- **Don't hand-write routes** — use `api.HandlerFromMux(server, router)`. If you see `r.Get("/api/..."` in your code, it should be in the spec instead.
- **Don't define request/response types manually** — they come from the spec via codegen.
- **Don't skip the CLI client** — CLI commands should use the generated HTTP client, not call the store directly. This ensures the CLI tests the same API path as the web UI.
- **Don't forget `operationId`** — without it, generated method names are ugly.
- **Don't edit generated files** — they'll be overwritten. Put customizations in your server implementation.

## Files

| File | Role | Edit? |
|------|------|-------|
| `openapi.yaml` | API contract (source of truth) | Yes |
| `oapi-codegen.yaml` | Server codegen config | Rarely |
| `oapi-codegen-client.yaml` | Client codegen config | Rarely |
| `api/server.gen.go` | Generated server interface + types | Never |
| `api/client.gen.go` | Generated Go HTTP client | Never |
| `internal/app/server.go` | Implements `api.ServerInterface` | Yes |
| `internal/app/store.go` | Entity persistence | Yes |
| `store.go` | Re-exports from internal/app | Rarely |
| `main.go` | Server + CLI entry point | Yes |
| `cmd/desktop/main.go` | Wails desktop entry point (optional) | Yes |

## Project structure

Shared code lives in `internal/app/` so both the server binary and the
desktop binary can import it:

```
myapp/
├── internal/app/         # Shared store + server (exported fields)
│   ├── store.go
│   └── server.go
├── main.go               # Server + CLI
├── store.go              # Re-exports from internal/app
├── cmd/desktop/          # Wails desktop (optional)
│   └── main.go
├── api/                  # Generated
├── frontend/             # Svelte
└── openapi.yaml          # Source of truth
```

## Reference Example

See `examples/todo-api/` for a complete working example including
server, CLI with auto-serve, Svelte frontend, and Wails desktop mode.
