# LLM Gateway — Implementation Plan

## Project Overview

A minimal-dependency Go service that acts as an OpenAI-compatible API gateway. It aggregates multiple free/custom LLM provider endpoints (each with its own base URL, auth key, and model name) behind a single endpoint protected by one custom auth key. Callers interact with it exactly as they would with the OpenAI API. The gateway exposes user-defined "combos" — virtual model IDs that fan out to a pool of real upstream models with configurable rotation and failure-aware fallback.

All configuration (providers, combos, gateway settings) is stored in an embedded SQLite database, editable live via the web dashboard. Providers and combos can also be exported/imported as plain SQL for backup, migration, or version control.

**Language: Go 1.22+**
Rationale over Python: ~10–15 MB RSS idle vs ~80–120 MB for a FastAPI/Uvicorn stack. No GIL, native goroutines for concurrent upstream calls, single static binary, trivial multi-arch Docker image. The OpenAI wire protocol is just HTTP + JSON — no SDK required on the server side.

---

## Repository Layout

```
llm-gateway/
├── cmd/
│   └── gateway/
│       └── main.go                  # entry point, flag/env parsing, server bootstrap
├── internal/
│   ├── config/
│   │   ├── config.go                # load env vars + bootstrap defaults
│   │   └── types.go                 # Provider, Combo, RotationPolicy structs
│   ├── store/
│   │   ├── store.go                 # SQLite open/migrate, all CRUD operations
│   │   ├── schema.sql               # embedded schema (go:embed)
│   │   └── export.go                # SQL dump export + import parser
│   ├── registry/
│   │   ├── registry.go              # in-memory provider + combo cache (loaded from DB)
│   │   └── health.go                # per-provider circuit-breaker / cooldown state
│   ├── router/
│   │   └── router.go                # HTTP mux: API routes + dashboard routes
│   ├── proxy/
│   │   ├── proxy.go                 # request translation & upstream dispatch
│   │   ├── responses.go             # /v1/responses protocol handler
│   │   ├── stream.go                # SSE passthrough for streaming responses
│   │   └── retry.go                 # rotation + retry logic (round-robin / priority)
│   ├── auth/
│   │   ├── api.go                   # Bearer token middleware (gateway API key)
│   │   └── dashboard.go             # session cookie auth for dashboard
│   ├── dashboard/
│   │   ├── handler.go               # dashboard HTTP handlers (login, pages, API)
│   │   └── static/                  # embedded frontend assets (go:embed)
│   │       ├── index.html           # SPA shell
│   │       ├── app.js               # vanilla JS, no build step needed
│   │       └── style.css            # minimal styling
│   └── middleware/
│       ├── logger.go                # structured request/response logging
│       └── recover.go               # panic recovery
├── Dockerfile                       # multi-stage, produces amd64 + arm64 images
├── docker-compose.example.yml       # example with volume for SQLite persistence
├── .github/
│   └── workflows/
│       └── release.yml              # build & push OCI image to ghcr.io on tag push
└── README.md
```

---

## Environment Variables (Primary Configuration)

All bootstrap/secret config comes from env vars. Everything else lives in the SQLite DB and is managed via the dashboard.

| Variable | Required | Default | Description |
|---|---|---|---|
| `GATEWAY_API_KEY` | yes | — | Bearer key callers use for `/v1/*` endpoints |
| `DASHBOARD_PASSWORD` | yes | — | Password to log in to the web dashboard |
| `DASHBOARD_SECRET` | yes | — | Random string used to sign session cookies (min 32 chars) |
| `GATEWAY_LISTEN` | no | `:8080` | Host:port to listen on |
| `GATEWAY_LOG_LEVEL` | no | `info` | `debug` \| `info` \| `warn` \| `error` |
| `DB_PATH` | no | `./gateway.db` | SQLite file path (use a mounted volume in Docker) |
| `REQUEST_TIMEOUT` | no | `60s` | Per-upstream attempt timeout |

**Docker / Portainer example:**

```
docker run \
  -e GATEWAY_API_KEY=gw-your-secret \
  -e DASHBOARD_PASSWORD=admin123 \
  -e DASHBOARD_SECRET=some-random-32-char-string-here \
  -e DB_PATH=/data/gateway.db \
  -v gateway_data:/data \
  -p 8080:8080 \
  ghcr.io/you/llm-gateway:latest
```

