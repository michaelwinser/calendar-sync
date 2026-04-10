---
name: appbase-app-yaml-reference
description: "app.yaml configuration reference — all fields, defaults, secret references, environment overrides, deployment targets. Read this instead of exploring config source code."
trigger: When you need to know what fields app.yaml supports, how to configure auth, store, scopes, tokens, GCP APIs, or deployment targets.
---

# app.yaml Reference

Complete reference for the `app.yaml` configuration file.

## Example

```yaml
name: my-app
port: 3000

store:
  type: sqlite
  path: data/app.db

gcp:
  apis:
    - tasks.googleapis.com
    - calendar-json.googleapis.com

auth:
  tokens:
    dev-token-12345: dev@localhost

environments:
  local:
    url: http://localhost:3000
    auth:
      client_id: ${secret:google-client-id}
      client_secret: ${secret:google-client-secret}
      extra_scopes:
        - https://www.googleapis.com/auth/tasks

  production:
    url: https://my-app.run.app
    store:
      type: firestore
      gcp_project: my-gcp-project
    auth:
      client_id: ${secret:google-client-id}
      client_secret: ${secret:google-client-secret}
      extra_scopes:
        - https://www.googleapis.com/auth/tasks
```

## Top-level fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | required | Application name (shown on login page, used for keychain) |
| `port` | int | `3000` | HTTP server port |

## store

Database configuration. Applies to all environments unless overridden.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `store.type` | string | `sqlite` | `sqlite` or `firestore` |
| `store.path` | string | `data/app.db` | SQLite database file path |
| `store.gcp_project` | string | — | GCP project ID (required for Firestore) |

## gcp

GCP-specific configuration for provisioning.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `gcp.apis` | [string] | — | Additional GCP APIs to enable during `appbase provision` |

Infrastructure APIs (Cloud Run, Firestore, etc.) are always enabled. This field is for app-specific APIs like `tasks.googleapis.com` or `calendar-json.googleapis.com`.

## auth

Authentication configuration. Can be set at top level or per-environment.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `auth.client_id` | string | — | Google OAuth client ID |
| `auth.client_secret` | string | — | Google OAuth client secret |
| `auth.redirect_url` | string | auto-detected | OAuth callback URL |
| `auth.allowed_users` | [string] | allow all | Email allowlist |
| `auth.extra_scopes` | [string] | — | Additional OAuth scopes to request |
| `auth.tokens` | map[string]string | — | Static token → email mappings |

### Secret references

Use `${secret:name}` to reference secrets from the OS keychain or GCP Secret Manager:

```yaml
auth:
  client_id: ${secret:google-client-id}
  client_secret: ${secret:google-client-secret}
```

Secret resolution order: OS keychain → Docker secrets → `.env` file → GCP Secret Manager → `SECRET_*` env vars.

### Token auth

Static tokens for lightweight auth without OAuth:

```yaml
auth:
  tokens:
    my-dev-token-123: dev@localhost
    ci-test-token-456: ci@test.com
```

Tokens must be at least 8 characters. Configure via env var as alternative: `AUTH_TOKENS=token1=email1,token2=email2`

### Extra scopes

Request additional OAuth permissions for Google API access:

```yaml
auth:
  extra_scopes:
    - https://www.googleapis.com/auth/tasks
    - https://www.googleapis.com/auth/calendar.readonly
```

These are combined with the default scopes (`openid`, `email`, `profile`).

## environments

Per-environment overrides. The active environment is determined by `APP_ENV` (default: `local`).

```yaml
environments:
  local:
    url: http://localhost:3000
    # All auth, store fields can be overridden

  staging:
    url: https://staging.my-app.run.app
    store:
      type: firestore
      gcp_project: my-staging-project

  production:
    url: https://my-app.run.app
    store:
      type: firestore
      gcp_project: my-prod-project
    auth:
      client_id: ${secret:google-client-id}
      client_secret: ${secret:google-client-secret}
      allowed_users:
        - admin@company.com
```

| Field | Type | Description |
|-------|------|-------------|
| `environments.<name>.url` | string | Base URL for this environment |
| `environments.<name>.port` | int | Port override |
| `environments.<name>.store` | StoreConfig | Database override |
| `environments.<name>.auth` | AuthConfig | Auth override |

## targets

Deployment targets describe where and how to deploy. Separate from `environments`, which control runtime behavior.

```yaml
targets:
  production:
    type: cloudrun
    project: my-gcp-project
    region: us-central1
    domain: myapp.example.com
    support_email: admin@example.com
    timeout: 600
    env:
      SYNC_KEY: ${secret:sync-key}
    store:
      type: firestore
    auth:
      client_id: ${secret:google-client-id}
      client_secret: ${secret:google-client-secret}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `type` | string | `cloudrun` | Deployment type |
| `project` | string | required | GCP project ID |
| `region` | string | `us-central1` | GCP region |
| `domain` | string | — | Custom domain (stable URL for schedulers, OAuth) |
| `support_email` | string | — | OAuth consent screen contact |
| `timeout` | int | — | Cloud Run request timeout in seconds |
| `env` | map | — | Extra env vars (supports `${secret:name}`) |
| `store` | StoreConfig | — | Database config for this target |
| `auth` | AuthConfig | — | Auth config for this target |

**Commands:**
- `appbase target list` — show configured targets
- `appbase target get <name>` — show all target fields
- `appbase target get <name> --field region` — get a single value (for scripts)
- `appbase provision <target>` — provision infrastructure for the target
- `appbase deploy <target>` — deploy to the target

**Backward compatibility:** If no `targets` section exists, deploy/provision synthesize a target from `environments.production` and `app.json`. Existing projects work without changes.

**Using target values in ./dev scripts:**
```sh
REGION=$(appbase target get production --field region)
PROJECT=$(appbase target get production --field project)
```

## Config priority

From highest to lowest:

1. **Environment variables** (`GOOGLE_CLIENT_ID`, `PORT`, etc.)
2. **Explicit `appbase.Config` fields** in Go code
3. **app.yaml** environment-specific overrides
4. **app.yaml** top-level values
5. **Defaults** (port 3000, sqlite, data/app.db)

## Related files

| File | Purpose |
|------|---------|
| `app.yaml` | Runtime configuration (this file) |
| `app.json` | Deploy script compatibility (name, gcpProject, region, urls) |
| `.env` | Fallback for secrets in CI/containers |
