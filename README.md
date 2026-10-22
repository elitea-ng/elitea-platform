# Elitea AI

Elitea AI platform Enterprise grade business harnessing LLMs for secure and scalable AI applications.

## Architecture

```
elitea-platform/
├── services/
│   ├── elitea-llm-gateway/  # LLM gateway service
│   ├── elitea-main/         # Go API server (chi/v5, pgx/v5, nats.go)
│   ├── elitea-scheduler/    # Background workers: price sync, budget write-back, retention sweeps (Go)
│   └── elitea-worker-python/# Python worker runtime and SDK
├── apps/
│   ├── elitea-desktop/      # Desktop app (Tauri 2 host around the elitea-web desktop build)
│   ├── elitea-mobile/       # Mobile app "Elitea AI" (Expo / React Native, iOS + Android)
│   └── elitea-web/          # React SPA (the UI); also builds the admin console
│                            #   (src/entries/admin) served by elitea-main
├── libs/proto/              # Protobuf contracts (buf v2) and their generated Go/Python stubs
├── libs/go/                 # Shared Go libraries
├── deploy/
│   ├── docker/              # Helper scripts and configs for container images
│   ├── docker-compose.yml   # Local dev environment (legacy shape, Go services)
│   ├── docker-compose.standalone-full.yml  # Target-architecture stack (Go + Bifrost, no pylon)
│   └── helm/                # Kubernetes Helm charts
└── .github/workflows/       # CI/CD pipelines
```

## Prerequisites

