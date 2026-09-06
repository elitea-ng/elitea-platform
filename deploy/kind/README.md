# The DeepWiki provider on kind

A reproducible local Kubernetes stack that installs **this repository's Helm
chart** (`deploy/helm/elitea`) with the DeepWiki provider enabled in the shape
it ships to production (ADR-0023):

* the provider pod is **two containers** — the Go sub-application host
  (`ghcr.io/elitea-ng/elitea-subapp-host`) with `ELITEA_DEEPWIKI_RUNNER=legacy`,
  and the Python **engine sidecar** (`ghcr.io/elitea-ng/elitea-deepwiki`) it
  reaches over a Unix socket they share (`/run/deepwiki/engine.sock`);
* the `elitea-main → provider` hop is **mutually authenticated**, both
  certificates issued by cert-manager from one internal CA `ClusterIssuer`
  named `elitea-internal-ca`, exactly as
  `templates/deepwiki/certificates.yaml` asks for;
* the engine sidecar runs `deepwiki.engine.runner=fixture`, so a generation
  completes **without an LLM and without a git host**.

## Prerequisites

* `podman` — this machine has no docker. `kind` is driven with
  `KIND_EXPERIMENTAL_PROVIDER=podman`, which the script exports itself.
  `KIND_ENGINE=docker` switches every build, save and exec to docker.
  It also drops the provider override. That is how
  `.github/workflows/helm-install-smoke.yml` runs this stack on a GitHub
  runner.
* `kind` v0.33+, `kubectl`, `helm` v3/v4, `python3`, `curl`.
* Network access for the first run only: the cert-manager chart and its
  images. Everything else is built locally or side-loaded from podman.
* ~8 GB of free disk for the images, and ~4 GB of memory for the podman
  machine.
* The production-authentication material, which `up` mints through the
  repository's own `deploy/scripts/gen-runtime-certs.sh` (idempotent: a tree
  a compose stack already produced is reused untouched).

## The three commands

```bash
deploy/kind/kind-stack.sh up       # build, load, install, seed
deploy/kind/kind-stack.sh verify   # prove the four claims below
deploy/kind/kind-stack.sh down     # delete the cluster
```

`up` is idempotent: an existing cluster, an existing image and an already
installed cert-manager are reused, and the chart install is a
`helm upgrade --install`.

## What `up` does

1. creates the kind cluster `elitea-kind`;
2. builds `elitea-subapp-host:kind`, `elitea-main:kind` (target `e2e`) and
   `elitea-deepwiki:kind-engine` with podman, skipping any that already exist,
   and side-loads them plus the five third-party images into the node — so a
   repeat run touches no registry;