No config file needed. On first boot, if the DB doesn't exist it is created with an empty schema. All providers, combos, and settings are then added through the dashboard.

---

## Storage — SQLite Schema (`internal/store/schema.sql`)

```sql
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS providers (
    id         TEXT PRIMARY KEY,
    display    TEXT NOT NULL,
    base_url   TEXT NOT NULL,
    auth_key   TEXT NOT NULL,
    model      TEXT NOT NULL,
    weight     INTEGER NOT NULL DEFAULT 1,
    tags       TEXT NOT NULL DEFAULT '',   -- comma-separated
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE IF NOT EXISTS combos (
    id           TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    rotation     TEXT NOT NULL DEFAULT 'round-robin',
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE IF NOT EXISTS combo_members (
    combo_id    TEXT NOT NULL REFERENCES combos(id) ON DELETE CASCADE,
    provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    position    INTEGER NOT NULL DEFAULT 0,   -- order matters for priority rotation
    PRIMARY KEY (combo_id, provider_id)
);

CREATE TABLE IF NOT EXISTS request_log (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    ts            INTEGER NOT NULL DEFAULT (unixepoch()),
    model_in      TEXT NOT NULL,   -- what caller sent (combo id or provider id)
    provider_used TEXT NOT NULL,   -- actual upstream provider id
    endpoint      TEXT NOT NULL,   -- chat.completions | completions | responses
    status        INTEGER NOT NULL,
    latency_ms    INTEGER NOT NULL,
    prompt_tokens INTEGER,
    completion_tokens INTEGER,
    error         TEXT
);
CREATE INDEX IF NOT EXISTS idx_log_ts ON request_log(ts DESC);
CREATE INDEX IF NOT EXISTS idx_log_provider ON request_log(provider_used);
```

The `request_log` table powers the dashboard analytics view. It is append-only; old rows can be pruned via a dashboard setting (default: keep 30 days).

---

## Export / Import SQL

### Export (`GET /admin/export` or dashboard button)

Produces a plain `.sql` file the user can download. No binary format, no proprietary schema — just INSERT statements readable in any text editor.

```sql
-- LLM Gateway Export
-- Generated: 2025-07-30T12:00:00Z
-- Version: 1

BEGIN TRANSACTION;

DELETE FROM combo_members;
DELETE FROM combos;
DELETE FROM providers;
DELETE FROM settings;

INSERT INTO providers (id, display, base_url, auth_key, model, weight, tags, enabled) VALUES
  ('groq-llama3', 'Groq Llama 3', 'https://api.groq.com/openai/v1', 'gsk_...', 'llama3-70b-8192', 1, 'fast,free', 1),
  ('together-mistral', 'Together Mistral', 'https://api.together.xyz/v1', 'tgr_...', 'mistralai/Mixtral-8x7B-Instruct-v0.1', 2, 'free', 1);

INSERT INTO combos (id, display_name, rotation, enabled) VALUES
  ('free-mix', 'Free Mix', 'round-robin', 1);

INSERT INTO combo_members (combo_id, provider_id, position) VALUES
  ('free-mix', 'groq-llama3', 0),
  ('free-mix', 'together-mistral', 1);

INSERT INTO settings (key, value) VALUES
  ('health.cooldown', '60'),
  ('health.error_codes', '429,500,502,503,504'),
  ('log.retention_days', '30');

COMMIT;
```

### Import (`POST /admin/import` or dashboard file upload)

The import handler:
1. Reads the uploaded `.sql` file entirely into memory.
2. Validates it starts with `-- LLM Gateway Export` header (rejects arbitrary SQL).
3. Executes it inside a transaction.
4. On success: reloads the registry from DB.
5. On failure: rolls back, returns error detail to caller.

This is intentionally simple — it replaces all data (DELETE + INSERT). A "merge" mode (INSERT OR IGNORE) can be added later if needed.

**Implementation note:** use `database/sql` `Exec` for the whole block — SQLite accepts multi-statement strings in a single exec. Parse the file into individual statements by splitting on `;` and execute each in the open transaction to keep error recovery clean.

---

## Core Data Structures (`internal/config/types.go`)