- [Go 1.26.9+](https://go.dev/dl/)
- [Podman](https://podman.io/docs/installation) with podman-compose
- [Task](https://taskfile.dev/installation/) (task runner)
- [Node.js 24+](https://nodejs.org/) (for UI development)
- [Helm 3.16+](https://helm.sh/docs/intro/install/) (for chart linting)

## Quick Start

```bash
git clone https://github.com/elitea-ng/elitea-platform.git
cd elitea-platform

# Start everything (postgres, nats, rustfs, elitea-main, elitea-web)
task up

# Or manually:
podman compose -f deploy/docker-compose.yml up --build
```

elitea-main is the browser edge of the local compose stack, on host port `8080`:
it serves the API itself, relays `/app` and `/docs` to the SPA container and
redirects `/` to `/app/` (`ELITEA_WEB_UPSTREAM`; compose only, no Traefik).

Services will be available at:

| Service | URL | Description |
|---------|-----|-------------|
| Browser origin | http://localhost:8080 | elitea-main: API, plus the SPA relayed |
| API | http://localhost:8080/api/ | elitea-main API endpoints |
| UI | http://localhost:8080/app/ | elitea-web React SPA |
| postgres | localhost:5432 | PostgreSQL 18 (no extensions needed) |

### DeepWiki on the dev stack (`deepwiki` profile)

DeepWiki is off by default. Turn it on with one command:

```bash
bash deploy/scripts/gen-deepwiki-certs.sh           # provider CA, host and client certs
bash deploy/scripts/gen-platform-listener-certs.sh  # :9446 listener, callback identity, elitea-vector certs
ELITEA_DEEPWIKI_ENABLED=true \
ELITEA_DEEPWIKI_PLATFORM_GRPC_TARGET=elitea-deepwiki:9443 \
ELITEA_RUNTIME_PLATFORM_ADDRESS=:9446 \
podman compose -f deploy/docker-compose.yml --profile deepwiki up -d
```

`ELITEA_RUNTIME_PLATFORM_ADDRESS` opens elitea-main's platform gRPC listener
(mTLS, compose network only). The DeepWiki host stores its artifacts there,
and the engine makes its model calls there. The profile also brings up
`qdrant` and `elitea-vector`, where the engine keeps its dense vectors;
elitea-vector verifies the engine's tokens on that listener. This stack has **no LLM gateway**,
so a model call still fails at elitea-main. Artifacts work, and
`DEEPWIKI_NATIVE_RUNNER=fixture` gives a canned wiki. To generate a wiki with
a real model, use `deploy/docker-compose.standalone-full.yml`, below.
A model call on this stack fails with `llm_gateway_not_configured` ("LLM_GATEWAY_URL
is empty"), which the engine reports on the invocation.

This stack also does not migrate the platform database. elitea-main runs with
`SKIP_MIGRATIONS=1`, and the schema comes from a loaded dump
(`deploy/docker-compose.staging.yml`). On an empty database elitea-main starts,
but projects and toolkits, DeepWiki's included, have no tables behind them.
`elitea-deepwiki-migrate` applies DeepWiki's own schema either way.

## Standalone full stack

`deploy/docker-compose.yml` above is the older local shape and keeps the
legacy database and routing conventions. `deploy/docker-compose.standalone-full.yml`
is a separate, self-contained recipe for the **target** architecture: Go `elitea-main` +
`elitea-web` + the Bifrost `elitea-llm-gateway` + postgres/nats/rustfs
+ a mock OIDC provider — no pylon, no centry, no LiteLLM.

```bash
deploy/scripts/standalone-stack.sh certs   # once — local mTLS material (deploy/certs/, gitignored)
deploy/scripts/standalone-stack.sh up
deploy/scripts/standalone-stack.sh seed
deploy/scripts/standalone-stack.sh seed-runtime
OPENAI_API_KEY=sk-... deploy/scripts/standalone-stack.sh seed-llm   # optional: a real provider credential
deploy/scripts/standalone-stack.sh check   # asserts the stack; read its last line

open http://localhost:8084/app/
```

The stack runs the native Rust agent worker by default; `STANDALONE_WORKER=python` selects
the legacy Python worker (until it is removed). Anything the Rust worker cannot serve for
an agent (an unported toolkit, a refused toolkit setting, an unavailable built-in
capability) is named in a notice on that reply in the chat.

`check` prints one line for each assertion. Its last line reports how many
passed, how many failed, how many were skipped, and how many of the expected
assertions reported a result at all. A skipped assertion exits non-zero unless
you pass `--allow-skips`, and a run that reports fewer results than expected
exits non-zero even with that flag. Read that line rather than the exit status
alone.

Or via Task: `task standalone:up` / `task standalone:down`.

Compose project `elitea-standalone`. Ports: `8084` entry (elitea-main, the browser edge), `8085`
gateway (direct, debug), `15433` postgres, `9400` oidc-mock
(**fixed** — the mock's issuer is derived from its Host header, so the port
cannot be remapped). Because of that fixed port, this stack and the E2E stack
(`apps/elitea-web/scripts/e2e-stack.sh`, also on 9400 by default) cannot run at
the same time; `standalone-stack.sh up` checks for the conflict and tells you
to stop the other one first.

**What works:** everything the E2E stack does, plus real chat completions
through the gateway at `POST /api/v2/.../llm/v1/chat/completions` (OpenAI
dialect) and `/llm/v1/messages` (Anthropic dialect), billed against the
provider credential you seed with `seed-llm`.

**What still does NOT work:** agent execution (the chat box "Send" button) is
a separate path from the `/llm` passthrough and is not enabled here. It needs
all of: `services/elitea-worker-python` running as the NodeEvent producer (Go
has no native agent-token producer — it only projects and serves the events);
`ELITEA_RUNTIME_ENABLED=true`, which requires the JetStream command bus and a full runtime PKI (
four gRPC server certs + a signing keyring); `cfg.RuntimeRoutes` to be wired in
`cmd/elitea-main/main.go` so `GET /api/v2/executions/{projectID}/{executionID}/events`
is actually mounted; and the web chat surface to subscribe to that SSE stream
instead of the current noop socket.io client. This is a separate, larger
effort — do not wire it piecemeal.

## Common Tasks

```bash
task up              # Start full local environment (foreground)
task up:detach       # Start in background
task down            # Stop all services
task down:clean      # Stop and remove volumes
task logs            # Tail logs from all services

task build           # Build Go binaries
task test            # Run all Go tests
task lint            # Run golangci-lint
task vet             # Run go vet

task ui:build        # Build the elitea-web SPA
task ui:lint         # Lint elitea-web

task images          # Build all container images
task images:go       # Build elitea-main image only
task images:scheduler # Build elitea-scheduler image only
task images:gateway  # Build elitea-llm-gateway image only
task images:web      # Build elitea-web image only

task helm:lint       # Lint all Helm charts
task all             # Run all checks
```

## Services

### elitea-main (Go)

The core platform API. Handles authentication, project management, prompt library, and orchestrates communication between frontend and agent runtime.

```bash
cd services/elitea-main
go run ./cmd/elitea-main     # Run directly
go run ./cmd/cutover-ctl     # LLM-path cutover verification gates
go run ./cmd/elitea-auth-validate -form-users-file /absolute/private/form-users.json
                             # Validate a resolved Form snapshot; success is silent
```

The validator is also shipped as `/elitea-auth-validate` in the `elitea-main`
image for a preflight/init-container command override. Its input must be a
nonempty canonical absolute regular file, no larger than 1 MiB, with owner-only
permissions (`0400` or `0600`) and no symlinked path component. It
prints only fixed generic failures and never prints the file path or contents.

**Environment variables:**

| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_URL` | — | PostgreSQL connection string |
| `ELITEA_EVENTS_NATS_URL` | — | NATS (JetStream) for live updates: project SSE stream and canvas presence (domain events go to webhooks only). Unset disables them; set but unreachable stops startup |

### The index plane, and the service that used to serve it

`pylon-indexer` was the transitional Pylon runtime: it cloned its plugins at
container start and served index ingest, provider descriptors and agent
execution. It is **deleted** (issue #339). The Go runtime plane dispatches index
ingest to `elitea-worker-python` on the same command stream and consumer group
as agent execution, so one worker serves both capabilities and no plugin is
loaded at run time.

`runtime_engine_litellm`, the LiteLLM proxy that ran inside that container, had
already gone (issue #323): it was a second LLM data plane with no budget and no
billing. The LLM data plane is `elitea-llm-gateway`, reached through
`elitea-main` at `/llm/v1`.

`deploy/INDEX_V2_CUTOVER.md` holds the cutover procedure and the rollback.

### elitea-web (React)

React SPA served by nginx (`apps/elitea-web/Containerfile`); the same app also
builds the admin console, the maintenance page and the embedded docs.

```bash
cd apps/elitea-web
npm ci
npm run dev          # Local dev server
npm run build        # Production build
```

## CI/CD

All workflows are in `.github/workflows/`:

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `ci-go.yml` | PR/push to Go paths | Lint, test, build validation |
| `ci-web.yml` | PR/push to UI paths | Typecheck, lint, unit tests, static gates, build SPA |
| `helm-lint.yml` | PR/push to helm paths | Lint charts, template dry-run |
| `publish.yml` | Push to main/next | Semantic release → build → publish-image (scan, manifest, sign) → chart |

### Release Flow

```
commit → main     → semantic-release → v1.2.0 → build images → sign with cosign
commit → next     → semantic-release → v1.3.0-rc.1 → build RC images
```

- **Versioning:** [Conventional Commits](https://www.conventionalcommits.org/) via semantic-release
- **Images:** Multi-arch (amd64 + arm64), native builds on platform-specific runners (Docker in CI, Podman locally)
- **Signing:** Keyless cosign via Sigstore OIDC
- **Rollback:** On any build/sign failure, the release, tag, and images are automatically deleted
- **Registry:** `ghcr.io/elitea-ng`

### Commit Convention

```
feat: add new endpoint        → minor bump (1.x.0)
fix: correct validation       → patch bump (1.0.x)
feat!: breaking change        → major bump (x.0.0)
chore: update deps            → no release
```

## Development

### Go Workspace

The repo uses `go.work` to manage multiple modules:

```bash
go work sync         # Sync workspace
go test ./...        # Test everything
```

### Adding a New Service

1. Create directory under `services/<name>/`
2. Add Containerfile
3. Add to `docker-bake.hcl` and `deploy/docker-compose.yml`
4. Add CI workflow in `.github/workflows/`
5. Add to `publish.yml` image matrix

### Helm Deployment

```bash
helm template my-release deploy/helm/elitea-main/ \
  -f deploy/helm/elitea-main/values-staging.yaml

helm upgrade --install elitea-main deploy/helm/elitea-main/ \
  -f deploy/helm/elitea-main/values-staging.yaml \
  -n elitea --create-namespace
```

## License

Apache License 2.0
