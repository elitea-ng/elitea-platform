# Centry hybrid PoV deployment

This directory has two mixed-deployment checkpoints:

- `task hybrid:up` is the small routing foundation and selects only Go
  `/healthz`.
- `task hybrid:pov:up` is the reproducible Auth, Configurations and
  `index.ingest.v1` proof used by the integration PR.

Both keep the current Centry stack as the compatibility baseline. PostgreSQL,
Redis, `pylon_auth`, `pylon_main` and the current UI continue to own every
capability that has not passed an explicit Go/worker cutover gate.

`pylon_indexer` no longer does. Issue #339 retired it: the index plane runs on
the Go runtime plane and the Python agent worker, `pov-compose.yml` puts the
service in the `index-v1` profile so Compose does not start it, and
`rollback/index-v1.yml` restores it if the cutover has to be reversed before
version-2 admission reopens. Read `deploy/INDEX_V2_CUTOVER.md` before doing
either.

The small foundation overlay adds:

- one `elitea-main` process sharing Centry PostgreSQL and authenticated Redis;
- one loopback-only Traefik edge sharing `pylon_main`'s network namespace;
- exact route selection, limited to Go `/healthz`; and
- a catch-all current-Pylon route for UI, APIs, Socket.IO and static content.

The foundation stack serves `/healthz` from Go. Current Pylon serves every
other path, and that includes the notification API and the index API. The
foundation stack loads `traefik/base.yml` and `traefik/middlewares.yml` only.
It does not load `traefik/index-routes.yml`. Read the next section for the
reason.

Current Main is rewritten to bind to loopback inside its container namespace,
and its original host port is replaced with
`https://localhost:${ELITEA_HYBRID_HTTPS_PORT:-18443}`. Traefik uses its
generated local default certificate in this PoV. Production certificate
delivery, rotation and trust are separate release gates.

## The edge, and what it needs

Two Traefik configurations exist here, and they load different files. Know
which one you are changing.

| Stack | Command | Traefik gets | Middleware definitions |
|---|---|---|---|
| Foundation | `task hybrid:up` | `traefik/base.yml` and `traefik/middlewares.yml`, each as its own file under `/etc/traefik/dynamic` | `traefik/middlewares.yml` in this repository |
| Full PoV | `./compose.sh up` | only `traefik/index-routes.yml`, as `/etc/traefik/dynamic/index.yml` | `hybrid_auth/traefik-dynamic.yml` in the private centry repository |

Every router in `traefik/index-routes.yml` names three middlewares:
`strip-caller-auth-context`, `normalize-runtime-public-authority` and
`go-main-forward-auth`. Read `traefik/middlewares.yml` for what each one does
and why the order is fixed.

### Why the foundation stack does not load `traefik/index-routes.yml` (#378)

`go-main-forward-auth` calls
`http://elitea-main:8080/internal/forward-auth/main`. elitea-main registers
that path only when it composes production authentication, and it composes
production authentication only when `ELITEA_AUTH_CONFIG_FILE` is set.
`docker-compose.yml` does not set that variable for the foundation stack. The
forward-auth target therefore does not exist there, and Traefik returns the
non-2xx answer to the caller.

The full PoV stack sets that variable and defines the same middleware against
its own `elitea-main-auth` service, so the routers work there.

A directory mount loaded `traefik/index-routes.yml` into the foundation stack
as a side effect. `docker-compose.yml` now names each file it mounts.
`services/elitea-main/tests/deployedge/edge_forward_auth_test.go` reads that
volume list and fails when a loaded router names a forward-auth endpoint that
the same stack does not register.

Traefik does not fail a stack whose router names a middleware that no loaded
file defines. It logs the error, drops the router, and keeps serving. In this
deployment the dropped routers are the ones that select Go, and `base.yml`
holds a `PathPrefix("/")` catch-all to pylon at priority 1. The result is
HTTP 200 from pylon on a path the configuration says goes to elitea-main, and
the header-stripping middleware never runs either. Run
`task deploy:check-edge` to catch this before you deploy. The No Binaries
workflow runs the same gate on every pull request.

## Inputs that stay in the private centry repository

`compose.sh` needs seven inputs it cannot provide. It checks each one and
exits 2 with the missing path. Only the full PoV stack needs them; the
foundation stack does not.

| Input | What it provides |
|---|---|
| `$CENTRY/docker-compose.yml` | The current Centry compose model. Every overlay here applies on top of it. |
| `$CENTRY/hybrid_auth/docker-compose.pov.yml` | The hybrid Auth overlay. It defines the `auth_gateway` Traefik service and the `elitea-main-auth` target that the private middleware definitions name. |
| `$CENTRY/hybrid_auth/docker-compose.indexing-checkpoint.yml` | The indexing checkpoint overlay. |
| `$CENTRY/hybrid_auth/bootstrap-auth-pov.sh` | Auth PoV bootstrap. |
| `$CENTRY/hybrid_auth/bootstrap-runtime.sh` | Runtime bootstrap. |
| `$CENTRY/envs/default.env` | Shared compose and shell environment. |
| `$CENTRY/envs/override.env` | Local secrets. It stays untracked, and the scripts never copy or print it. |

Two more private inputs are used but not checked for existence:

| Input | What it provides |
|---|---|
| `$CENTRY/hybrid_auth/Containerfile.litellm` | The standalone LiteLLM image that `pov-compose.yml` builds. |
| `$CENTRY/hybrid_auth/traefik-dynamic.yml` | The PoV edge base file. It defines the three middlewares and points them at `elitea-main-auth`. |