```go
type Provider struct {
    ID      string
    Display string
    BaseURL string
    AuthKey string
    Model   string
    Weight  int
    Tags    []string
    Enabled bool
}

type RotationPolicy string

const (
    RoundRobin         RotationPolicy = "round-robin"
    WeightedRoundRobin RotationPolicy = "weighted-round-robin"
    Priority           RotationPolicy = "priority"
    Random             RotationPolicy = "random"
)

type Combo struct {
    ID          string
    DisplayName string
    Rotation    RotationPolicy
    Members     []string // provider IDs in position order
    Enabled     bool
}
```

---

## API Endpoint Coverage

### `/v1/chat/completions` (POST)
Standard OpenAI chat endpoint. Supports both streaming (`stream: true`) and non-streaming. The gateway rewrites the `model` field to the upstream's real model name before forwarding.

### `/v1/completions` (POST)
Legacy text completions endpoint. Same proxy/rewrite/retry logic as chat completions; forwarded to upstream `/v1/completions`. Some free providers don't support this — if the upstream returns 404, treat it as a hard failure and rotate.

### `/v1/responses` (POST)
OpenAI's Responses API (introduced 2025). Different shape from chat completions:

**Request shape:**
```json
{
  "model": "free-mix",
  "input": "Tell me a joke",
  "instructions": "You are a helpful assistant.",
  "stream": false
}
```

**Upstream translation strategy:**

Most free providers do not yet expose `/v1/responses` natively. The gateway translates transparently:

1. Detect if upstream provider has `responses_native: true` in its config → forward as-is to `{base_url}/v1/responses`.
2. Otherwise (default) → translate to a `/v1/chat/completions` call:
   - Map `input` (string or array) → `messages[{role:"user", content: input}]`
   - Map `instructions` → `messages[{role:"system", content: instructions}]` prepended
   - Forward to upstream `/v1/chat/completions`
   - Translate the chat completion response back to Responses API shape before returning to caller.

**Response shape (translated):**
```json
{
  "id": "resp_abc123",
  "object": "response",
  "created_at": 1700000000,
  "model": "free-mix",
  "output": [
    {
      "type": "message",
      "role": "assistant",
      "content": [{ "type": "output_text", "text": "Why did the..." }]
    }
  ],
  "usage": { "input_tokens": 12, "output_tokens": 30, "total_tokens": 42 }
}
```

**Streaming for `/v1/responses`:**
When `stream: true`, the translated SSE chunks use the Responses API event names (`response.output_text.delta`, `response.completed`, etc.) rather than the chat completions delta format. The stream module handles both shapes via a `StreamFormat` enum:

```go
type StreamFormat int
const (
    StreamFormatChat      StreamFormat = iota // data: {"choices":[{"delta":...}]}
    StreamFormatResponses                     // data: {"type":"response.output_text.delta",...}
)
```

**Provider field addition:**
```sql
ALTER TABLE providers ADD COLUMN responses_native INTEGER NOT NULL DEFAULT 0;
```
Dashboard shows a toggle "Supports /v1/responses natively" per provider. When off (default), the gateway silently translates.

### `/v1/models` (GET)
Returns all enabled combos first, then all enabled direct providers, in OpenAI list format.

### `/v1/embeddings` (POST)
Simple passthrough (no combo routing — embeddings models differ from chat models). Route to a named provider directly. Can be added in Phase 6.

---

## Module Responsibilities

### `internal/store` — Persistence Layer

All DB access goes through this package. Exposes typed methods only — no raw SQL outside this package.

```go
func (s *Store) ListProviders() ([]Provider, error)
func (s *Store) GetProvider(id string) (*Provider, error)
func (s *Store) UpsertProvider(p Provider) error
func (s *Store) DeleteProvider(id string) error

func (s *Store) ListCombos() ([]Combo, error)
func (s *Store) UpsertCombo(c Combo) error
func (s *Store) DeleteCombo(id string) error

func (s *Store) LogRequest(entry LogEntry) error
func (s *Store) QueryLogs(filter LogFilter) ([]LogEntry, error)
func (s *Store) PruneLogs(olderThanDays int) error

func (s *Store) ExportSQL() (string, error)
func (s *Store) ImportSQL(sql string) error

func (s *Store) GetSetting(key string) (string, error)
func (s *Store) SetSetting(key, value string) error
```

SQLite driver: `modernc.org/sqlite` — pure Go, no CGO, works in distroless. Single additional dependency.

