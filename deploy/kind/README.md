# The chart's defaults on kind

A reproducible local Kubernetes stack that installs **this repository's Helm
chart** (`deploy/helm/elitea`) on its **shipped defaults** (S10): elitea-main
with its runtime plane, `elitea-vector` beside the bundled Qdrant (the upstream
`qdrant` subchart, REST and gRPC over TLS), and the DeepWiki and Inventory
providers in the shape they ship to production (ADR-0023). `values-kind.yaml`
turns off only what is too heavy for the proof (the worker, the LLM gateway,
the web UI, the scheduler) and wires none of elitea-main's provider or vector
env: the chart derives it from the one switch per feature.

* the provider pod is **two containers** — the Go sub-application host
  (`ghcr.io/elitea-ng/elitea-subapp-host`) with `ELITEA_DEEPWIKI_RUNNER=native`,
  and the Rust-native **engine sidecar**
  (`ghcr.io/elitea-ng/elitea-deepwiki-engine-native`) it reaches over a Unix
  socket they share (`/run/deepwiki/engine.sock`);
* the `elitea-main → provider` hop is **mutually authenticated**, both
  certificates issued by cert-manager from one internal CA `ClusterIssuer`
  named `elitea-internal-ca`, exactly as
  `templates/deepwiki/certificates.yaml` asks for;
* every runtime-CA certificate — the providers' callback client
  (`elitea-deepwiki-callback-client`, `elitea-inventory-callback-client`),
  the `elitea-vector` pair and Qdrant's `elitea-qdrant-tls` — is issued by
  cert-manager from a second `ClusterIssuer`, `elitea-runtime-ca`, backed by
  `gen-runtime-certs.sh`'s runtime CA (`manifests/runtime-ca-issuer.yaml`);
* both engine sidecars run their fixture runners
  (`deepwiki.engine.runner=fixture`, `inventory.engine.runner=fixture`), so a
  generation completes **without an LLM and without a git host**;
* the provider's way back is **gRPC on elitea-main's platform listener**
  (`:9446`, #1359 U5): the host uploads the pages over
  `elitea.runtime.v1.ArtifactObjectService`, presenting the sub-application
  callback certificate. That listener belongs to the runtime plane, so
  elitea-main runs it (`main.runtime.enabled`, the base plane only) on the
  NATS JetStream the chart bundles (`nats` + `natsBootstrap`).

## Prerequisites

* `podman` — this machine has no docker. `kind` is driven with
  `KIND_EXPERIMENTAL_PROVIDER=podman`, which the script exports itself.
  `KIND_ENGINE=docker` switches every build, save and exec to docker.
  It also drops the provider override. That is how
  `.github/workflows/helm-install-smoke.yml` runs this stack on a GitHub
  runner.
* `kind` v0.33+, `kubectl`, `helm` 4.3+ (3.8+ also works), `python3`, `curl`.
* Network access: the cert-manager chart and its images, the NATS images,
  which the node pulls, and the upstream NATS and Qdrant charts, which `up`
  fetches with `deploy/helm/elitea/build-deps.sh` (`helm dependency build`
  from `Chart.lock`). Everything else is built locally or side-loaded from
  podman.
* ~8 GB of free disk for the images, and ~4 GB of memory for the podman
  machine.
* The production-authentication material, which `up` mints through the
  repository's own `deploy/scripts/gen-runtime-certs.sh` (idempotent: a tree
  a compose stack already produced is reused untouched).

## The three commands

```bash
deploy/kind/kind-stack.sh up       # build, load, install, seed
deploy/kind/kind-stack.sh verify   # prove the five claims below
deploy/kind/kind-stack.sh down     # delete the cluster
```

`up` is idempotent: an existing cluster, an existing image and an already
installed cert-manager are reused, and the chart install is a
`helm upgrade --install`.

## What `up` does

1. creates the kind cluster `elitea-kind`;
2. builds `elitea-subapp-host:kind`, `elitea-main:kind` (target `e2e`),
   `elitea-deepwiki-engine-native:kind`, `elitea-inventory-engine:kind` and
   `elitea-vector:kind` with podman, skipping any that already exist, and
   side-loads them plus the third-party images (PostgreSQL, rustfs and
   `qdrant/qdrant:v1.19.2-unprivileged`) into the node — so a repeat run
   pulls only the NATS images;
