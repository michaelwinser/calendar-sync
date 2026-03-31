# Calendar Sync

Google Calendar synchronization app built on [appbase](https://github.com/michaelwinser/appbase).

See `docs/MVSR.md` for mission, vision, strategy, and roadmap.

## Development Process

### Before committing

Run the code review agent manually on staged changes:

```bash
git diff --cached | claude -p "$(cat .claude/agents/code-review.md)"
```

Address all MUST-FIX items before committing. SUGGESTION items are optional.

### Starting a milestone

1. Write an implementation plan in `docs/M{n}-plan.md`
2. Run the architecture review agent:
   ```bash
   claude -p "$(cat .claude/agents/architecture-review.md) Review the plan in docs/M{n}-plan.md"
   ```
3. Address BLOCKING items before starting implementation
4. If major changes are made to the plan mid-milestone, re-run the architecture review

### Use cases and e2e tests

- Use cases are defined in `docs/PRD.md` with IDs like UC-0001
- E2e tests validate use cases via the CLI
- `./dev ci` runs the full e2e suite
- All use cases marked as completed for the current milestone must pass before committing

## Sandboxing

All development sessions should run inside a [nono](https://github.com/always-further/nono) sandbox:

```bash
./sandbox claude                    # Interactive Claude Code session
./sandbox ./dev ci                  # CI pipeline
./sandbox ./dev serve               # Dev server
```

The sandbox extends the `claude-code` profile with:
- `~/.config/calendar-sync` (app data) — read+write
- `~/go` (Go module cache) — read+write
- Port 4004 binding (dev server)

**Deploy runs outside the sandbox** — it needs gcloud credentials which are correctly blocked by nono's sensitive path policy:
```bash
# Deploy directly (not via ./sandbox)
./dev deploy
```

## Constraints

- **No local mode**: This app always requires Google OAuth — do not use appbase's `LocalMode`. Every code path assumes an authenticated user with calendar scopes.
- **Devcontainer for non-Go tooling**: All tools other than Go run in the devcontainer (oapi-codegen, etc.). If a new tool is needed, ask before adding it.

## Tech Stack

- **Backend**: Go, appbase (chi router, SQLite, Google OAuth)
- **Frontend**: Pure HTML+CSS with minimal JavaScript (no SPA framework)
- **API**: OpenAPI spec with oapi-codegen