### `internal/registry` — Runtime State

Loaded from DB at startup and after any mutation via dashboard. Hot-reload path: dashboard handler calls `store.UpsertProvider(...)` then `registry.Reload()`. No restart needed.

```go
type Registry struct {
    mu        sync.RWMutex
    providers map[string]*Provider
    combos    map[string]*Combo
    health    map[string]*ProviderHealth
    // per-combo round-robin counters
    rrCounters map[string]*atomic.Int64
    // per-combo smooth-WRR state
    wrrState   map[string][]wrrEntry
}

func (r *Registry) Reload(store *Store) error
func (r *Registry) IsAvailable(id string) bool
func (r *Registry) RecordFailure(id string, code int)
func (r *Registry) RecordSuccess(id string)
```

### `internal/proxy/proxy.go` — Request Translation

Incoming request arrives with `model` = a combo ID or provider ID.

Steps:
1. Peek body (via `io.TeeReader`) to extract `model` and `stream` fields.
2. Look up combo or provider in registry.
3. Enter retry loop (see `retry.go`).
4. Rewrite body: replace `model` with upstream's real model name.
5. For `/v1/responses` with non-native provider: translate body to chat completions shape.
6. Forward with upstream auth header; strip inbound `Authorization`.
7. On success: translate response back if needed, log to DB, return to caller.

### `internal/proxy/responses.go` — Responses API Translation

```go
// Translate /v1/responses request → /v1/chat/completions request body
func ResponsesToChatRequest(body []byte, upstreamModel string) ([]byte, error)

// Translate /v1/chat/completions response → /v1/responses response body
func ChatToResponsesResponse(body []byte, originalModel string) ([]byte, error)

// Translate chat completions SSE stream → responses SSE stream (chunk by chunk)
func TranslateResponsesStream(upstream io.Reader, w http.ResponseWriter) error
```

### `internal/proxy/retry.go` — Rotation Engine

```
func SelectUpstream(combo *Combo, reg *Registry, exclude []string) (*Provider, error)
```

Implements all four strategies. For `round-robin` and `weighted-round-robin`, state is per-combo atomic counter stored in `Registry`. For `priority`, iterate members in order skipping unhealthy ones. `exclude` is the set already attempted this request.

Retry loop:
```
for attempt := 0; attempt < len(combo.Members); attempt++:
    provider = SelectUpstream(combo, reg, tried)
    if provider == nil: break   # all exhausted
    resp, err = dispatch(provider, req)
    if err == nil && resp.StatusCode not in errorCodes:
        reg.RecordSuccess(provider.ID)
        log request to DB (async goroutine)
        return resp
    reg.RecordFailure(provider.ID, resp.StatusCode)
    tried = append(tried, provider.ID)
return 502 "all upstreams failed"
```

### `internal/router/router.go` — HTTP Mux

Uses `net/http` `ServeMux` (Go 1.22 pattern routing):

| Route | Auth | Handler |
|---|---|---|
| `POST /v1/chat/completions` | API key | Chat proxy |
| `POST /v1/completions` | API key | Legacy completions proxy |
| `POST /v1/responses` | API key | Responses API proxy / translator |
| `GET /v1/models` | API key | Model list |
| `POST /v1/embeddings` | API key | Embeddings passthrough |
| `GET /healthz` | none | Liveness probe |
| `GET /readyz` | none | Readiness (≥1 provider healthy) |
| `GET /admin/status` | API key | JSON health dump |
| `POST /admin/reload` | API key | Trigger registry reload |
| `GET /admin/export` | API key | Download SQL export |
| `POST /admin/import` | API key | Upload SQL import |
| `GET /dashboard/` | session | Dashboard SPA shell |
| `POST /dashboard/login` | none | Password → set session cookie |
| `POST /dashboard/logout` | session | Clear session cookie |
| `GET /dashboard/api/*` | session | Dashboard JSON API (CRUD + logs + export) |

### `internal/auth/api.go` — API Bearer Middleware

Validates `Authorization: Bearer <key>` against `GATEWAY_API_KEY`. Returns `401` on mismatch.

### `internal/auth/dashboard.go` — Session Middleware