3. installs cert-manager and applies `manifests/ca-issuer.yaml`: a self-signed
   `ClusterIssuer`, a CA `Certificate` in the `cert-manager` namespace, and the
   `elitea-internal-ca` `ClusterIssuer` over it (a `ClusterIssuer`'s CA Secret
   is resolved in the controller's namespace, which is why the CA lives there).
   Then it puts `gen-runtime-certs.sh`'s runtime CA into the Secret
   `elitea-runtime-ca-tls` in that namespace and applies
   `manifests/runtime-ca-issuer.yaml`, the `elitea-runtime-ca` `ClusterIssuer`;
4. mints (or reuses) the production-authentication material through the
   repository's own `deploy/scripts/gen-runtime-certs.sh` — idempotent, so a
   tree a compose stack already minted is reused untouched — and puts it into
   the Secrets the chart names: the auth material, the runtime material
   (`elitea-runtime-material`, the platform listener's keypair and the
   command-signing keys), `elitea-worker-material` with ONLY `runtime-ca.crt`
   (all the provider pods read from it; no worker runs here). No callback
   client Secret: the chart issues it;
5. applies `manifests/infra.yaml` — PostgreSQL (plain `postgres:18`, Debian 13,
   with `CREATE EXTENSION pgcrypto` in its initdb and no pgvector; the data is
   an `emptyDir`, so an image change starts on a fresh `initdb` and needs no
   collation repair) and an S3-compatible
   object store (rustfs) — and the other Secrets the chart names by name:
   `elitea-db`, `elitea-deepwiki-secrets`, `elitea-inventory-secrets`,
   `elitea-qdrant-api`, `elitea-main-llm-gateway-secrets` and
   `elitea-main-storage-secrets`;
6. creates the artifact bucket **before** elitea-main starts, because
   `configureObjectStoreRetentionLifecycle` has no tolerance for a missing one
   and it is the Deployment that crash-loops, after the install looked green;
7. `helm upgrade --install` of the ONE chart with `values-kind.yaml`, NATS
   included. The chart's own pre-install hooks run both migrations:
   elitea-main's `-all-tenants` run and the providers'
   `elitea-deepwiki-engine migrate` and `elitea-inventory-engine migrate`; its
   post-install hook
   runs the NATS bootstrap, which creates the runtime command-bus streams.
   No `--wait` (it would hold that hook until elitea-main is Ready, and
   elitea-main waits for the streams); `rollout status` waits instead;
8. applies `seed.sql` — the wiki toolkit, the repository credential the facade
   resolves through the project vault, the artifact bucket row, and the
   personal access token `verify` authenticates with.

## What `verify` proves

Every check exits non-zero on failure.

1. **Every pod is Ready.** Job pods (`Succeeded`) are excluded, because a
   completed migration Job is never `Ready` and reading it as a failure would
   make a correct stack fail forever. This is the check that covers the
   defaults `verify` drives no request through: Qdrant (`elitea-vectordb-0`,
   whose probes are HTTPS once its TLS is on), `elitea-vector` (Ready only
   once it reaches Qdrant) and the Inventory provider pod.
2. **The provider pod has containers `elitea-deepwiki` and `engine`**, and
   their runners are `native` (the host, which reaches the sidecar over the
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
   for the manifest and the pages that landed. Those pages reached the bucket
   over ArtifactObjectService on the platform listener, so this check is also
   the proof of the provider's gRPC artifact hop.
5. **The bundled KEDA scales DeepWiki (S11).** `values-kind.yaml` sets
   `keda.enabled: true` and `deepwiki.autoscaling.enabled: true`, so the
   release installs KEDA and DeepWiki's ScaledObject (KEDA's `postgresql`
   trigger over the host's invocation store, with a TriggerAuthentication on
   the `elitea-db` Secret). KEDA's CRDs arrive in the same release, so on the
   first install the ScaledObject is a post-install hook, adopted as an
   ordinary release object by the next `helm upgrade`. The check polls until
   `elitea-deepwiki` reports `Ready=True` — CRDs, operator, hook and a scaler
   that really queried PostgreSQL — and asserts KEDA's
   `keda-hpa-elitea-deepwiki` targets the Deployment. The KEDA images are
   pulled from `ghcr.io/kedacore`, not side-loaded. Delete the ScaledObjects
   before a `helm uninstall` of a release that bundles KEDA (`down` deletes
   the whole cluster, so it does not need to).

The runtime-plane and NATS steps (#1359 U5) and the S10 defaults (Qdrant,
elitea-vector, Inventory, the runtime-CA issuer) are **unverified locally**:
they render (`deploy/helm/tests/render-network-policies.sh` renders
`values-kind.yaml`) and lint, and the `helm-install-smoke.yml` run is what
proves them on a cluster.

## How long

Measured on an Apple-silicon laptop under podman, `down` then `up` then
`verify`. The GitHub runner path (`helm-install-smoke.yml`, docker, images
from the shared layer cache) records its own duration in the job summary;
the laptop numbers below say nothing about it.

* **`up` from no cluster at all: 2m26s**, with the three application images
  already built in podman. That is a fresh kind cluster, six image side-loads,
  cert-manager, the infrastructure, the chart and the seed.
  (Measured before S10 added Qdrant, elitea-vector and Inventory; not
  re-measured locally since.)
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
* the Form graph keeps its session and attempt store in PostgreSQL
  (elitea-main shared migration 0153), so no Redis is involved.

So the DeepWiki facade needs none of this, and proving that the facade
REGISTERED its provider does. The worker and the LLM gateway stay off
throughout; the runtime plane's dispatch streams are on, as they ship, with
nothing consuming them.

## Known limits

* **The native image is compiled from source.** A cold `up` builds
  `elitea-deepwiki-engine-native` (~420 crates), which is most of its time.
  The sidecar runs the **fixture** runner, which needs no LLM, no git host
  and no index database; the same image runs `migrate` for the Job.
* **No browser login.** The Form provider's user list exists so the graph
  parses (`gen-runtime-certs.sh` writes one throwaway account); `verify`
  authenticates with a PAT. There is no OIDC provider and no edge.
* **No web UI, gateway, scheduler, worker or OTel collector.** They are
  disabled through values (each `enabled: false`), for weight; `dbInit` is off too, with
  `dbInit.externallyManaged: true` stating what the postgres initdb script
  does.
* **Nothing is durable.** PostgreSQL and the object store use `emptyDir`; a
  node restart loses the database. Qdrant's 2Gi claim is a local-path volume
  on the node. This is a proving stack, not a dev
  environment.

## Files

| file | what it is |
|------|------------|
| `kind-stack.sh` | `up` / `verify` / `down` |
| `values-kind.yaml` | the chart values: the defaults, minus the heavy components |
| `manifests/ca-issuer.yaml` | the self-signed CA and the `elitea-internal-ca` ClusterIssuer |
| `manifests/runtime-ca-issuer.yaml` | the `elitea-runtime-ca` ClusterIssuer over `gen-runtime-certs.sh`'s CA |
| `manifests/infra.yaml` | PostgreSQL, rustfs |
| `seed.sql` | the wiki toolkit, its credential, the bucket row, the PAT |

## The real engine

The kind stack runs the engine sidecar with the **fixture** runner: it proves
the chart, the two-container pod, the mTLS hop, admission and the artifact
path, offline. The real analysis engine is proven on compose instead —
`apps/elitea-web/scripts/deepwiki-real-engine.sh` runs the Playwright
project `deepwiki-real-engine` (DWIKI-014/014b) against
`deploy/docker-compose.deepwiki-native-real-engine.yml`: the native engine
with its `native` runner, a git daemon serving the seeded repository, and the
deterministic LLM stub behind the gateway. In CI that recipe runs from
`.github/workflows/deepwiki-real-engine.yml` — manual dispatch plus a weekly
cron, never on a pull request. The same three things
are what a real-engine kind run would need in-cluster (a git host the engine
may clone from, a model the gateway resolves for the toolkit's project, and
`deepwiki.engine.runner: native`).