3. installs cert-manager and applies `manifests/ca-issuer.yaml`: a self-signed
   `ClusterIssuer`, a CA `Certificate` in the `cert-manager` namespace, and the
   `elitea-internal-ca` `ClusterIssuer` over it (a `ClusterIssuer`'s CA Secret
   is resolved in the controller's namespace, which is why the CA lives there);
4. mints (or reuses) the production-authentication material through the
   repository's own `deploy/scripts/gen-runtime-certs.sh` — idempotent, so a
   tree a compose stack already minted is reused untouched — and puts it into
   the two Secrets the chart names;
5. applies `manifests/infra.yaml` — PostgreSQL (`pgvector/pgvector:0.8.5-pg16`,
   with `CREATE EXTENSION vector` in its initdb), Redis and an S3-compatible
   object store (rustfs) — and the four other Secrets the chart names by name;
6. creates the artifact bucket **before** elitea-main starts, because
   `configureObjectStoreRetentionLifecycle` has no tolerance for a missing one
   and it is the Deployment that crash-loops, after the install looked green;
7. `helm upgrade --install` with `values-kind.yaml`. The chart's own
   pre-install hooks run both migrations: elitea-main's `-all-tenants` run and
   the provider's `python -m elitea_deepwiki.storage`;
8. applies `seed.sql` — the wiki toolkit, the repository credential the facade
   resolves through the project vault, the artifact bucket row, and the
   personal access token `verify` authenticates with.

## What `verify` proves

Every check exits non-zero on failure.

1. **Every pod is Ready.** Job pods (`Succeeded`) are excluded, because a
   completed migration Job is never `Ready` and reading it as a failure would
   make a correct stack fail forever.
2. **The provider pod has containers `elitea-deepwiki` and `engine`**, and
   their runners are `legacy` (the host, which reaches the sidecar over the
   socket) and `fixture` (the engine).
3. **The facade registered the provider.** `kubectl exec` into PostgreSQL:
   ```sql
   SELECT provider_id, healthy
     FROM provider_hub.provider_origin_registration o
     JOIN provider_hub.provider_health_projection p USING (project_id, provider_id)
   ```
   must report `wikis` with `healthy = t`. That row is written by
   `cmd/elitea-main/provider_registrar.go`, which only runs when
   `ELITEA_AI_PROJECT_ID` names a public project — so this check also proves
   the registrar is armed and its health probe reached the provider over the
   mTLS hop.
4. **A fixture generation completes through the facade.** Port-forwards
   elitea-main, mints the seeded PAT into an HS512 bearer over the auth
   plane's `auth-pat-signing-key`, `POST`s
   `/api/v2/deepwiki/tools/1/wikis/generate_wiki/invoke` with the request the
   web app sends, polls
   `/api/v2/deepwiki/invocations/1/wikis/generate_wiki/{id}` until
   `Completed`, and lists
   `/api/v2/artifacts/objects/1/wiki-artifacts?prefix=acme--e2e-generated--main/`
   for the manifest and the pages that landed.

## How long

Measured on an Apple-silicon laptop under podman, `down` then `up` then
`verify`. The GitHub runner path (`helm-install-smoke.yml`, docker, images
from the shared layer cache) records its own duration in the job summary;
the laptop numbers below say nothing about it.

* **`up` from no cluster at all: 2m26s**, with the three application images
  already built in podman. That is a fresh kind cluster, six image side-loads,
  cert-manager, the infrastructure, the chart and the seed.
* **Building the images adds ~8 minutes** the first time — elitea-main ~5
  (it builds the admin SPA), the engine sidecar ~2, the host ~1. `up` skips
  any image podman already has.
* **`verify`: 7 seconds.** The fixture runner's paced steps are the whole of
  it (`ELITEA_DEEPWIKI_FIXTURE_STEP_SECONDS: "1"`).

Image side-loading dominates a cold `up`, so the node's existing images are
skipped on a re-run; pass `KIND_FORCE_LOAD=1` after rebuilding an image under
a tag the node already carries.

## Why this stack carries Form authentication

The obvious minimal install is OIDC-only — that is the smallest configuration
in which the DeepWiki facade composes at all (ADR-0023 H0), and it was where
this stack started. It cannot satisfy check 3, and the chain is short:

* the provider registrar is armed only by a public project
  (`cmd/elitea-main/provider_registrar.go`: "provider registration skipped: no
  public project (ELITEA_AI_PROJECT_ID)");
* `ELITEA_AI_PROJECT_ID` is **refused** while the Configurations plane is off —
  `cmd/elitea-main/configurations_config.go` answers
  `current Configurations settings require explicit enablement`, and it is a
  boot failure, not a warning;
* `ELITEA_CONFIGURATIONS_ENABLED=true` requires an authenticated deployment —
  a credential reader plus a principal validator. Either plane answers now
  (`cmd/elitea-main/production_authentication.go`), so an OIDC-only install
  satisfies it too; this file uses the Form plane;
* the Form graph keeps its session and attempt store in a **mutually
  authenticated** Redis (`internal/authcomposition/redis.go` always builds a
  TLS config), which is why `runtimeRedis` is on.

So the DeepWiki facade needs none of this, and proving that the facade
REGISTERED its provider does. The runtime dispatch plane, the worker and the
LLM gateway stay off throughout.

## Known limits

* **The engine sidecar's `-engine` tag does not carry the engine closure.**
  `elitea-deepwiki.validateGuards` refuses a `runner: legacy` install whose
  engine image tag does not end in `-engine`, because on a real deployment that
  suffix means the ~92-package closure (torch, transformers, faiss-cpu,
  tree-sitter). This stack builds the tag with `EXTRAS=[storage-postgres]`
  only — the sidecar runs the **fixture** runner, which is what both compose
  stacks pair with the plain image, and the closure would add multiple GB for
  nothing. Set `DEEPWIKI_ENGINE_EXTRAS='[engine,storage-postgres]'` to build
  the real one.
* **No browser login.** The Form provider's user list exists so the graph
  parses (`gen-runtime-certs.sh` writes one throwaway account); `verify`
  authenticates with a PAT. There is no OIDC provider and no edge.
* **No web UI, gateway, scheduler, worker or OTel collector.** They are
  disabled through values; `dbInit` is off too, with
  `dbInit.externallyManaged: true` stating what the postgres initdb script
  does.
* **Nothing is durable.** PostgreSQL and the object store use `emptyDir`; a
  node restart loses the database. This is a proving stack, not a dev
  environment.

## Files

| file | what it is |
|------|------------|
| `kind-stack.sh` | `up` / `verify` / `down` |
| `values-kind.yaml` | the minimal chart values |
| `manifests/ca-issuer.yaml` | the self-signed CA and the `elitea-internal-ca` ClusterIssuer |
| `manifests/infra.yaml` | PostgreSQL, Redis, rustfs |
| `seed.sql` | the wiki toolkit, its credential, the bucket row, the PAT |

## The real engine

The kind stack runs the engine sidecar with the **fixture** runner: it proves
the chart, the two-container pod, the mTLS hop, admission and the artifact
path, offline. The real analysis engine is proven on compose instead —
`apps/elitea-web/scripts/deepwiki-real-engine.sh` runs the Playwright
project `deepwiki-real-engine` (DWIKI-014/014b) against
`deploy/docker-compose.deepwiki-real-engine.yml`: the `-engine` image with
`ELITEA_DEEPWIKI_RUNNER=legacy`, a git daemon serving the seeded repository,
and the deterministic LLM stub behind the gateway. In CI that recipe runs from
`.github/workflows/deepwiki-real-engine.yml` — manual dispatch plus a weekly
cron, never on a pull request. The same three things
are what a real-engine kind run would need in-cluster (a git host the engine
may clone from, a model the gateway resolves for the toolkit's project, the
engine image built with `DEEPWIKI_ENGINE_EXTRAS='[engine,storage-postgres]'`).
