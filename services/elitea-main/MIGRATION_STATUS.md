# elitea-main Migration Status

Last updated: 2026-09-07

> **The pylon bridges this document describes are DELETED (issue #383).**
>
> Everything below is a HISTORICAL record of what each migration phase built.
> Five of the things it names no longer exist in the tree, and no deployment
> ever ran them:
>
> | Deleted | Was |
> |---------|-----|
> | `deploy/rpc-bridge/bridge.py` | the last writer of the pickle wire format `AGENTS.md` bans |
> | `internal/api/shadow/` | the comparator that mirrored requests to pylon and diffed the answers, plus `/internal/shadow` |
> | `internal/cutover/{router,tracker,inventory,gates,decommission}.go` | the reverse proxy to `LEGACY_URL`, the Redis endpoint-state machine and `/internal/cutover` |
> | `internal/infra/authsvc/rpc.go` | the Redis remote-call client for `pylon_auth:rpc:request` |
> | `internal/compat/rpcbridge/` | the adapter that wrapped that client |
>
> The environment variables in the sections below — `SHADOW_ENABLED`,
> `SHADOW_LEGACY_URL`, `SHADOW_WEIGHT`, `LEGACY_URL`, `CANARY_WEIGHT` — are gone
> from every deploy file with them. No Go code ever read them.
>
> `cmd/cutover-ctl` is KEPT, without its endpoint-promotion subcommands: the
> seven verification gates it still carries (`cost-parity`, `models-parity`,
> `budget-check`, `overhead-check`, `sse-flush-check`, `budget-status-audit`,
> `cutover-verify`) drive real deployments and are the LiteLLM-to-Bifrost
> acceptance harness. `status`, `summary`, `promote`, `promote-all`, `rollback`
> and `decommission-check` were clients of the two deleted mounts.
>
> `internal/cutover/index_v2_preflight.go` STAYS. It is a different cutover —
> the index capability v1-to-v2 move from issues #337/#339 — and it is live:
> `cmd/index-v2-preflight`, `internal/infra/db/repos/index_v2_cutover.go` and
> `internal/transport/redisdispatch/index_v2_cutover.go` all call it.
>
> `internal/api/TestNoPylonBridgeWiringReturns` fails if any of the deleted
> wiring comes back.

## Phase Summary

| Phase | Name | Status | Loop Command |
|-------|------|--------|--------------|
| 0 | Auth, Infra, Contracts | ✅ Complete | `migrate-phase-0-infra`, `migrate-phase-0-contracts` |
| 1 | CRUD + Events + Streaming | ✅ Complete | `migrate-phase-1-crud`, `migrate-phase-1-events`, `migrate-phase-1-streaming` |
| 2 | Shadow Mode | ✅ Complete | `migrate-phase-2-shadow` |
| 3 | Cutover Tooling | ✅ Complete | `migrate-phase-3-cutover` |
| 4 | Endpoint-by-endpoint migration | ✅ Complete | `migrate-phase-4-migrate` |
| 5 | Full cutover & decommission | ✅ Complete | `migrate-phase-5-decommission` |

---

## Phase 0: Auth, Infra, Contracts ✅

### Delivered
- **Auth middleware** (`internal/api/middleware/auth.go`) — Traefik forward-auth headers + Bearer/Basic decode + Redis cache (SHA-256 key, 60s TTL) + RPC delegation to pylon_auth
- **RBAC middleware** (`internal/api/middleware/rbac.go`) — permission expansion (dot-separated prefixes) + set intersection
- **Auth RPC client** (`internal/infra/authsvc/rpc.go`) — Redis pub/sub RPC to pylon_auth, 2s timeout, reply channels
- **Event bus** (`internal/infra/redis/events.go`) — publish/subscribe with typed Event struct
- **Health handler** (`internal/api/health/handler.go`) — /healthz, /readyz, /startupz with dependency checking
- **OTel middleware** (`internal/api/middleware/otel.go`) — request duration histogram, total counter, active gauge
- **Shadow comparator** (`internal/api/shadow/shadow.go`) — forward to legacy, JSON diff, latency comparison
- **API errors** (`pkg/apierr/apierr.go`) — typed APIError with status dispatch
- **Domain types** — applications, conversations, predict, analytics, toolkits, publishing, admin, indexer

### Tests (all passing)
- `internal/api/middleware/` — 15 tests (auth: 6, rbac: 7, otel: 2)
- `internal/infra/authsvc/` — 5 integration tests
- `internal/api/health/` — 6 tests
- `internal/infra/redis/` — 2 integration tests
- `internal/api/shadow/` — 6 tests
- `internal/domain/applications/` — 4 tests

---

## Phase 1: CRUD + Events + Streaming ✅

### CRUD Handlers (all at `/api/v2/projects/{projectID}/...`)
| Domain | Path | Methods | Repo impl |
|--------|------|---------|-----------|
| applications | `/applications` | GET, POST, GET/:id, PUT/:id, DELETE/:id, versions | `infra/db/repos/applications.go` |
| skills | `/skills` | GET, POST, GET/:id, PUT/:id, DELETE/:id | `infra/db/repos/skills.go` |
| folders | `/folders` | GET, POST, PUT/:id, PATCH/:id, DELETE/:id | `infra/db/repos/folders.go` |
| tags | `/tags` | GET (aggregated from applications) | `infra/db/repos/tags.go` |
| analytics | `/analytics` | GET /, /agents, /tools, /users | `infra/db/repos/analytics.go` |
| conversations | `/conversations` | GET, POST, GET/:id, PUT/:id, DELETE/:id, GET/:id/messages | `infra/db/repos/conversations.go` |

### Event System
- **Publisher** (`internal/events/publisher.go`) — typed constants, project-scoped Redis channels
- **Webhook handler** (`internal/api/webhook/handler.go`) — CRUD for webhook registrations
- **Webhook dispatcher** (`internal/api/webhook/dispatcher.go`) — event→HTTP delivery with HMAC-SHA256
- **Webhook repo** (`infra/db/repos/webhooks.go`) — full pgx impl with ListByEvent
- **SSE events** (`internal/api/v2/events/handler.go`) — real-time project event stream with heartbeat

### Streaming Handlers
| Domain | Path | Mode |
|--------|------|------|
| predict | `POST /predict` | JSON or SSE (stream flag) |
| predict/llm | `POST /predict/llm` | JSON or SSE |
| chat | `POST /chat/{id}/messages` | JSON or SSE |
| pipelines | `POST /pipelines/run`, `GET /pipelines/{id}/status`, `POST /pipelines/{id}/cancel` | async with polling |

### Interfaces not yet implemented (need RPC bridge)
- `Predictor` — delegates to pylon_indexer
- `LLMService` — delegates to pylon_indexer
- `ChatService` — delegates to pylon_indexer
- `PipelineRunner` — delegates to pylon_indexer

### Tests added
- `internal/api/v2/applications/` — 7 tests
- `internal/api/v2/predict/` — 4 tests
- `internal/api/webhook/` — 3 tests
- `internal/events/` — 2 tests

---

## Phase 2: Shadow Mode ✅

### Delivered
- **Metrics collector** (`internal/api/shadow/metrics.go`) — ring buffer, aggregate stats (match rate, latency, errors)
- **Admin API** at `/internal/shadow/`:
  - `GET /config` — current shadow config
  - `PUT /config` — dynamic enable/disable, weight, legacy URL
  - `GET /stats` — aggregate match rates
  - `GET /results?limit=N` — recent comparison results
  - `POST /reset` — clear metrics
- **Enhanced middleware** — weighted sampling (crypto/rand), metrics integration, body snapshot for async
- **Env config** — `SHADOW_ENABLED`, `SHADOW_LEGACY_URL`, `SHADOW_WEIGHT`

### Tests added
- Shadow metrics: 4 tests
- Shadow admin: 4 tests
- Total shadow tests: 15

---

## Phase 3: Cutover Tooling ✅

### Delivered
- **Indexer RPC client** (`internal/infra/indexersvc/rpc.go`) — Redis pub/sub RPC bridge to pylon_indexer
  - Implements `Predictor`, `LLMService`, `ChatService`, `PipelineRunner` interfaces
  - Single-response RPC (`call`) + streaming RPC (`callStream`) patterns
  - Configurable timeout (30s default, 120s for streams)
  - Same request/reply channel pattern as authsvc
- **Cutover tracker** (`internal/cutover/tracker.go`) — per-endpoint migration state machine
  - States: legacy → shadow → canary → go
  - Stored in Redis hash (`elitea:cutover:endpoints`)
  - Set/Get/List/Summary operations
- **Cutover admin API** at `/internal/cutover/`:
  - `GET /` — list all endpoint states
  - `GET /summary` — count by backend state
  - `PUT /` — set endpoint state (path, backend, updated_by)
- **Full wiring** — indexerClient serves all four interfaces, cutover tracker mounted in router

---

## Phase 4: Endpoint-by-Endpoint Migration ✅

### Delivered
- **Cutover routing middleware** (`internal/cutover/router.go`) — per-endpoint traffic routing
  - Reads state from tracker, routes to Go handler or reverse-proxies to legacy
  - Canary weight: crypto/rand-based weighted sampling
  - Dynamic weight adjustment via `SetCanaryWeight()`
  - Path normalization: maps concrete URLs to endpoint patterns (UUIDs → `{id}`)
- **Endpoint inventory** (`internal/cutover/inventory.go`) — 23 known API endpoints
  - `SeedDefaults()` called at startup, idempotent (won't overwrite existing states)
- **Contract tests** (`tests/contract/contract_test.go`) — endpoint parity validation
  - Compares status codes and JSON schema (top-level keys) between Go and legacy
  - Gated by `CONTRACT_AUTH_TOKEN` env var (skips gracefully if absent)
  - Covers: applications, skills, folders, tags, conversations, analytics, predict, healthz
- **Helm chart** (`deploy/helm/elitea-main/`) — full Kubernetes deployment
  - Deployment with rolling update (maxSurge:1, maxUnavailable:0)
  - HPA (2-10 replicas, CPU/memory targets)
  - PDB (minAvailable: 1)
  - ConfigMap with all env vars
  - Staging values override (`values-staging.yaml`)
- **Env config** — `LEGACY_URL` (enables cutover router), `CANARY_WEIGHT`

### Tests added
- `internal/cutover/` — 5 tests (normalize, go-state, legacy-state, canary-state, weight)

---

## Deployment Readiness

### Can run now
- Health endpoints (/healthz, /readyz)
- All CRUD endpoints (if Postgres schema matches)
- Shadow comparison against legacy
- Webhook management
- SSE event streaming
- Shadow admin API

### Now available (via indexer RPC bridge)
- Predict/LLM (delegates to pylon_indexer via Redis RPC)
- Chat (delegates to pylon_indexer via Redis RPC)
- Pipelines (delegates to pylon_indexer via Redis RPC)
- Cutover tracking admin API (/internal/cutover/)

### Required environment
```
DATABASE_URL=postgres://...
REDIS_URL=host:6379
SHADOW_ENABLED=true|false
SHADOW_LEGACY_URL=http://pylon-main:8000
SHADOW_WEIGHT=0.1
LEGACY_URL=http://pylon-main:8000    # enables cutover routing middleware
CANARY_WEIGHT=0.1                     # fraction of canary traffic to Go (0.0-1.0)
```

### Image
```bash
docker build -t ghcr.io/eliteaai/elitea-main:shadow-v0.1 .
```

---

## File Layout (key files)

```
cmd/elitea-main/main.go              — entrypoint, wiring
internal/api/router.go               — chi router, all mounts
internal/api/middleware/              — auth, rbac, otel, project, ratelimit, recover, requestid
internal/api/shadow/                  — comparator, middleware, metrics, admin
internal/api/health/                  — health checks
internal/api/webhook/                 — webhook CRUD + dispatcher
internal/api/v2/applications/        — handler
internal/api/v2/skills/              — handler
internal/api/v2/folders/             — handler
internal/api/v2/tags/                — handler
internal/api/v2/analytics/           — handler
internal/api/v2/conversations/       — handler
internal/api/v2/events/              — SSE stream handler
internal/api/v2/predict/             — predict + LLM handler
internal/api/v2/chat/                — chat handler
internal/api/v2/pipelines/           — pipeline handler
internal/api/generated/              — oapi-codegen ServerInterface (84 methods)
internal/api/oapiserver/             — full implementation of ServerInterface
internal/compat/rpcbridge/           — Redis RPC → domain port adapters
internal/domain/                     — type definitions per domain
internal/events/                     — publisher, event constants
internal/infra/authsvc/              — Redis RPC to pylon_auth
internal/infra/indexersvc/           — Redis RPC to pylon_indexer (predict/chat/pipelines)
internal/infra/db/repos/             — pgx repository implementations
internal/infra/redis/                — event bus
internal/cutover/                    — tracker, routing middleware, inventory, gates, decommission
cmd/cutover-ctl/                     — CLI for endpoint promotion/rollback/decommission
tests/contract/                      — integration contract tests (Go vs legacy)
deploy/helm/elitea-main/             — Helm chart (deployment, HPA, PDB, configmap)
internal/auth/                       — shared auth.User type
pkg/apierr/                          — typed API errors
pkg/ssewriter/                       — SSE helper
```

---

## Phase 5.5: Full API Contract (OpenAPI ServerInterface) ✅

### Delivered
- **OpenAPI 3.0.3 spec** (`api/openapi/v2.yaml`) — 84 operations matching SPA contract
- **oapi-codegen ServerInterface** (`internal/api/generated/api.gen.go`) — 5,524 lines, typed handlers
- **oapiserver** (`internal/api/oapiserver/`) — **84/84 methods implemented, 0 unconditional stubs**
  - `applications.go` — 5 methods (CRUD via Repository)
  - `skills.go` — 2 methods (List, Create via Repository)
  - `tags.go` — 1 method (List via Repository)
  - `folders.go` — inherited from existing
  - `conversations.go` — 4 methods (entity settings via Repository)
  - `analytics.go` — 7 methods (all KPIs via Repository)
  - `versions.go` — 12 methods (version lifecycle + relations)
  - `publishing.go` — 12 methods (publish/export/import/fork/public apps)
  - `artifacts.go` — 9 methods (filesystem-backed S3-compatible storage)
  - `admin.go` — 7 methods (roles, users, moderation — direct PostgreSQL)
  - `misc.go` — 25 methods (chat config, categories, settings, icons, search, toolkits, groups, etc.)
- **LocalValidator permissions** — closed the last gap vs pylon_auth RPC (loads permissions from role_permission table)
- **Port interfaces** (`internal/domain/predict/ports.go`, `conversations/ports.go`, `toolkits/ports.go`)
- **RPC bridge adapters** (`internal/compat/rpcbridge/indexer/`, `auth/`) — swap point for future gRPC
- **RouterConfig restructured** — `AuthDeps` + `IndexerDeps` sub-structs for service boundary visibility

### pylon_auth dependency status: ELIMINATED
LocalValidator now loads both roles AND permissions from PostgreSQL directly. The Redis RPC path (`authsvc.Client`) is a dormant fallback — no longer required for any endpoint.

### Service separation readiness
| Service | Status | Effort |
|---------|--------|--------|
| elitea-auth | Ready to extract (Go impl 100%) | Medium |
| elitea-gateway | Proto defined, needs LiteLLM wrapping | Medium |
| elitea-indexer-grpc | Protocol upgrade (Redis→gRPC) | Medium |
| elitea-artifacts | Implemented (filesystem), extract to MinIO/S3 | Medium |
| elitea-analytics | Needs CQRS event pipeline | Large |

---

## Phase 5: Full Cutover & Decommission ✅

### Delivered
- **cutover-ctl** (`cmd/cutover-ctl/main.go`) — CLI for endpoint lifecycle management
  - `status` — tabular view of all endpoint states
  - `summary` — counts per state + migration progress percentage
  - `promote <pattern>` — advance endpoint to next state with readiness gates
  - `promote-all` — batch promote all eligible endpoints
  - `rollback <pattern>` — move endpoint back one state
  - `decommission-check` — verify all endpoints migrated, print decommission steps
  - `--force` flag to skip readiness gates
- **Readiness gates** (`internal/cutover/gates.go`) — pre-promotion safety checks
  - Health check gate
  - Shadow match rate gate (≥95% required for canary/go promotion)
  - Error rate gate (≤1% for go promotion)
  - Configurable thresholds via `GateConfig`
- **Decommission report** (`internal/cutover/decommission.go`) — automated decommission plan
  - `GET /internal/cutover/decommission` — JSON report with readiness + step list
  - Returns 200 if ready, 412 Precondition Failed if endpoints remain
  - 10-step decommission procedure with kubectl commands

---

## Migration Runbook

### Prerequisites
- elitea-main deployed alongside pylon_main in same namespace
- Both services share Redis and Postgres
- `LEGACY_URL` set to pylon_main service URL

### Step 1: Deploy in Shadow Mode
```bash
helm install elitea-main deploy/helm/elitea-main/ \
  -f deploy/helm/elitea-main/values-staging.yaml -n elitea
```

### Step 2: Verify Shadow Parity
```bash
# Check shadow match rate
curl http://elitea-main:8080/internal/shadow/stats

# Run contract tests
CONTRACT_GO_URL=http://elitea-main:8080 \
CONTRACT_LEGACY_URL=http://pylon-main:8000 \
CONTRACT_AUTH_TOKEN=$TOKEN \
go test ./tests/contract/ -v
```

### Step 3: Promote to Canary (per-endpoint)
```bash
# Promote CRUD endpoints first (lowest risk)
cutover-ctl promote applications
cutover-ctl promote skills
cutover-ctl promote folders
cutover-ctl promote tags

# Check status
cutover-ctl summary
```

### Step 4: Increase Canary Weight
```bash
kubectl set env deployment/elitea-main CANARY_WEIGHT=0.5
# Monitor error rates, then:
kubectl set env deployment/elitea-main CANARY_WEIGHT=1.0
```

### Step 5: Promote to Go
```bash
# Once canary is stable at 100%
cutover-ctl promote-all
cutover-ctl summary
```

### Step 6: Decommission Legacy
```bash
cutover-ctl decommission-check
# Follow the printed steps
```

### Rollback (any stage)
```bash
# Single endpoint
cutover-ctl rollback applications

# All endpoints back to legacy
cutover-ctl rollback all
```