On `POST /dashboard/login`: compare posted password to `DASHBOARD_PASSWORD`. On match, create a signed session token (HMAC-SHA256 over a random nonce using `DASHBOARD_SECRET`) stored in a `HttpOnly; SameSite=Strict` cookie. Sessions are in-memory map with 24h TTL (resets on restart, forcing re-login — acceptable for personal use). All `/dashboard/*` routes except `/login` check for valid session cookie.

---

## Web Dashboard

A server-rendered + minimal JS single-page app. No npm, no build step. All assets are embedded in the binary via `go:embed`. Served at `/dashboard/`.

### Pages / Sections

**Overview (home)**
- Total providers, combos, requests today
- Per-provider health status table (green/cooldown/disabled) with cooldown countdown
- Last 10 request log entries

**Providers**
- Table of all providers: ID, display name, model, base URL (masked), weight, enabled toggle, tags
- Add / Edit form: all provider fields including `responses_native` toggle
- Delete with confirmation
- "Test" button: sends a minimal `POST /v1/chat/completions` through the gateway using that provider directly and shows response time + status

**Combos**
- Table of all combos with member list and rotation policy
- Add / Edit form: drag-to-reorder member list (important for `priority` rotation)
- Delete with confirmation
- "Test" button: sends a minimal request through the combo and shows which provider was selected

**Request Logs**
- Paginated table: timestamp, model_in, provider_used, endpoint, status, latency, tokens
- Filter by provider, date range, endpoint type, status (errors only)
- Small chart: requests per hour (last 24h), stacked by provider

**Settings**
- Health cooldown duration
- Error codes that trigger rotation
- Log retention days (prune job runs nightly)
- Gateway API key display (masked, with "reveal" button)

**Export / Import**
- "Export SQL" button → downloads `gateway-export-YYYY-MM-DD.sql`
- "Import SQL" file picker → uploads, validates, applies, shows result
- Warning banner: "Import replaces all providers, combos, and settings"

### Dashboard API endpoints (`/dashboard/api/`)

All return JSON. All require session cookie.

```
GET    /dashboard/api/providers          → list
POST   /dashboard/api/providers          → create
PUT    /dashboard/api/providers/:id      → update
DELETE /dashboard/api/providers/:id      → delete
POST   /dashboard/api/providers/:id/test → test connection

GET    /dashboard/api/combos             → list
POST   /dashboard/api/combos             → create
PUT    /dashboard/api/combos/:id         → update
DELETE /dashboard/api/combos/:id         → delete
POST   /dashboard/api/combos/:id/test    → test combo

GET    /dashboard/api/logs               → paginated log query
GET    /dashboard/api/logs/chart         → hourly counts last 24h

GET    /dashboard/api/settings           → all settings
PUT    /dashboard/api/settings           → update settings

GET    /dashboard/api/export             → SQL export (download)
POST   /dashboard/api/import             → SQL import (upload)

GET    /dashboard/api/health             → per-provider health state
```

### Frontend Implementation Notes

- Vanilla JS (`fetch` + `innerHTML`/`insertAdjacentHTML`). No framework.
- Single `app.js` handles client-side routing by `location.hash` (`#providers`, `#combos`, etc.).
- Forms submit via `fetch` (JSON body), response updates the table in-place.
- Drag-to-reorder for combo members: HTML5 `draggable` API, no library needed.
- Log chart: use `<canvas>` with a tiny inline chart renderer (50 lines, no Chart.js dependency needed for a simple bar chart).
- Style: a single CSS file with CSS variables for theming. Dark mode via `prefers-color-scheme`. Minimal, functional, no framework needed.

---

## `/v1/models` Response Shape

```json
{
  "object": "list",
  "data": [
    {
      "id": "free-mix",
      "object": "model",
      "created": 1700000000,
      "owned_by": "gateway",
      "description": "Free Mix (Groq + Together + OpenRouter)"
    },
    {
      "id": "groq-llama3",
      "object": "model",
      "created": 1700000000,
      "owned_by": "gateway"
    }
  ]
}
```

Only `enabled: true` combos and providers appear. Combo IDs appear first.

---

## Error Handling & Failure Modes