`traefik/middlewares.yml` in this repository does not replace the private file
and does not collide with it. The PoV stack mounts one file only, so the two
never load together.

## Validate

From the `elitea-platform` repository:

```bash
task hybrid:config
```

The default assumes the repositories are siblings:

```text
projects/
├── centry/
└── elitea-platform/
```

For another layout, pass an absolute or repository-relative Centry path:

```bash
task hybrid:config CENTRY_DIR=/path/to/centry
```

The command merges the current Centry Compose model with this overlay and runs
`docker compose config --quiet`; it does not start or recreate containers.

## Start

```bash
task hybrid:up
```

This is deliberately a mixed deployment. Do not remove `pylon_auth` or
`pylon_main`, and do not add a product route to `traefik/base.yml` merely
because a prototype handler exists. A selected route
must carry its current HTTP/DTO behavior, exact permission/project policy,
tenant derivation, database effects, browser evidence and rollback gate.

The first integration keeps LiteLLM and all model/provider execution on the
current path. Bifrost and NATS worker transport are separate compatibility and
architecture slices.

## Reproduce the full integration proof

Prerequisites:

- a current Centry checkout with local `envs/override.env`;
- Docker with Compose v2, `jq` and `openssl`;
- registry/network access needed by the pinned container and Python dependency
  builds; and
- the current Centry database/configuration values. The scripts never copy
  them into tracked files or print them.

From this repository, run one command:

```bash
./deploy/centry-hybrid/compose.sh up ../centry
```

To build and deploy a specific EliteaUI checkout as part of the same command,
set `ELITEA_HYBRID_UI_DIR`. This is the reproducible way to prove a UI branch;
the launcher installs its build into Centry's ignored static directory before
starting the mixed stack:

```bash
ELITEA_HYBRID_UI_DIR=/absolute/path/to/EliteaUI \
./deploy/centry-hybrid/compose.sh up ../centry
```

The UI build uses same-origin `/api/v2` by default. A different API base can be
selected explicitly with `ELITEA_HYBRID_UI_SERVER_URL`; do not bake a
machine-specific URL into a build intended for another operator.

For a non-sibling checkout:

```bash
./deploy/centry-hybrid/compose.sh up /absolute/path/to/centry
```

The equivalent convenience alias, when [Task](https://taskfile.dev/) is
installed, is `task hybrid:pov:up`.

The command performs these bounded steps:

1. Reuses the current Centry environment and provisions Auth/runtime PKI and
   credentials under ignored `deploy/centry-hybrid/.runtime/`. Complete
   material is reused; a partial directory or certificate expiring within 24
   hours fails closed rather than being silently replaced.
2. Validates the merged Compose model and requires the exact Go index and
   notification-event routes, version-2 stream/group, Python worker runtime,
   external LiteLLM mounts and standalone LiteLLM build. This catches an empty
   or wrong route file before containers are recreated.
3. When `ELITEA_HYBRID_UI_DIR` is set, builds that exact EliteaUI checkout and
   replaces Centry's ignored static UI distribution. The selected UI Git
   revision is printed for evidence.
4. Builds Main and the Python worker from the checked-out platform commit.
5. Builds and launches standalone LiteLLM from Centry's
   `hybrid_auth/Containerfile.litellm`. It uses its own `litellm` database,
   generates the Prisma client during the image build, applies the LiteLLM
   schema at startup and is reachable only on the Compose network.
6. Starts the version-1 Redis/group long enough to run the read-only cutover
   preflight. Version-2 Redis admission remains blocked until that gate exits
   successfully.
7. Starts the mixed platform and waits for healthy services.

The current UI remains unchanged except for its index and notification SSE
client work. Current Pylon remains the catch-all owner outside the explicitly
ported notification list, count, details, read-state, delete, and event routes.
Only the exact authenticated Configurations, LiteLLM, index, and notification
routes in the tracked Traefik files select Go. Notification SSE reads committed
user-scoped rows from the existing `centry.notifications` table and supports
`Last-Event-ID`; notification CRUD uses that same table, and no second
notification schema is introduced. The independent worker still executes the current
synchronous SDK indexing logic inside its own process; Redis carries bounded
signed command references, while content and output use their private TLS
channels.

Useful checks:

```bash
./deploy/centry-hybrid/compose.sh config ../centry
./deploy/centry-hybrid/compose.sh ps ../centry
./deploy/centry-hybrid/compose.sh preflight ../centry
./deploy/centry-hybrid/compose.sh logs ../centry
```

Override local image tags when needed:

```bash
ELITEA_MAIN_IMAGE=elitea-ng/elitea-main:my-tag \
ELITEA_WORKER_IMAGE=elitea-ng/elitea-worker-python:my-tag \
./deploy/centry-hybrid/compose.sh up ../centry
```

### LiteLLM and dependency posture

LiteLLM is externalized for this checkpoint, but its compatibility
implementation is still Centry-owned. `pylon_main`, Go Main and the independent
worker all use the same standalone service and master-key contract. It was
externalized from `pylon_indexer` first; that service is retired now (#339), and
the configuration it read is kept beside the rollback overlay that would need
it. Bifrost replacement is intentionally outside
this PR.

The branch builds pinned inputs and does not upgrade dependencies as a
deployment side effect. Main's image scan and the worker's locked SDK/artifact
checks remain active. Remaining SDK/LiteLLM dependency remediation, full SBOM
and image signing are release gates documented in the gap register; they must
be handled as compatibility-tested upgrades rather than unreviewed changes to
this functional proof.
