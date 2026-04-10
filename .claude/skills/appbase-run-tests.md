---
name: run-tests
description: Run and interpret the test suite
trigger: When the user asks to run tests, check test status, or debug test failures
---

# Running and Interpreting Tests

## Quick Commands

```bash
# All tests
go test ./...

# Verbose with use case IDs visible
go test -v ./...

# Specific package
go test ./store/...

# Specific use case (by name pattern)
go test -v -run UC-0002 ./...

# Via CLI (if available)
myapp test
```

## Understanding Output

### Use Case Tests

Tests are named `UC-XXXX_Description`:

```
=== RUN   TestUseCases/UC-0001_List_returns_empty
--- PASS: TestUseCases/UC-0001_List_returns_empty (0.00s)
=== RUN   TestUseCases/UC-0002_Create_item
--- FAIL: TestUseCases/UC-0002_Create_item (0.00s)
    usecases_test.go:42: expected status 201, got 500. Body: {"error":"table not found"}
```

**When a UC test fails:**
1. Note the UC-XXXX ID
2. Check the PRD for the acceptance criteria
3. The error message usually indicates the layer:
   - `401` → auth/session issue
   - `400` → validation issue in handler
   - `500` → store/database error
   - `404` → entity not found or user scoping issue

### Common Failure Patterns

| Error | Likely Cause |
|-------|-------------|
| `401 unauthorized` | Missing `login(c)` in test, or session expired |
| `table not found` | Schema migration not run in test setup |
| `no such column` | Schema changed but migration uses `IF NOT EXISTS` (old table persists) |
| `UNIQUE constraint` | Test creating duplicate data without cleanup |

## Test Isolation

- Each test gets its own in-memory SQLite database (`:memory:`)
- Tests within a `TestUseCases` function share the same database (they run sequentially)
- Use separate `TestUseCases` functions if you need isolated databases

## CI

GitHub Actions runs `go test ./...` on every push and PR. Check `.github/workflows/ci.yml` for the pipeline.
