# Elitea AI

Elitea AI platform Enterprise grade business harnessing LLMs for secure and scalable AI applications.

## Architecture

```
elitea-platform/
├── services/
│   ├── elitea-llm-gateway/  # LLM gateway service
│   ├── elitea-main/         # Go API server (chi/v5, pgx/v5, go-redis/v9)
│   ├── elitea-scheduler/    # Scheduled job runner (Go, cron + Redis RPC)
│   └── elitea-worker-python/# Python worker runtime and SDK
├── apps/
│   ├── elitea-ui/           # React SPA (git submodule)
│   └── elitea-web/          # React SPA rewrite; also builds the admin console
│                            #   (src/entries/admin) served by elitea-main
├── proto/                   # Protobuf contracts (buf v2) → gen/go, gen/python
├── gen/                     # Generated protobuf stubs (Go + Python)
├── libs/go/                 # Shared Go libraries
├── deploy/
│   ├── docker/              # Containerfiles for UI and helper images
│   ├── docker-compose.yml   # Local dev environment (legacy shape, Go services)
│   ├── docker-compose.standalone-full.yml  # Target-architecture stack (Go + Bifrost, no pylon)
│   └── helm/                # Kubernetes Helm charts
└── .github/workflows/       # CI/CD pipelines
```

## Prerequisites

- [Go 1.25+](https://go.dev/dl/)
- [Podman](https://podman.io/docs/installation) with podman-compose
- [Task](https://taskfile.dev/installation/) (task runner)
- [Node.js 24+](https://nodejs.org/) (for UI development)
- [Helm 3.16+](https://helm.sh/docs/intro/install/) (for chart linting)

## Quick Start

```bash
# Clone with submodules
git clone --recurse-submodules https://github.com/EliteaAI/elitea-platform.git
cd elitea-platform

# Start everything (postgres, redis, traefik, elitea-main, elitea-ui)
task up

# Or manually:
podman compose -f deploy/docker-compose.yml up --build
```

The local compose stack is fronted by Traefik on host port `8080`.

Services will be available at:

| Service | URL | Description |
|---------|-----|-------------|
| Gateway | http://localhost:8080 | Traefik routing to API + UI |
| API | http://localhost:8080/api/ | elitea-main API endpoints |
| UI | http://localhost:8080/app/ | elitea-ui React SPA |
| postgres | localhost:5432 | PostgreSQL 18 |
| redis | localhost:6379 | Redis 7 |

## Standalone full stack

`deploy/docker-compose.yml` above is the older local shape and keeps the
legacy database and routing conventions. `deploy/docker-compose.standalone-full.yml`
is a separate, self-contained recipe for the **target** architecture: Go `elitea-main` +
`elitea-web` + the Bifrost `elitea-llm-gateway` + postgres/redis/rustfs/traefik
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

`check` prints one line for each assertion. Its last line reports how many
passed, how many failed, how many were skipped, and how many of the expected
assertions reported a result at all. A skipped assertion exits non-zero unless
you pass `--allow-skips`, and a run that reports fewer results than expected
exits non-zero even with that flag. Read that line rather than the exit status
alone.

Or via Task: `task standalone:up` / `task standalone:down`.

Compose project `elitea-standalone`. Ports: `8084` entry (Traefik), `8085`
gateway (direct, debug), `15433` postgres, `16380` redis, `9400` oidc-mock
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
`ELITEA_RUNTIME_ENABLED=true`, which requires a full runtime PKI (TLS Redis +
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

task ui:build        # Build EliteaUI SPA
task ui:lint         # Lint EliteaUI

task images          # Build all container images
task images:go       # Build elitea-main image only
task images:scheduler # Build elitea-scheduler image only
task images:gateway  # Build elitea-llm-gateway image only
task images:ui       # Build elitea-ui image only
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
| `REDIS_URL` | — | Redis host:port |

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

### elitea-ui (React)

React SPA served by nginx. Connected as a git submodule from [EliteaUI](https://github.com/EliteaAI/EliteaUI).

```bash
cd apps/elitea-ui
npm ci
npm run dev          # Local dev server
npm run build        # Production build
```

## CI/CD

All workflows are in `.github/workflows/`:

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `ci-go.yml` | PR/push to Go paths | Lint, test, build validation |
| `ci-ui.yml` | PR/push to UI paths | Lint, build SPA |
| `ci-python.yml` | PR/push to the Python worker, the SDK gates or the gateway | Worker suite, SDK conformance, projection checks |
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
- **Registry:** `ghcr.io/eliteaai`

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
