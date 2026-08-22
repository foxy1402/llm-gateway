# LLM Gateway

A minimal-dependency Go service that aggregates multiple free/custom LLM provider endpoints behind a single **OpenAI-compatible** API. Callers use it exactly like the OpenAI API; the gateway routes each request to a real upstream, rotating across a pool and falling back on failure.

Configuration (providers, combos, settings) lives in an embedded SQLite database and is edited live through the built-in web dashboard. Everything can also be exported/imported as plain SQL for backup or version control.

## Why Go

- ~10–15 MB RSS idle (`modernc.org/sqlite` is pure Go — no CGO), vs ~80–120 MB for a Python FastAPI stack
- Native goroutines for concurrent upstream calls
- Single static binary, trivial multi-arch Docker image
- The OpenAI wire protocol is just HTTP + JSON — no SDK needed on the server

## Features

- **OpenAI-compatible API**: `/v1/chat/completions`, `/v1/completions`, `/v1/responses`, `/v1/models`, `/v1/embeddings` (`/v1/embeddings` must address a provider id directly — see [Combos](#combos))
- **Combos**: virtual model IDs that fan out to a pool of providers with configurable rotation and failure-aware fallback
- **Four rotation strategies**: `round-robin`, `weighted-round-robin` (smooth, Nginx-style), `priority` (ordered failover), `random`
- **`/v1/responses` translation**: providers without native Responses API support are transparently translated through chat completions
- **Health & cooldown**: failing upstreams are cooled down for a configurable window and skipped during selection
- **Streaming**: SSE passthrough for chat completions and translated Responses-format streaming
- **SQL export/import**: back up or migrate all config as a human-readable `.sql` file
- **Web dashboard**: manage providers, combos, settings; view request logs, health, and a requests-per-hour chart; test connectivity
- **Single binary**: frontend assets embedded via `go:embed`; no npm, no build step

## Quickstart

### Docker

```bash
docker run \
  -e GATEWAY_API_KEY=gw-your-secret \
  -e DASHBOARD_PASSWORD=admin123 \
  -e DASHBOARD_SECRET=$(head -c 32 /dev/urandom | base64) \
  -e DB_PATH=/data/gateway.db \
  -v gateway_data:/data \
  -p 8080:8080 \
  ghcr.io/foxy1402/llm-gateway:latest
```

Then open `http://localhost:8080/dashboard/` and sign in with `DASHBOARD_PASSWORD`. The `latest` tag is rebuilt on every push to `main`/`master` and ships `linux/amd64` + `linux/arm64`.

**Data directory permissions:** the image runs as the distroless `nonroot` user (uid/gid 65532) and bakes in a `/data` dir owned by that user. A named Docker volume (`-v gateway_data:/data`) inherits that owner on first mount, so it "just works." On a PaaS that mounts a *different* path or forces a root-owned volume, the gateway can't write SQLite and exits with `unable to open database file (14)`. Fixes, in order of preference:

- Point `DB_PATH` at a path your platform guarantees is writable (often a mounted app-storage volume).
- Pre-create the volume with uid 65532 ownership, e.g. `docker run --rm -v gateway_data:/data alpine chown -R 65532:65532 /data`.
- If your platform forces a specific uid, override with `docker run --user <their-uid> …` and a matching writable `DB_PATH`.

### From source

Requires Go 1.25+ (per `go.mod` and the `modernc.org/sqlite v1.55` dependency).

```bash
export GATEWAY_API_KEY=gw-your-secret
export DASHBOARD_PASSWORD=admin123
export DASHBOARD_SECRET=$(openssl rand -base64 32)   # or see table below for Windows
go run ./cmd/gateway
```

On first boot the SQLite DB is created empty. Add providers and combos via the dashboard.

### Docker Compose / Portainer

See [`docker-compose.example.yml`](docker-compose.example.yml):

```yaml
services:
  gateway:
    image: ghcr.io/foxy1402/llm-gateway:latest
    ports: ["8080:8080"]
    environment:
      GATEWAY_API_KEY: "gw-change-me"
      DASHBOARD_PASSWORD: "change-me"
      DASHBOARD_SECRET: "a-random-32-char-string-goes-here"
      DB_PATH: "/data/gateway.db"
    volumes: ["gateway_data:/data"]
    restart: unless-stopped
volumes:
  gateway_data:
```

## Environment variables

All bootstrap/secret config comes from env vars. Everything else lives in SQLite and is managed via the dashboard.

| Variable | Required | Default | Description |
|---|---|---|---|
| `GATEWAY_API_KEY` | yes | — | Bearer key callers use for `/v1/*` endpoints |
| `DASHBOARD_PASSWORD` | yes | — | Password to log in to the web dashboard |
| `DASHBOARD_SECRET` | yes | — | Random string used to sign session cookies (min 32 chars). See [Generating DASHBOARD_SECRET](#generating-dashboard_secret). |
| `GATEWAY_LISTEN` | no | `:8080` | Listen address. Accepts `:8080`, `8080` (bare port), or `0.0.0.0:8080`. Some PaaS hosts reject the leading `:`, so a plain port number like `8080` is normalized to `:8080` for you. |
| `GATEWAY_LOG_LEVEL` | no | `info` | `debug` \| `info` \| `warn` \| `error` |
| `DB_PATH` | no | `./gateway.db` | SQLite file path (use a mounted volume in Docker) |
| `REQUEST_TIMEOUT` | no | `60s` | Timeout for **connect + response headers** only (per attempt). Streaming bodies run unbounded until the client disconnects or the upstream stalls for 90s — a fixed timeout would kill long coding generations mid-edit. |
| `MAX_REQUEST_BODY_MB` | no | `25` | Cap on the incoming `/v1/*` request body size. Raise this if you send large base64-encoded images (vision/OCR) or documents — a single high-res image can easily be 5-20MB as base64 JSON. `0` or invalid values fall back to the default. |
| `MAX_ACCOUNT_ATTEMPTS_PER_PROVIDER` | no | `10` | Self-heal ceiling: how many accounts (API keys) of **one** provider get tried within a single request before giving up on it. Default `10` covers realistic key pools in full (a provider with 5 keys tries all 5 before failing). Lower it if you'd rather fail fast than have one slow/unlucky request serially churn through a very large key pool. `0` or invalid values fall back to the default. |
| `BAN_MAXFAIL` | no | `5` | Failed dashboard logins within `BAN_FIND_TIME` before the client IP is banned (429 + `Retry-After`). Guards the login that protects all stored provider keys. `0` disables the gate. |
| `BAN_FIND_TIME` | no | `10m` | Failure window for the login fail-to-ban counter |
| `BAN_TIME` | no | `30m` | Base ban duration; doubles per repeat offense |
| `BAN_MAX_TIME` | no | `24h` | Cap on the escalated ban duration |
| `TRUSTED_PROXY` | no | `0` | Set `1` when running behind a PaaS load balancer or reverse proxy so the fail-to-ban reads the real client IP from `X-Forwarded-For`/`X-Real-IP`. **Leave unset when the gateway is directly exposed** — otherwise a spoofed header could evade bans. Without it on a PaaS, all traffic appears to come from the load balancer and a ban would lock everyone out, including you. |

### Generating DASHBOARD_SECRET

Any cryptographically random string ≥ 32 chars works. Generate locally (no website needed):

| Platform | Command |
|---|---|
| **Windows (PowerShell)** | `-join ((48..57)+(65..90)+(97..122) \| Get-Random -Count 48 \| ForEach-Object {[char]$_})` |
| **macOS / Linux** | `openssl rand -base64 32` |
| **macOS / Linux (alt)** | `head -c 32 /dev/urandom \| base64` |

Or an online generator if you prefer: [passwordsgenerator.net](https://passwordsgenerator.net/) (set length ≥ 32), [1Password's generator](https://1password.com/password-generator), or [bitwarden.com/password-generator](https://bitwarden.com/password-generator/). Prefer generating locally — the secret signs admin session cookies, so avoid pasting it into third-party sites.

## Verifying before you push (PaaS smoke test)

Don't ship blind. `scripts/deploy-smoke.ps1` reproduces exactly what CI and a real container deployment do and fails non-zero if anything regresses — build the image, boot it as `nonroot` with a fresh volume (catches the `CANTOPEN` DB crash), drive the dashboard admin API, check API-key auth, stream a slow SSE through a combo (catches stream-timeout bugs), and confirm streaming usage is logged as real tokens.

```powershell
powershell -ExecutionPolicy Bypass -File scripts\deploy-smoke.ps1
```

Requires Docker Desktop running locally. Green output = safe to push. Run this before every commit that touches `internal/proxy`, `internal/store`, `internal/dashboard`, `internal/config`, or `Dockerfile`.

## Concepts

### Providers

A provider is one upstream LLM endpoint: base URL, auth key, model name, weight, tags, enabled flag, and whether it speaks `/v1/responses` natively. Its **ID** is what callers use as the `model` field.

The **base URL** is the full OpenAI-compatible root *including its version path*. The gateway always appends a bare endpoint path (`/chat/completions`, `/completions`, `/responses`, `/embeddings`, `/models`) directly onto it, so you control the exact version:

- `https://api.groq.com/openai/v1` → `…/openai/v1/chat/completions`
- `https://generativelanguage.googleapis.com/v1beta/openai` → `…/v1beta/openai/chat/completions`
- `https://generativelanguage.googleapis.com/v1beta` → `…/v1beta/chat/completions`
- `https://open.bigmodel.cn/api/paas/v4` → `…/api/paas/v4/chat/completions`

No version-detection heuristics are applied, so any OpenAI-compatible host (including custom roots like `https://host/custom`) works out of the box.

### Combos

A combo is a virtual model ID bound to an ordered pool of provider IDs plus a rotation policy. Callers send `model: "<combo-id>"` and the gateway picks a healthy member, retrying others on failure.

| Rotation | Behavior |
|---|---|
| `round-robin` | Cycles members evenly (atomic counter modulo available). |
| `weighted-round-robin` | Smooth WRR — members picked in proportion to `weight`. |
| `priority` | Always tries members in order, skipping unhealthy ones (classic failover). |
| `random` | Random healthy member, weighted by provider `weight` (higher weight picked proportionally more often; uniform when all weights are 0/1). |

**`/v1/embeddings` cannot address a combo — by design, not a bug.** Embedding vectors from different models aren't interchangeable: they can differ in dimension and always live in a different (incompatible) vector space. If a combo silently rotated an embeddings request across providers/models, you could embed some documents with model A and a later query with model B — similarity search against that mix returns meaningless results, with nothing in the response telling you it happened. Requesting `model: "<combo-id>"` against `/v1/embeddings` fails fast with `404` (`"model %q is a combo; embeddings require a direct provider id"`) instead.

This doesn't cost you self-heal, though: **a direct provider with multiple accounts/keys already rotates across all of them on `/v1/embeddings`**, exactly like it does for chat completions (see [Failure & cooldown](#failure--cooldown)) — one provider id, many keys under it, no combo needed.

### Failure & cooldown

When an upstream returns a retryable status (default `402, 429, 500, 502, 503, 504`) or times out, the gateway records a failure and cools that **account** (API key) down for `health.cooldown` seconds (default 60) — a sibling key on the same provider stays in rotation. `402` is included by default because several OpenAI-compatible upstreams (e.g. some Vercel AI Gateway providers) report exhausted credits as "402 Payment Required" instead of the more common `429`; both are equally account-scoped and equally worth rotating away from. Customize the set via the `health.error_codes` dashboard/admin setting (comma-separated status codes) and the cooldown via `health.cooldown`. A 404 on `/v1/completions` permanently flags that provider as not supporting the legacy endpoint (until restart).

**Self-heal within one request**: a direct provider call or an unpinned combo member automatically rotates across that provider's *other* healthy keys — up to `MAX_ACCOUNT_ATTEMPTS_PER_PROVIDER` of them — before the request fails, so a single client call transparently survives one or more keys being rate-limited or out of credit. Pinned combo members (a member bound to one specific key) don't have siblings to fall back to *within that member*, but a same-provider sibling pinned to a **different** key on another combo member still rotates in normally.

## API examples

### List models

```bash
curl -H "Authorization: Bearer $GATEWAY_API_KEY" http://localhost:8080/v1/models
```

### Chat completions

```bash
curl -H "Authorization: Bearer $GATEWAY_API_KEY" \
     -H "Content-Type: application/json" \
     -d '{"model":"free-mix","messages":[{"role":"user","content":"hello"}]}' \
     http://localhost:8080/v1/chat/completions
```

Streaming works by adding `"stream": true`; the gateway passes SSE chunks through.

### Responses API

```bash
curl -H "Authorization: Bearer $GATEWAY_API_KEY" \
     -H "Content-Type: application/json" \
     -d '{"model":"free-mix","input":"Tell me a joke","instructions":"Be brief"}' \
     http://localhost:8080/v1/responses
```

If the chosen upstream provider lacks native Responses support, the gateway translates the request to chat completions and the response back to the Responses shape.

### Direct provider call

Use a provider's ID as the model to bypass combos:

```bash
curl -H "Authorization: Bearer $GATEWAY_API_KEY" \
     -H "Content-Type: application/json" \
     -d '{"model":"groq-llama3","messages":[{"role":"user","content":"hi"}]}' \
     http://localhost:8080/v1/chat/completions
```

### Admin endpoints

```bash
# JSON health/config dump
curl -H "Authorization: Bearer $GATEWAY_API_KEY" http://localhost:8080/admin/status

# Hot-reload the registry from the DB
curl -X POST -H "Authorization: Bearer $GATEWAY_API_KEY" http://localhost:8080/admin/reload

# Download SQL export
curl -H "Authorization: Bearer $GATEWAY_API_KEY" http://localhost:8080/admin/export -o backup.sql

# Import SQL (replaces all providers, combos, settings)
curl -X POST -H "Authorization: Bearer $GATEWAY_API_KEY" \
     -H "Content-Type: application/sql" --data-binary @backup.sql \
     http://localhost:8080/admin/import
```

### Probes

- `GET /healthz` — always 200 (liveness)
- `GET /readyz` — 200 if at least one provider is enabled, else 503

## Export / import

The dashboard's **Export/Import** page (or `GET/POST /admin/export|import`) produces a plain `.sql` file:

```sql
-- LLM Gateway Export
-- Generated: 2026-07-31T00:00:00Z
-- Version: 1
BEGIN TRANSACTION;
DELETE FROM combo_members;
DELETE FROM combos;
DELETE FROM providers;
DELETE FROM settings;
INSERT INTO providers (...) VALUES (...), (...);
...
COMMIT;
```

Import validates the header, runs inside a transaction, and rolls back on any error. **Import replaces all providers, combos, and settings.**

## Dashboard

Served at `/dashboard/`. Sign in with `DASHBOARD_PASSWORD`. Sections:

- **Overview** — provider/combo counts, requests today, live health table, recent requests
- **Providers** — CRUD, weight/tags/edit, enable toggle, test button
- **Combos** — CRUD with drag-to-reorder members (matters for `priority`), test button
- **Request Logs** — filter by provider/endpoint/errors, pagination, 24h stacked bar chart
- **Settings** — cooldown, rotation error codes, log retention
- **Export/Import** — SQL backup and restore

The frontend is vanilla JS with no build step, embedded in the binary via `go:embed`.

## Development

```bash
go build ./...     # build
go test ./...      # unit + integration tests
go vet ./...       # vet
```

Project layout:

```
cmd/gateway/         entry point, env parsing, bootstrap
internal/config/     env + domain types
internal/store/      SQLite open/migrate, CRUD, export/import
internal/registry/   in-memory provider/combo cache + health/cooldown
internal/proxy/      request translation, rotation/retry, SSE streaming
internal/auth/       Bearer API key + dashboard session auth
internal/dashboard/  dashboard JSON API + embedded SPA
internal/middleware/ logging + panic recovery
```

## Error behavior

| Scenario | Behavior |
|---|---|
| Upstream 402 / 429 / 5xx | Rotate to the next account (same provider first, then next combo member), apply per-account cooldown |
| Upstream 404/405 on any endpoint | Mark provider unsupported for that endpoint, rotate |
| Every account/combo member exhausted after a 402/429/5xx | Surface the last real upstream status (e.g. `429`) instead of a generic error |
| Every account/combo member exhausted with no upstream response at all | `502` with OpenAI-shaped error |
| Upstream connect/header timeout | Rotate |
| Client disconnects mid-stream | Upstream request aborted (no eager rotation) |
| Stream stalls >90s between chunks | Stream gracefully terminated |
| Bad/missing API key | `401`, no upstream call |
| Unknown model | `404` OpenAI-shaped error |
| `/v1/embeddings` addressed to a combo id | `404` — embeddings require a direct provider id (by design, see [Combos](#combos)) |
| Body too large | `413` |
| Import without export header | `400`, no DB change |
| Import DB error | `500`, transaction rolled back |

## License

MIT