| Scenario | Gateway Behavior |
|---|---|
| Upstream returns 429 | Rotate to next, apply cooldown to that provider |
| Upstream returns 500/502/503/504 | Rotate to next, apply cooldown |
| Upstream returns 404 on `/v1/completions` | Treat as failure, rotate (provider doesn't support legacy endpoint) |
| All members in combo exhausted | `502` with `{"error": {"message": "all upstreams failed", "type": "gateway_error"}}` |
| Provider cooldown active | Skip during selection; re-eligible after cooldown duration |
| Upstream timeout | Treat as failure, rotate |
| Auth failure on API | `401` immediately, no upstream call |
| Auth failure on dashboard | `302` redirect to `/dashboard/login` |
| Unknown model/combo | `404` with OpenAI-shaped error body |
| Request body > configurable limit | `413` |
| `/v1/responses` translation failure | `500` with explanation; log error |
| Import SQL invalid header | `400` with message; no DB change |
| Import SQL DB error | `500` with message; transaction rolled back |

---

## Weighted Round-Robin Implementation Note

For `weighted-round-robin`, use the **smooth weighted round-robin** algorithm (Nginx style). Each provider has a `current_weight` that increments by its `weight` each round; the one with highest `current_weight` is selected and then reduced by the total weight sum. This avoids burst clustering compared to naive weight expansion.

```
on each select:
  for each member:
    member.current += member.weight
  winner = member with max current_weight (skip if unhealthy)
  winner.current -= total_weight
  return winner
```

---

## Streaming Considerations

- Set `Transfer-Encoding: chunked` or let Go's `ResponseWriter` handle it.
- Use `http.Flusher` to flush each SSE chunk to the client immediately.
- Set upstream client `DisableCompression: true` to avoid gzip buffering.
- Forward `X-Accel-Buffering: no` so nginx/caddy upstreams don't buffer SSE.
- Per-chunk timeout: reset a deadline on each received chunk to detect stalled upstream.
- For `/v1/responses` streaming, translate each `choices[0].delta.content` chunk into a `response.output_text.delta` event before sending to caller.

---

## Docker / OCI Image

**Multi-stage Dockerfile:**

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /gateway ./cmd/gateway

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /gateway /gateway
ENTRYPOINT ["/gateway"]
```

Note: `modernc.org/sqlite` is pure Go and builds with `CGO_ENABLED=0` — no CGO toolchain needed.

**GitHub Actions (`release.yml`)** triggers on `v*` tag push:

```yaml
- uses: docker/setup-buildx-action@v3
- uses: docker/build-push-action@v5
  with:
    platforms: linux/amd64,linux/arm64
    push: true
    tags: ghcr.io/${{ github.repository }}:${{ github.ref_name }}
```

Image size target: ~18–25 MB (distroless static + stripped binary + embedded frontend assets + SQLite driver).

**`docker-compose.example.yml`:**

```yaml
services:
  gateway:
    image: ghcr.io/you/llm-gateway:latest
    ports:
      - "8080:8080"
    environment:
      GATEWAY_API_KEY: "gw-change-me"
      DASHBOARD_PASSWORD: "change-me"
      DASHBOARD_SECRET: "a-random-32-char-string-goes-here"
      DB_PATH: "/data/gateway.db"
    volumes:
      - gateway_data:/data
    restart: unless-stopped

volumes:
  gateway_data:
```

---

## Implementation Phases

### Phase 1 — Skeleton, DB & Auth (Day 1)
- [ ] `go mod init`, project layout, `cmd/gateway/main.go`
- [ ] `internal/store`: SQLite open, schema migration, basic CRUD stubs
- [ ] `internal/auth/api.go`: Bearer middleware
- [ ] `internal/auth/dashboard.go`: session cookie auth + login endpoint
- [ ] `GET /healthz` returning 200
- [ ] Dockerfile builds, runs, DB file persists across restarts

### Phase 2 — Single Provider Passthrough (Day 1–2)
- [ ] `internal/registry`: load providers + combos from DB into memory map
- [ ] `internal/proxy/proxy.go`: forward single provider call (no combo, no retry)
- [ ] `POST /v1/chat/completions` working end-to-end against one real provider
- [ ] Header rewrite (strip inbound auth, inject upstream auth + model)
- [ ] Log request to DB after response

### Phase 3 — Combos & Rotation (Day 2–3)
- [ ] Combo registry with all four rotation strategies
- [ ] `GET /v1/models` returning enabled combos + providers
- [ ] `internal/proxy/retry.go`: full rotation + retry loop
- [ ] `POST /v1/completions` legacy endpoint (with 404 fallback handling)
- [ ] `GET /readyz` + `GET /admin/status`

### Phase 4 — `/v1/responses` Support (Day 3)
- [ ] `internal/proxy/responses.go`: request translation (responses → chat)
- [ ] Response translation (chat → responses shape)
- [ ] `responses_native` provider flag in DB + registry
- [ ] `POST /v1/responses` route wired up with native/translated path
- [ ] Streaming translation for responses format
- [ ] Add `endpoint` column routing to log table

### Phase 5 — Streaming (Day 4)
- [ ] `internal/proxy/stream.go`: SSE passthrough for chat completions
- [ ] `StreamFormat` enum + responses stream translation
- [ ] Per-chunk flush + stall detection
- [ ] Early error detection before stream commits (retry window)

### Phase 6 — Health & Cooldown (Day 4)
- [ ] `internal/registry/health.go`: per-provider state + cooldown
- [ ] Wire failure detection into retry loop
- [ ] Configurable error codes and cooldown duration from DB settings
- [ ] `POST /admin/reload` registry hot-reload

### Phase 7 — Export / Import (Day 5)
- [ ] `internal/store/export.go`: `ExportSQL()` — generates INSERT dump
- [ ] `ImportSQL()` — validates header, runs in transaction, reloads registry
- [ ] `GET /admin/export` and `POST /admin/import` routes
- [ ] Import validation: reject files without gateway export header
- [ ] Test round-trip: export → wipe DB → import → verify providers/combos intact

### Phase 8 — Web Dashboard (Day 5–7)
- [ ] `internal/dashboard/static/`: `index.html`, `app.js`, `style.css`
- [ ] `go:embed` wiring, serve under `/dashboard/`
- [ ] Login page + session management
- [ ] Dashboard API: providers CRUD endpoints
- [ ] Dashboard API: combos CRUD with member ordering
- [ ] Dashboard API: settings read/write
- [ ] Dashboard API: logs query + hourly chart data
- [ ] Dashboard API: export/import endpoints
- [ ] Frontend: overview page with health table
- [ ] Frontend: providers page with add/edit/delete/test
- [ ] Frontend: combos page with drag-to-reorder members
- [ ] Frontend: logs page with filter + chart
- [ ] Frontend: settings page
- [ ] Frontend: export/import UI

### Phase 9 — Hardening & Polish (Day 7–8)
- [ ] Request body size limit middleware
- [ ] Per-request timeout context propagation
- [ ] Structured JSON logging (`log/slog`)
- [ ] Graceful shutdown (`context` + `http.Server.Shutdown`)
- [ ] Nightly log pruning goroutine (respects `log.retention_days` setting)
- [ ] `POST /v1/embeddings` passthrough (route directly to named provider)
- [ ] README with quickstart, env var reference, Portainer stack example
- [ ] `docker-compose.example.yml`

### Phase 10 — CI/CD (Day 8)
- [ ] GitHub Actions: `go test ./...` on PR
- [ ] GitHub Actions: multi-arch OCI build + push to ghcr.io on tag
- [ ] `go vet` + `staticcheck` in CI

---

## Dependencies Decision

| Package | Purpose | Rationale |
|---|---|---|
| `modernc.org/sqlite` | SQLite driver | Pure Go, CGO-free, distroless-compatible; single most important dep |
| `gopkg.in/yaml.v3` | (optional) YAML config | Only needed if keeping a yaml bootstrap file; can be dropped entirely since all config is now in DB + env vars |

Everything else — HTTP, JSON, logging (`log/slog`), context, sync, embed — is Go stdlib. The dashboard frontend uses zero npm packages.

---

## Future Extensions (out of scope for v1)

- **Persistent health state** — write cooldown state to DB so it survives restarts
- **Per-combo rate limiting** — protect callers from bursting the gateway
- **Prometheus metrics** — `GET /metrics` with request count, latency histogram, upstream error rate per provider
- **Multi-tenancy** — multiple API keys with different combo access scopes
- **Dashboard dark/light theme toggle** — currently follows `prefers-color-scheme` only
- **Provider auto-discovery** — query upstream `/v1/models` and auto-populate available model names
- **Webhook on provider failure** — POST to a URL when a provider enters cooldown
