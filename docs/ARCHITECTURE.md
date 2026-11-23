# Elitea Platform — Architecture

## Overview

`elitea-platform` is the Go monorepo for the cloud-native replatforming of
the legacy runtime's main service (Python/Flask) to `elitea-main` (Go). It follows a service + shared
library structure managed with Go workspaces.

## Repository Layout

```
elitea-platform/
├── services/elitea-main/   # Main API service (replaces the legacy main service)
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

Replaces the legacy main service: the central API gateway and domain logic service.

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

## Internal listeners and administration data

Administration data (provider, policy, identity and role data) is written
through elitea-main's public HTTP API only, where the authentication and
permission gates are (ADR-0059, decision 7, "Internal paths"). The scheduler
and the sub-application host read policy from PostgreSQL or receive it from
Main; none of their listeners writes administration data.

Each Go service keeps a reviewed allowlist of its internal listeners: every
gRPC method it serves and every internal HTTP route, with what the method
writes, what decides it and the caller identity it requires, plus every place
in the source that builds a listener or registers a gRPC service:

| Service | Allowlist |
|---------|-----------|
| elitea-main | `services/elitea-main/internal/transport/internal_listener_allowlist.txt` |
| elitea-scheduler | `services/elitea-scheduler/cmd/elitea-scheduler/internal_listener_allowlist.txt` |
| elitea-subapp-host | `services/elitea-subapp-host/internal/spi/internal_listener_allowlist.txt` |

A test beside each allowlist (run by `ci-go`) builds the service's internal
gRPC listeners through their real constructors and compares what
`grpc.Server.GetServiceInfo()` reports with the allowlist, and scans the
module's source for listener and registration sites, both ways: a new method,
service, listener or internal route fails until it is reviewed. It also fails
when:

- a method that can change administration data is decided by anything other
  than Main's own route, run in process with the caller's bearer
  (`main-route`): an execution claim or a workload certificate never
  authorizes an administration write;
- an internal handler names the `administration` permission mode. Internal
  handlers resolve permissions for a principal they build from an execution
  claim (a synthetic `auth.User`), always in the default (project) mode;
  resolving the administration mode there would pick up that user's
  elevation from inside the cluster.

A future internal RPC that changes administration data carries the principal
and the elevation id, Main makes the decision, and it follows ADR-0036 (gRPC
over mTLS, contract in `libs/proto`). The gateway and the Rust worker, outside
the Go workspace, have their own listener tests.

The internal methods that can change administration data, and the caller
identity each requires (the test compares this table with the allowlists):

<!-- internal-admin-writes:begin -->
| Service | Listener | Method | Caller identity |
|---------|----------|--------|-----------------|
| elitea-main | platform | `/elitea.runtime.v1.InternalToolService/CallTool` | `ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS` |
<!-- internal-admin-writes:end -->

`CallTool` runs `/app/{project}/mcp` in process with the caller's bearer: the
platform MCP tools include the project's configurations and secrets, so the
route's project-mode permission checks decide for the bearer's own principal.
The workload identity on the platform listener (`dns:` or `spiffe://`, from
`ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS`: the agent worker, and the Inventory
engine's callback identity) only admits the call to the service.

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
4. Deprecate the legacy main service once all routes are validated in production

See the cloud-native migration plan for the full 24-week roadmap.
