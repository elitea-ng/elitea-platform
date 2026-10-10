# Elitea Platform — Architecture

## Overview

`elitea-platform` is the Go monorepo for the cloud-native replatforming of
`pylon_main` (Python/Flask) to `elitea-main` (Go). It follows a service + shared
library structure managed with Go workspaces.

## Repository Layout

```
elitea-platform/
├── services/elitea-main/   # Main API service (replaces pylon_main)
├── libs/go/                # Shared Go libraries
│   ├── authlib/            # Authentication primitives
│   ├── eventslib/          # Domain event types & publisher interface
│   ├── rpclib/             # Inter-service HTTP RPC helpers
│   └── observability/      # OTel SDK setup
├── libs/proto/             # Protobuf definitions (internal gRPC contracts)
├── deploy/                 # Kubernetes/Helm, docker-compose, ArgoCD
├── tools/scripts/          # Developer tooling
└── .github/workflows/      # CI pipelines
```

## Services

### elitea-main

Replaces `pylon_main`: the central API gateway and domain logic service.

**Responsibilities:**
- HTTP API (chi router)
- Authentication / RBAC enforcement
- Domain logic: applications, conversations, predict, analytics, toolkits,
  publishing, admin, indexer integration
- Tenant-aware PostgreSQL access (schema-per-tenant)
- NATS (JetStream) for the runtime command bus, live updates and presence
- SSE streaming for long-running operations

**Ports:**
- `:8080` — HTTP API
- `/healthz` — liveness probe
- `/readyz` — readiness probe
- `/startupz` — startup probe

## Shared Libraries

| Library | Purpose |
|---------|---------|
| `authlib` | API key verification, JWT validation, `Principal` type |
| `eventslib` | Typed domain events, `Publisher` interface |
| `rpclib` | Internal service-to-service HTTP clients |
| `observability` | OTel TracerProvider / MeterProvider bootstrap |

## Dependency Decisions

| Technology | Choice | Rationale |
|-----------|--------|-----------|
| HTTP Router | `go-chi/chi` | Lightweight, stdlib-compatible, excellent middleware ecosystem |
| PostgreSQL | `jackc/pgx/v5` | Best performance, native PostgreSQL protocol support |
| Messaging | `nats-io/nats.go` | NATS core + JetStream; replaced Redis (removed) |
| Tracing/Metrics | `go.opentelemetry.io/otel` | Vendor-neutral, CNCF standard |
| Query generation | `sqlc` | Type-safe SQL, no ORM overhead |

## Multi-Tenancy

Each tenant maps to a PostgreSQL schema. The `infra/db/tenant` package provides
a `Router` that sets `search_path` per request, routing queries to the correct
schema transparently.

## Migration Strategy

The migration from Python/Flask proceeds domain by domain:

1. Infrastructure layer (DB pool, NATS, health probes) — **done**
2. Auth middleware — in progress
3. Domain services (one per sprint)
4. Deprecate `pylon_main` once all routes are validated in production

See the cloud-native migration plan for the full 24-week roadmap.
