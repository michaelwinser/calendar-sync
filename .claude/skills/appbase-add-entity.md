---
name: add-entity
description: Add a new domain entity to an app built on appbase
trigger: When the user wants to add a new data type/entity to their application
---

# Adding a New Entity

When the app needs a new data type (e.g., Activity, Location, Contact), follow this checklist. The `store.Collection[T]` generic handles both SQLite and Firestore automatically — you only write the struct.

## Checklist

### 1. Define the entity struct

```go
// internal/app/store.go (or a new file for the entity)
type ActivityEntity struct {
    ID        string `json:"id"        store:"id,pk"`
    UserID    string `json:"userId"    store:"user_id,index"`
    Title     string `json:"title"     store:"title"`
    Status    string `json:"status"    store:"status"`
    StartDate string `json:"startDate" store:"start_date"`
    CreatedAt string `json:"createdAt" store:"created_at"`
}
```

**Rules for `store:` tags:**
- `pk` — primary key (exactly one required)
- `index` — creates a SQL index on this column
- Column names must be `snake_case` alphanumeric
- Supported Go types: `string`, `int`, `int64`, `bool`, `float64`
- Dates stored as `string` in RFC3339 format

### 2. Create the store

```go
type ActivityStore struct {
    Coll *store.Collection[ActivityEntity]
}

func NewActivityStore(d *db.DB) (*ActivityStore, error) {
    coll, err := store.NewCollection[ActivityEntity](d, "activities")
    if err != nil {
        return nil, err
    }
    return &ActivityStore{Coll: coll}, nil
}

func (s *ActivityStore) List(userID string) ([]ActivityEntity, error) {
    return s.Coll.Where("user_id", "==", userID).OrderBy("created_at", store.Desc).All()
}

func (s *ActivityStore) Create(userID, title string) (*ActivityEntity, error) {
    a := &ActivityEntity{
        ID:        uuid.New().String(),
        UserID:    userID,
        Title:     title,
        Status:    "active",
        CreatedAt: time.Now().Format(time.RFC3339),
    }
    if err := s.Coll.Create(a); err != nil {
        return nil, err
    }
    return a, nil
}

func (s *ActivityStore) Get(id string) (*ActivityEntity, error) {
    return s.Coll.Get(id)
}

func (s *ActivityStore) Update(id string, entity *ActivityEntity) error {
    return s.Coll.Update(id, entity)
}

func (s *ActivityStore) Delete(id string) error {
    return s.Coll.Delete(id)
}
```

**What `store.Collection` does for you:**
- Creates the SQL table with correct types on first use
- Auto-migrates missing columns when you add fields to the struct
- Creates indexes for fields tagged with `,index`
- Works on both SQLite and Firestore (Firestore is schemaless — no migration needed)

### 3. Add the API spec

Add endpoints to `openapi.yaml`:

```yaml
/api/activities:
  get:
    operationId: listActivities
    security: [session: []]
    responses:
      '200':
        content:
          application/json:
            schema:
              type: array
              items: { $ref: '#/components/schemas/Activity' }
  post:
    operationId: createActivity
    security: [session: []]
    requestBody:
      required: true
      content:
        application/json:
          schema: { $ref: '#/components/schemas/CreateActivityRequest' }
    responses:
      '201':
        content:
          application/json:
            schema: { $ref: '#/components/schemas/Activity' }
```

### 4. Regenerate code

```bash
./dev codegen
```

### 5. Implement the server methods

The compiler will error until you implement the new methods:

```go
func (s *MyServer) ListActivities(w http.ResponseWriter, r *http.Request) {
    userID := appbase.UserID(r)
    items, err := s.activities.List(userID)
    if err != nil {
        server.RespondError(w, http.StatusInternalServerError, err.Error())
        return
    }
    // Convert store entities to API types
    server.RespondJSON(w, http.StatusOK, toAPIActivities(items))
}
```

### 6. Add CLI commands

```go
listCmd := &cobra.Command{
    Use:   "activities",
    Short: "List activities",
    RunE: func(cmd *cobra.Command, args []string) error {
        if err := setup(); err != nil { return err }
        httpClient, baseURL, cleanup, err := appcli.ClientForCommand(cmd, "myapp", app.Handler())
        if err != nil { return err }
        defer cleanup()
        client, _ := api.NewClientWithResponses(baseURL, api.WithHTTPClient(httpClient))
        resp, _ := client.ListActivitiesWithResponse(context.Background())
        // print results
        return nil
    },
}
```

### 7. Write use case tests

```go
h.Run("UC-XXX", "Create an activity", func(c *harness.Client) {
    c.SetHeader("X-Test-User", "test@example.com")
    resp := c.POST("/api/activities", `{"title":"Team standup"}`)
    c.AssertStatus(resp, 201)
    c.AssertJSONHas(resp, "title", "Team standup")
})
```

### 8. Initialize in setup()

```go
func setup() error {
    // ... existing setup ...
    activities, err = NewActivityStore(app.DB())
    if err != nil {
        return err
    }
    server := &MyServer{activities: activities, /* other stores */}
    api.HandlerFromMux(server, app.Server().Router())
    return nil
}
```

### 9. Verify

```bash
go build ./...
go test -v ./...
./dev lint-api
```

## Query Patterns

```go
// Single filter
coll.Where("user_id", "==", uid).All()

// Multiple filters (AND)
coll.Where("user_id", "==", uid).Where("status", "==", "active").All()

// Sort
coll.Where("user_id", "==", uid).OrderBy("created_at", store.Desc).All()

// Limit
coll.Where("user_id", "==", uid).Limit(10).All()

// First match
coll.Where("user_id", "==", uid).OrderBy("created_at", store.Desc).First()
```

## Anti-Patterns

- **Don't skip user_id** — every query must scope by user
- **Don't put business logic in handlers** — handlers validate and delegate to store
- **Don't create entities without tests** — every store method needs a test
- **Don't forget the CLI** — if the API supports it, the CLI should too
- **Don't hand-write SQL/Firestore backends** — use `store.Collection[T]` unless you need advanced queries
- **Don't edit generated files** — they'll be overwritten on `./dev codegen`
