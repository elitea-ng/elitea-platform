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
module's source for listener and registration sites (with how often each
occurs) and internal mux routes, both ways: a new method, service, listener
or internal route fails until it is reviewed. Test-helper packages are
skipped only when the allowlist names them, and no scanned file may import
one. The test also fails when:

- a method declared to change administration data is decided by anything
  other than Main's own route, run in process with the caller's bearer
  (`main-route`): an execution claim or a workload certificate never
  authorizes an administration write;
- an internal handler, or any package of the module it imports, names the
  `administration` permission mode (a `*ModeAdministration` or
  `*AdministrationMode` identifier, or the literal), outside the files the
  allowlist reviews by name (the constant's definition, the resolver, the
  public-route middleware, enrollment). Internal handlers resolve permissions
  for a principal they build from an execution claim (a synthetic
  `auth.User`), always in the default (project) mode; resolving the
  administration mode there would pick up that user's elevation from inside
  the cluster.

What a method writes and what decides it are declared by the reviewer of its
allowlist line; the test cannot prove them. It proves that every method was
reviewed, that a method declared `admin` is decided by Main's route and
listed below, and the administration-mode rule above.

A future internal RPC that changes administration data carries the principal
and the elevation id, Main makes the decision, and it follows ADR-0036 (gRPC
over mTLS, contract in `libs/proto`).

The LLM gateway and the Rust worker are outside the Go workspace. Each has its
own allowlist and its own listener test, run by its own CI job (ADR-0059 R1.9):

| Service | Allowlist | Test | CI job |
|---------|-----------|------|--------|
| elitea-llm-gateway | `services/elitea-llm-gateway/internal/grpcapi/internal_listener_allowlist.txt` | `internal/grpcapi/surface_allowlist_test.go`, `internal/server/surface_allowlist_test.go` | `ci-gateway` |
| elitea-worker-rust | `services/elitea-worker-rust/src/sandbox/internal_listener_allowlist.txt` | `src/sandbox/surface_allowlist_tests.rs` | `ci-rust` |

- **The gateway** uses a copy of the Go checks above. Its test builds the
  `GatewayService` listener through `grpcapi.NewServer` and compares
  `GetServiceInfo()` with the allowlist. It also scans the module's source for
  listener sites and the routes of the health and metrics mux. The HTTP twin
  walks the `/llm` chi router (`chi.Walk`), which only the `GatewayService`
  RPCs reach. The gateway reads provider, policy and model data from
  PostgreSQL. Its calls write run and metering data only: the request log,
  budget counters and usage.
- **The worker** serves nothing in its default binary. The
  `elitea-sandbox-supervisor` binary serves `SandboxSupervisorService`,
  `BrowserSessionService` and `WorkspaceRunService`, one mTLS listener for
  each runtime profile. The test finds each tonic server module that the
  non-test source names, and the file that names it. It reads the routes of
  that server from the code that tonic generated, and compares each file and
  route with the allowlist. It also counts every
  listener and registration site in the non-test source. Each request carries
  a grant that Main signed for one run (`claim`). No method writes provider,
  policy, identity or role data.

Each allowlist line gives a reason: who calls the method and what it writes. A
fixture method, route or site that is not on the list makes each test fail.

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

## Admin audit role use and tamper evidence

<a id="admin-audit-role-use-tamper-evidence"></a>

Every administration-mode row of `centry.audit_events` (a request during which
the permission resolver answered the `administration` mode for the caller)
records the capacity it was written in (ADR-0059 decision 8, story R1.4a,
shared migration 0268): `acting_roles` (the caller's standing administration
roles, from the resolution the request already made), `elevation_id`,
`credential_kind` (`session`, `form`, `sealed`, `token`, `native`, `run`,
`system`, `scim`; NULL when unknown, never a guess), `central_override`,
`reason_code`, `reason` and `reason_missing`, and (story R2.8a, shared migration
0275) `elevation_flagged`, the review mark of an action the action table flags
under an elevation.

**Tamper evidence (R1.4b, issue 2576, shared migration 0276).** The chain's
canonical form is versioned. `elitea_runtime.audit_events_chain_forms` records,
per partition, the first chain position hashed in each form version; a
position below every recorded one is form v1 (shared 0206, unchanged).
`centry.audit_event_canonical` dispatches on the chain position (never the row
id) to `centry.audit_event_canonical_v1` or `_v2`, and both the chain trigger
and the verifier call it, so old links verify under the form they were hashed
in. Form v2 appends `acting_roles`, `elevation_id`, `credential_kind`,
`central_override`, `reason_code`, `reason_missing` and `elevation_flagged`
to v1's columns: on a row chained from the cut-over they are covered by the
hash and guarded by the append-only trigger (which now compares the form the
row was hashed in). On a row chained before the cut-over they are still
outside its hash, as they always were. The reason **text** is never hashed
and never exported: only the code leaves the row. The same fields (without
the text) are in the `elitea.audit.v1` export's `details` of an
`admin_audit` event.

## Admin audit coverage

<a id="admin-audit-coverage"></a>

Every administration mutation writes an admin audit row (ADR-0059,
Verification; story R1.7, issue 2511). The audit middleware
(`services/elitea-main/internal/api/middleware/audit.go`) still decides by path
prefix (`auditedSurfaces`), but the prefix list is no longer the proof: the
test `TestEveryAdministrationMutationWritesAnAuditRow`
(`services/elitea-main/internal/api/admin_mutation_audit_coverage_test.go`)
walks every write of the administration surface found by driving the composed
router (`internal/api/adminsurface`, every variant of the action table and the
Configuration sections), every write pair of the central override and every
write block of the separation matrix
(`internal/api/testdata/separation_matrix_writes.txt`). It drives each as a
caller who holds every administration permission and fails when the request
wrote no row, or wrote one without `acting_roles` or `credential_kind` (a
check that resolved with a detached context). A route mounted outside the
prefixes with an in-handler check, the shape a prefix list misses, fails it.

The reviewed exemption list is
`services/elitea-main/internal/api/testdata/admin_mutation_audit_exemptions.reviewed`:
one `METHOD pattern: reason` per line, each with a written reason, and an
entry that no longer matches a driven mutation fails the test. It is empty.
A handler that panics on an audited mutation still writes its row (status
500, or the status it had already written) before the panic reaches the
recoverer. The proof covers what the test can enumerate: the driven
administration surface (a route whose check the composed router cannot
reach is listed with a reason by the surface tests), the override pairs and
the matrix writes, on the success path.

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
