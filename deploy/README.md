# `deploy/` — how EliteA is deployed

Two delivery paths, deliberately not one:

- **compose** (`docker-compose*.yml`) — local development, E2E and the
  standalone full stack. Runs the whole topology on one host.
- **Helm + ArgoCD** (`helm/`, `argocd/`) — Kubernetes. Composition is
  **app-of-apps**, not an umbrella chart.

> This machine uses **podman**: `podman compose up -d`, not `docker compose`.

Agent execution (the chat send path) is gated on `ELITEA_RUNTIME_ENABLED`, which
is a provisioning exercise rather than a flag — TLS Redis, three mTLS listeners,
a SAN-bearing workload certificate, an Ed25519 signing keyring, production auth
and a workload-session row. [`runtime/README.md`](runtime/README.md) documents
that contract and the permission rules its material must satisfy.

## Compose — the standalone stack

`deploy/docker-compose.standalone-full.yml`, driven by
`deploy/scripts/standalone-stack.sh`, is the one-host path: the whole topology,
including the runtime plane, on one machine.

### Operator quick start (no seeders)

The `standalone-stack.sh seed*` subcommands (`seed`, `seed-runtime`, `seed-llm`,
`seed-index`) are E2E and local-development conveniences, not required steps.
A fresh install reaches a working, logged-in stack with:

```bash
deploy/scripts/standalone-stack.sh certs
deploy/scripts/standalone-stack.sh build
deploy/scripts/standalone-stack.sh up
# log in through the browser, then configure from the UI
```

Everything a hand-written `INSERT` used to cover is now automatic on `up`:
schema bootstrap (`db-init`, `elitea-migrate`), the `agentstate` database and
the `vector` extension (`db-init`), the artifact bucket (`rustfs-bucket-init`),
RBAC roles and a per-user PAT and personal project (first-login provisioning
in `cmd/elitea-main`), and — since this fix — the worker's
`elitea_runtime.workload_sessions` authorization (`runtime-session-init`,
below). `standalone-stack.sh`'s own header comment carries the equivalent
E2E/dev run, with every seeder, for local development and CI.

The one bootstrap floor an operator still sets by hand is
`GATEWAY_EGRESS_ALLOWLIST` on the `elitea-llm-gateway` service: it is the
comma-separated `host:port` allowlist the gateway checks a saved credential's
`api_base` against before it will proxy to it
(`internal/config/config.go`, `internal/account/egress.go`), and it defaults
to the compose network's own mock/real LLM services. Add a private model
host (a self-hosted vLLM instance, for example) to it before saving a
credential that points there, or the gateway refuses the connection with what
reads as the model being down.

### `ELITEA_INITIAL_GLOBAL_ADMINS` — the first administrator

Every standalone/production-shaped compose file passes this variable through
to `elitea-main` (`v2auth.InitialGlobalAdminsFromEnv`,
`cmd/elitea-main/main.go`). It is read only as a fallback, when the
deployment's authentication configuration document has an empty
`identity.initial_global_admins` list — which is the case for
`deploy/runtime/auth.form.yml`, the standalone stack's document, on purpose:
Form is a structural requirement there, not the login path, and the document
is not where this stack names its administrator.

Set it to a comma-separated list of entries. Two forms are accepted today,
matching the federated login planes' stored (prefixed) provider references —
see `matchesInitialGlobalAdmin` in
`services/elitea-main/internal/api/v2/auth/first_login.go` for the exact rule:

- `oidc:<sub>` — the OIDC subject claim
- `saml:<nameid>` — the SAML NameID

`email:<verified address>` — matching the login's verified e-mail claim
instead of a provider-specific subject — is landing in a parallel branch.

The variable is empty by default in every compose file, so a fresh clone
stays unprivileged until an operator opts in.

### `runtime-session-init` — authorizing the worker automatically

The agent worker cannot start serving chat until a row exists in
`elitea_runtime.workload_sessions` naming its certificate identity;
`WorkloadSessionsRepository` has no process-local registration or fallback
allowlist by design, so nothing mints that row on its own. Helm automates
this with a pre-install/pre-upgrade hook Job
(`deploy/helm/elitea/templates/worker/runtime-session-job.yaml`, plus a
recurring renewal CronJob — nothing else re-stamps `expires_at`, so a
deployment left un-upgraded goes dark on a timer otherwise).

`docker-compose.standalone-full.yml` now runs the equivalent as a one-shot
`runtime-session-init` service, ordered before the worker with
`depends_on: condition: service_completed_successfully`. It runs
`deploy/runtime/provision-runtime-session.sh` — the same upsert-and-verify
shell script the Helm Job runs (extracting the worker's SPIFFE identity from
its certificate's SAN, waiting for the schema, upserting the row, then
re-checking it with the exact conjunction `VerifyActiveSession` applies) —
kept as a byte-for-byte mirror rather than a shared file, because charts here
stay self-contained artifacts independent of the monorepo layout they ship
from. Its session id, producer id, TTL and wait behavior are configurable
through `RUNTIME_WORKER_SESSION_ID`, `RUNTIME_WORKER_PRODUCER_ID`,
`RUNTIME_WORKER_SESSION_TTL`, `RUNTIME_WORKER_SESSION_WAIT_ATTEMPTS` and
`RUNTIME_WORKER_SESSION_WAIT_INTERVAL`, defaulting to the same values
`deploy/helm/elitea/values.yaml`'s `worker.runtime` and
`worker.runtimeSession` blocks default to.

`standalone-stack.sh seed-runtime` still exists as a manual fallback — a
database restored from a backup that predates this row, or a worker brought
up with `--no-deps` — but a normal `up` no longer needs it.

### DeepWiki and Inventory — canned data with no engine closure

Both sub-applications run their Go host (`elitea-subapp-host`) with
`RUNNER=legacy`, reaching an engine SIDECAR (`elitea-deepwiki`,
`elitea-inventory`) over a shared Unix socket — the socket hop this stack
exists to exercise, not just the Go half. Neither sidecar carries the real
analysis engine's dependency closure here; both serve their `fixture`
runner instead, so `up` shows a populated wiki and a populated Inventory
graph with no repository, no model and no ~1 GB+ engine image.

Inventory's fixture graph (six entities, two source toolkits, five
relations) is the SAME one the Go sub-application host's own fixture runner
serves on the E2E stack
(`conformance/provider/fixtures/inventory/spi/graph.json` — see
`services/elitea-inventory/README.md`'s "Fixture mode" section for how the
two are kept from drifting). `INVENTORY_FIXTURES=/fixtures/inventory`
points the `elitea-inventory-engine` service at the bind-mounted copy of
that directory instead of the image's packaged one, for iterating on the
fixture data without a rebuild:

```bash
INVENTORY_FIXTURES=/fixtures/inventory deploy/scripts/standalone-stack.sh up
```

`INVENTORY_RUNNER=fixture` is a separate switch on the Go host itself
(`elitea-inventory`, not the engine sidecar): it bypasses the sidecar
entirely and serves the Go runner's own copy of the same graph. Useful for
isolating which half of the hop you are looking at; not needed for a normal
demo now that the sidecar's fixture is populated by default.

## Composition decision (issue #240)

`deploy/helm/elitea-platform/` used to be an empty `.gitkeep` — an umbrella
chart that was scaffolded and never built. It has been **removed**. The single
composition mechanism is `deploy/argocd/app-of-apps.yaml`, which renders every
Application in `deploy/argocd/applications/` and syncs them in sync-wave order.
Two mechanisms would mean two places to add a service and two answers to "what
is deployed"; there is now one.

```bash
kubectl apply -f deploy/argocd/app-of-apps.yaml
```

`kubectl apply -f deploy/argocd/` reaches only that file — kubectl does not
recurse — which is the intended behaviour: the children are created by ArgoCD.

The root syncs nothing usable on its own. Two values have no chart default and
cannot get one, and the platform chart refuses to render without them. Read
[Values an operator supplies](#values-an-operator-supplies-and-where-each-one-goes-475)
first.

## Chart × service × status

The platform is ONE chart and ONE Application. It used to be eight charts and
six Applications; they synced independently, nothing ordered them, and each
carried its own copy of the database secret name, the NATS URL and the Redis
address. Those copies had already drifted apart.

| Chart (`deploy/helm/`) | ArgoCD Application | Wave | Namespace | Status |
|---|---|---|---|---|
| `nats` | `applications/nats.yaml` | -2 | `elitea-gateway` | **Production reference.** scale-1 profile by default; `values-ha.yaml` for HA. |
| `nats-bootstrap` | `applications/nats-bootstrap.yaml` | -1 | `elitea-gateway` | **Production reference.** Idempotent Helm hook Job; HA needs `replicas=3`. |
| `elitea` | `applications/elitea.yaml` | 0 | `elitea` | **The platform.** One release: elitea-main and its migration Job, elitea-web, the scheduler, the LLM gateway, the agent worker, the runtime Redis, the OTel collector, the `dbInit` Job, and the DeepWiki provider service with its own migration Job. |

Components of the `elitea` chart are switched by `<component>.enabled`, and
`deploy/helm/elitea/values.yaml` holds every one of them. Ordering inside the
platform is Helm hook ordering, not a sync wave: the migration runs
`pre-install,pre-upgrade` and Helm blocks the release until it finishes, the
workload-session Job follows it, and the Redis consumer groups are created
`post-install`. A failed migration aborts the release, and the previous pods
keep serving.

Three charts, and that is all of them: `helm lint`, the template matrix and
`publish.yml` each read `deploy/helm/*/Chart.yaml`, so a fourth chart fails CI
until somebody templates and publishes it.

One image has no chart. `ghcr.io/elitea-ng/elitea-ui` is the old UI: it runs
in compose, and the Kubernetes path is the `web` component. `pylon-indexer`
used to be the second one, deployed nowhere; issue #339 deleted the service, so
it is no longer built or published either. The Go runtime plane serves index
ingest through the agent worker, on the same command stream.

One component is off by default and needs TWO settings, not one. `deepwiki`
is the DeepWiki provider service (ADR-0022). Turning it on is the `deepwiki`
block AND `main.env.ELITEA_DEEPWIKI_ENABLED` with its base URL, callback
origin, git allowlist and the three client-certificate paths — elitea-main is
the only door to it, so a provider nobody can reach is a Deployment doing
nothing. Both halves refuse to render while half configured
(`elitea-deepwiki.validateGuards`, `elitea-main.validateDeepWiki`), because the
container's own refusal is a CrashLoopBackOff somebody has to go read logs for.

Two things about it are worth knowing before turning it on. The published
image carries the engine SOURCE but not its ~92-package closure, so it serves
the whole SPI and REFUSES every tool; the `-engine` image tag is the one that
can run a generation, and the chart refuses the combination of
`ELITEA_DEEPWIKI_RUNNER=legacy` with a non-engine tag. And
`ELITEA_DEEPWIKI_GIT_ALLOWLIST` is read by BOTH halves and is fail-closed on
both: the facade checks it before opening the vault, the provider before
building a clone URL, and two values that disagree mean an invocation that
starts and then fails.

Two images share one chart component. The `worker` component runs either
`elitea-worker-rust` (the default) or `elitea-worker-python`, selected by
`worker.implementation`. They are not drop-in for each other and the chart
handles the difference rather than the operator: the Rust worker's argument
parser matches an exact five-token command line and additionally requires
`--toolkit-security-config`, so the Python argument list makes it exit
immediately with `worker_cli.invalid_arguments`. `worker.runtime.sensitiveTools`
is written once and carried to each as that implementation expects — an
environment variable for Python, a second JSON file for Rust. `runtime.json`
itself is byte-identical for both, and `deploy/helm/tests/render-worker.sh`
holds all of that.

## Values an operator supplies, and where each one goes (#475)

Read this before the first sync. A reader of the committed files alone can say
where every value comes from, and this table is that answer.

`deploy/argocd/applications/elitea.yaml` states its values in one
`spec.source.helm` block. Nothing reaches the release from anywhere else.

| Value | Where it comes from | Who supplies it |
|---|---|---|
| Everything with a default | `deploy/helm/elitea/values.yaml`, named in `spec.source.helm.valueFiles` | the chart |
| `postgresql.existingSecret`, `postgresql.key` | `spec.source.helm.parameters` | the chart states a default; change it to your Secret |
| `llmGateway.env.GATEWAY_SELF_LLM_ORIGINS` | `spec.source.helm.parameters`, **empty in git** | **the operator** |
| `llmGateway.egressPosture` | `spec.source.helm.parameters`, **empty in git** | **the operator** |
| the database password itself | the Kubernetes Secret that `postgresql.existingSecret` names | **the operator**, out of band |
| the runtime material (CA, certificates, signing keyring, Redis password, spool key) | the Kubernetes Secrets that `main.runtime.material.secretName`, `worker.materialSecretName` and `runtimeRedis.materialSecretName` name | **the operator**, out of band — see [`runtime/README.md`](runtime/README.md) |

**The two empty parameters are fields, not defaults.** Neither can get a chart
default: both name addresses that only the operator knows, and a guessed origin
would guard a name nobody uses and read as armed. The chart REFUSES to render
while either is empty, so a sync of the committed file reports SyncFailed and
names the field. It does not install a gateway with a disarmed guard.

Fill them in your own GitOps copy of the Application, or with:

```bash
argocd app set elitea \
  -p llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.example.com/llm/v1 \
  -p llmGateway.egressPosture=public-unrestricted
```

**No secret goes in a parameter.** A Helm parameter is rendered into a
ConfigMap, which anybody with `get` on the namespace can read. The database
DSN carries a password, so the chart takes a Secret NAME and every component
that reads the database reads that one Secret. Prove it after a change:

```bash
helm template elitea deploy/helm/elitea \
  -f deploy/helm/elitea/values.yaml \
  --set-string postgresql.existingSecret=elitea-main-db \
  --set-string postgresql.key=database-url \
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1 \
  --set-string llmGateway.egressPosture=public-unrestricted \
  | grep -A4 'name: DATABASE_URL'
```

`deploy/helm/tests/render-bf0-2b.sh` makes the same assertion in CI. It reads
the parameters out of the Application, renders the chart from them, and reads
DATABASE_URL back out of the manifest, so an Application that stops supplying
its values fails there.

### The first global administrator (`identity.initial_global_admins`)

A fresh database holds one administrator, `dev@elitea.ai`. That account has no
password on a single-sign-on deployment, so it cannot sign in. Name your own
account instead.

Supply the list in ONE of two places. The authentication configuration document
wins when the deployment has one:

- `main.authConfig...identity.initial_global_admins` — a YAML list.
- `main.env.ELITEA_INITIAL_GLOBAL_ADMINS` — a comma-separated string. Use this
  on a single-sign-on-only deployment, which carries no document.

**An entry takes one of three shapes.** Since v1.39.0 the list applies to OIDC
and to SAML logins alike.

| Shape | Matches |
|---|---|
| `oidc:<sub>` | the OIDC subject, as the database stores it |
| `saml:<nameid>` | the SAML NameID, as the database stores it |
| `email:<address>` | a VERIFIED e-mail address, case-insensitive |

A reference may also be written bare, with no `oidc:` / `saml:` namespace. The
namespaced spelling is what the database holds, so prefer it.

**Read your own reference without a database session.** Sign in once, then call
the endpoint the application already serves:

```bash
curl -s -H "Cookie: elitea_session=<your session>" \
  https://elitea.example.com/api/v2/social/author | jq .provider_refs
```

`provider_refs` holds the exact values this list accepts. Before this endpoint
carried the field, an operator on Azure AD or Okta had no other way to learn it:
their OIDC `sub` is an opaque identifier that nobody can know before the first
sign-in, so the procedure was a `SELECT provider_ref FROM
auth_core__user_provider` against the production database.

**`email:` needs a stated verification.** It matches an OIDC login whose
id_token carries `"email_verified": true`, and nothing else. A provider that
OMITS the claim cannot be matched by an address — use `oidc:<sub>` there.
`OIDC_REQUIRE_EMAIL_VERIFIED` does not change this: that variable gates account
ADOPTION by address, not account creation, so it states nothing about a first
login. SAML carries no verified-address statement at all, so a SAML deployment
names its administrator by `saml:<nameid>`, which is often the address already.
An identity provider that merely asserts an address can never assert its way
into the administration role.

**The grant is one-shot per account.** Somebody who already holds ANY
administration-mode role is left exactly as you left them, so a demotion is
never undone by the next sign-in. An account that holds none still receives the
grant, so the normal recovery works: add the entry, restart the pod, sign in
again. Only `dev@elitea.ai` is seeded with an administration-mode role, and only
in a fresh database.

**A malformed entry warns and is ignored.** The pod logs how many entries are
reference-shaped and how many are address-shaped, and warns about the rest, so a
misspelling is visible at boot rather than at somebody's first sign-in.

## Outbound e-mail — the SMTP variables are bootstrap defaults (gap G7)

`SMTP_HOST`, `SMTP_PORT`, `SMTP_USERNAME`, `SMTP_PASSWORD`, `SMTP_TLS`,
`EMAIL_FROM`, `EMAIL_REPLY_TO` and `PUBLIC_BASE_URL` are the DEFAULT layer.
They are not the whole configuration any more.

The relay is administered at **Admin → E-mail** (`/admin/app/email`, also the
Configuration page's "E-mail" section). That page writes the `email` section of
`centry.platform_config` and seals the SMTP password in the platform vault's
hidden bucket. `internal/emailsettings` merges the two layers FIELD BY FIELD on
every send, with the database winning.

Three consequences for an operator:

1. **A change in the console needs no restart.** The merge runs per message, so
   a corrected relay host reaches the next invitation. Nothing in this chart has
   to be re-synced.
2. **A chart may ship some of these and leave the rest to the console.** A
   deployment that sets `EMAIL_FROM` and `PUBLIC_BASE_URL` and lets an
   administrator name the relay is a normal install. The pod no longer refuses
   to start on a partial set — completeness is decided on the merged document at
   send time, and an incomplete merge reports `invitation_delivered: false` with
   the field to fix. A value that is WRONG still refuses to start.
3. **The password belongs in the vault or in the Secret, never in a chart
   parameter.** A Helm parameter renders into a ConfigMap. `SMTP_PASSWORD` is in
   the `secrets:` block for that reason, and the console's own password goes to
   the vault, not to a `platform_config` row.

`ELITEA_EMAIL_SUPPRESS: "true"` still renders every message and sends none — the
setting for a shadow or staging deployment. It outranks the console: a
suppressed deployment reports nothing as delivered however the relay is
configured.

Check what a running deployment resolved. The route is gated on
`runtime.plugins` in administration mode, so send an administrator session
cookie or a personal access token with it:

```bash
curl -s -H "Authorization: Bearer $ADMIN_PAT" \
  https://elitea.example.com/api/v2/admin/email/administration | jq
# .settings  — what the console stored
# .effective — the merged document the next message uses
# .sources   — per field: "database" | "environment" | "unset"
# .configured / .reason — whether a message can be sent, and if not, which field to set
```

## Distribution — the charts are published to GHCR as OCI artifacts

Every chart in the table above is packaged and pushed on each release by the
`chart` job in `.github/workflows/publish.yml`, next to the images:

```
oci://ghcr.io/elitea-ng/charts/<chart>
```

Install without cloning this repository:

```bash
helm install elitea oci://ghcr.io/elitea-ng/charts/elitea --version 1.2.3
```

Four properties of the published artifact that the in-repo chart does not have:

- **`--version` is required in practice.** There is no `latest` chart tag, and
  there will not be one: an OCI chart reference resolves by SemVer, so a
  non-SemVer tag is a foot-gun for every tool that enumerates the repository.
  The in-repo `Chart.yaml` files all stay at the placeholder `0.1.0`; only the
  packaged artifact carries a release number.
- **`image.tag` defaults to the chart version** for the `elitea` chart, whose
  images this repository publishes. The in-repo default is `"latest"` with
  `pullPolicy: IfNotPresent`, which means a node holding an older cached
  `latest` layer keeps serving it — the release job stamps the tag at package
  time so the published chart cannot install that way.
- **`appVersion` tracks the platform release** for that same chart. The two
  charts that deploy a third-party image (`nats`, `nats-bootstrap`) keep their
  deliberate upstream pin, because a platform release number means nothing to
  those images' registries.
- **Charts are cosign-signed**, keyless, exactly as the images are:

  ```bash
  cosign verify ghcr.io/elitea-ng/charts/elitea:1.2.3 \
    --certificate-identity-regexp '^https://github.com/elitea-ng/elitea-platform/' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  ```

The job runs after the images are pushed and signed, so a chart never reaches
the registry referencing a tag that does not exist yet. If any part of a
release fails, the rollback job deletes the pushed charts along with the
images.

## Capability flags — what a Helm install serves (#382)

`cmd/elitea-main` gates whole capabilities on environment variables. Each
variable makes the composition root build a dependency, and the router
registers a route group only when that dependency exists.

The chart used to set **none** of them. So the same image served the pylon-free
configuration on compose and served none of it on Kubernetes — the platform
could run without pylon on compose and could not on Kubernetes, which is the
deployment target. `deploy/helm/elitea/values.yaml` now carries every flag
under `main.env`, and `deploy/helm/elitea/values-standalone.yaml` is the values
file whose capability set matches `docker-compose.standalone-full.yml`.

| Flag | Default install | `values-standalone.yaml` | Prerequisite |
|---|---|---|---|
| `ELITEA_ARTIFACTS_ENABLED` | on | on | object storage configured |
| `ELITEA_CONFIGURATIONS_ENABLED` | **derived** | **on** | any authentication **and** `ELITEA_AI_PROJECT_ID` |
| `ELITEA_PROJECT_INFO_ENABLED` | off | **on** | production authentication |
| `ELITEA_AI_PROJECT_ID` | empty | **set** | must name a project that exists — set `platform.aiProjectId`, not this key |
| `ELITEA_CONFIGURATIONS_MUTATION_ENABLED` | off | off | `ELITEA_CONFIGURATIONS_ENABLED` **and** `runtime.enabled` — read below |
| `ELITEA_INDEX_TYPES_ENABLED` | off | **on** | production authentication |
| `ELITEA_APPLICATION_SKILLS_ENABLED` | off | **on** | production authentication |
| `REDIS_URL` | empty | **set at install** | a Redis the cluster can reach |
| `ADMIN_UI_STATIC_DIR` | **set** | set | the image ships the bundle at it |
| `ELITEA_RUNTIME_ENABLED` and its block | off | **on** | production authentication **and** runtime material — read below |

No flag stays off in **both** files any more.

### `ELITEA_CONFIGURATIONS_ENABLED` follows the deployment

This flag no longer ships a literal value. `values.yaml` leaves it **empty**,
and `templates/main/_helpers.tpl` renders `"true"` for an install that has both
prerequisites the chart can see:

1. **Any authentication.** `fileConfig.authConfig` (the Form document) **or**
   `env.OIDC_ISSUER_URL` (the single-sign-on plane, which a SAML or typed
   identity provider composes through).
2. **`env.ELITEA_AI_PROJECT_ID`.** The binary refuses to start without it once
   the plane is on, so the chart refuses the manifest instead.

State `"true"` or `"false"` under `main.env` to decide it yourself. A stated
value always wins.

**An OIDC-only install gets the configuration plane.** It could not before.
`cmd/elitea-main` tested the FormGraph, and only `ELITEA_AUTH_CONFIG_FILE`
builds one, so an install with real corporate single sign-on was refused every
credential route, the whole model catalogue and the project vector store —
however real its SSO. Each LLM setup step then fell back to
`deploy/scripts/seed-llm-api.py` or hand-written SQL. The composition root now
asks `productionAuthenticationComposed`: a reader of the caller's credential
plus a `PrincipalValidator`. The OIDC session plane carries both.

A deployment with **no** authentication is still refused, in the chart and in
the binary. That is the shape a default `values.yaml` describes, which is why
the default install still renders the flag `"false"`.

### `values-auth-minimal.yaml` — the cheapest install that can seed itself

`values-standalone.yaml` turns the whole capability set on, the runtime plane
included, and that plane costs a second Secret of certificates and signing keys,
a worker, and a dispatch bus with consumer groups.

The last step of a fresh install that still needed hand-written SQL is smaller
than that: seeding an LLM credential and the model catalogue. It needs
`ELITEA_CONFIGURATIONS_ENABLED` and nothing else — that flag composes the
configuration write path, and that path decides `status_ok` in the request
(#457), which is what makes a saved credential visible to the gateway.

`deploy/helm/elitea/values-auth-minimal.yaml` is that shape:
`fileConfig.authConfig` + `runtimeRedis`, **no runtime plane**, and
`ELITEA_CONFIGURATIONS_ENABLED` + the public project (`platform.aiProjectId`)
on top.
`templates/guards.yaml` ties the worker to the runtime plane and to
`runtimeRedis`; it does not tie `runtimeRedis` to the runtime plane, so this
combination renders. The TLS Redis is not optional even so — production Form
authentication keeps its session store there and
`internal/authcomposition/config.go` accepts a `rediss://` URL only.

Two things it does **not** change:

- No default for the flags it does not name. `values.yaml` still ships
  `ELITEA_PROJECT_INFO_ENABLED`, `ELITEA_INDEX_TYPES_ENABLED` and
  `ELITEA_APPLICATION_SKILLS_ENABLED` as `"false"`.
- Not the mutation flag. It is deliberately absent, which leaves `"false"` in
  force — read the section below for why that flag is a separate cutover.

It is also no longer the **only** way to reach the configuration write path.
An OIDC-only install reaches it by naming `env.ELITEA_AI_PROJECT_ID`, with no
Form document and no TLS Redis at all. This file stays the recipe for an
install that wants the Form plane itself.

`deploy/scripts/standalone-stack.sh seed-llm` writes its rows through that
route now (`deploy/scripts/seed-llm-api.py`), not with `INSERT`. Two database
calls survive, both named in `SEED_LLM_SQL_EXCEPTIONS` in that script, and the
subcommand checks its own command trace against that list before it exits.

Two used to. Each answered a body that only one of the two shipped clients
could read, so the flag could not be turned on without breaking the other. Both
were fixed the same way — ONE body carrying BOTH key sets, projected from the
same rows, so the halves cannot disagree:

- `ELITEA_APPLICATION_SKILLS_ENABLED` (#395). The attached-skills read answers
  the published `SkillsList` keys (`items`, `total`, `page`, `page_size`,
  `total_pages`) that `apps/elitea-web` reads, beside the Pylon keys (`skills`,
  `max_skills`) that `apps/elitea-ui` reads.
- `ELITEA_INDEX_TYPES_ENABLED` (#394). The index-types read answers the
  published `DocumentLoadersResponse` keys (`items`, `total`) beside the Pylon
  keys (`document_types`, `image_types`, `code_types`). Every entry of `items`
  names one category and lists that category's extensions, so the two halves
  are the same pinned SDK snapshot read twice.

Both are off in a default install only because the capability needs production
authentication, which a default install does not build — the same reason
`ELITEA_PROJECT_INFO_ENABLED` is off there. Both still test the FormGraph in
`cmd/elitea-main`; only `ELITEA_CONFIGURATIONS_ENABLED` was moved to the
credential-plane predicate, because it is the one that blocks a fresh install
from seeding its own LLM configuration.

Turning the skills flag off again is a safe rollback: `internal/api/router.go`
serves the same path from the skills handler, with the same rows in the same
published envelope. Turning the index-types flag off is **not** free. The
toolkits handler answers that path instead, and it **refuses** — `501` with
`{"code": "index_types_not_available"}`. It used to answer `200` with a static
six-loader list that no data backs; the refusal is the honest form of the same
gap, because a client can see it, and `501` is classified as final by
`apps/elitea-web` so the screen does not retry it. A default install therefore
still ships that gap, but no longer disguises it as an answer.

`ELITEA_PROJECT_INFO_ENABLED` behaves the same way when off: the prototype
`elitea_core` handler answers `501` with
`{"code": "project_info_not_available"}` rather than `200` with a null
`icon_meta` and no `teammates_count`. Project-icon SELECTION is unaffected by
the flag — the `PUT .../project-info` write and the `project_icon` upload,
listing and delete are served by the `elitea_core` handler in every install,
and they now write to and read from the object store and the tenant
`configuration` row for real. Uploaded project icons need an object store
(`STORAGE_BACKEND` / `STORAGE_CONTAINER`); without one those three routes
answer `501` `icon_storage_not_configured`.

### Why `ELITEA_CONFIGURATIONS_MUTATION_ENABLED` stays off

This row used to record the prerequisite as "needs the retired LiteLLM
lifecycle facade". That reason is gone. The configuration lifecycle takes no
LLM transport at all now — read the comment at the end of
`services/elitea-main/cmd/elitea-main/configurations_config.go` and the one at
the composition site in `cmd/elitea-main/main.go`. Issue #460 records the stale
row.

The flag stays off for two reasons that are true today.

1. **Its real prerequisites are larger than the flag.** The chart refuses the
   flag without `ELITEA_CONFIGURATIONS_ENABLED="true"` and without
   `runtime.enabled=true`, because the write routes dispatch a
   configuration-validation command. `values.yaml` has neither, so the default
   install cannot set the flag at all.
2. **The flag is not what makes a saved credential usable.** The flag composes
   a second write route, and that route wins the path when both are composed —
   see `TestConfigurationWriteRouteWinnerDependsOnTheMutationComposition` in
   `services/elitea-main/internal/api`. It is a different request contract from
   the one `apps/elitea-web` sends today, and its lifecycle reconciler writes
   `configuration.status_ok` asynchronously. The compatibility route that every
   install serves now writes `status_ok` itself, in the request, from the same
   admission decision the lifecycle uses (issue #457). Turning the flag on is
   therefore a separate cutover with its own web-client work, not a remedy for
   an invisible credential.

Prerequisites are checked while the chart renders, not when the pod starts. A
values file that turns a capability on without what it needs fails
`helm template` with a message naming the field and the Go source that would
otherwise refuse at boot. `deploy/helm/tests/render-capabilities.sh` asserts
all of this against the rendered YAML, and it reads the required
runtime names out of `internal/runtimecomposition/config.go`, so a newly
required name fails the gate until the chart renders it.

### The runtime plane, and how its material arrives

The runtime plane is agent execution, the execution-events stream, index
ingest, index scheduling and configuration validation. It is **all-or-nothing**:
`internal/runtimecomposition/config.go` requires about thirty names at once and
refuses to start on a partial set. The chart exposes the whole block under
`runtime:` and refuses a partial one at render time.

Its material — the signing key, the verification keyring, the Redis password,
the Redis CA and the three listener keypairs — comes from a **plain Kubernetes
Secret**. Set `runtime.material.secretName`, and give the Secret one key for
each of these names, which are the names `deploy/scripts/gen-runtime-certs.sh`
writes:

```
runtime-ca.crt                command-signing-key.pem
command-signing-keyring.json  redis-producer-password
control-server.crt   control-server.key
output-server.crt    output-server.key
content-server.crt   content-server.key
```

The three server certificates must carry the Service DNS name, because the
agent worker dials all three listeners through this chart's Service.

#### Why an init container copies the Secret (issue #404)

The runtime reads every one of those files through
`internal/security/securefile`, which refuses a path that resolves through a
symlink and requires **owner bits only** on private material. A Kubernetes
Secret volume gives neither:

- mounted whole, it is a symlink farm, so every path resolves through `..data/`
  and `securefile` refuses it;
- mounted per file with `subPath`, the files are real but owned by `root`,
  while this pod runs as nonroot — and the only modes that let a nonroot
  process read a root-owned file (`0440` with `fsGroup`, or `0444`) carry the
  group or other bits that `securefile` rejects.

So the chart adds an init container, `runtime-material`. It runs the **same
image** as the service, and therefore the **same user**. It copies each Secret
key into a memory-backed `emptyDir` at mode `0600`, and it removes anything in
that directory that the Secret does not carry. Every file it writes belongs to
the user that then reads it, and no `securefile` rule changes. The compose
stack answers the same problem in the same way; read
`deploy/runtime/install-material.sh`.

The init container then reads every installed file back through `securefile`,
with the same permission profile that the service applies. **A missing Secret
key stops the pod in the init container, with a message**, rather than in a
restart loop of the service.

The service container never mounts the Secret. It sees only the copies.

Two settings go with it:

- `runtime.material.secretDefaultMode` (default `0444`) is the mode the kubelet
  gives each Secret key. The init container has to read them, and the kubelet
  owns them as root, so without a pod `fsGroup` the read bit for other users is
  the only one that reaches this pod's user. Those bits apply inside this pod's
  own mount namespace; the copies that the service reads are owner-only. Set
  `podSecurityContext.fsGroup` and lower this to `0440` to tighten it. The
  chart refuses a mode that the pod could not read.
- `runtime.material.sizeLimit` (default `8Mi`) bounds the `emptyDir`.

`runtime.material.volume` remains, for a deployment whose material is
**already** real, owner-owned files at `runtime.material.mountPath` — a CSI
secret driver, for example. It is mutually exclusive with `secretName`, and it
renders no init container. Exactly one of the two is required.

Set `runtime.enabled: false` if you have no material yet. Everything else above
still works without it, and it is the larger half of the gap.

### The authentication material (issue #444)

Production authentication reads **five more files** through the same
`securefile`: the Auth Redis password, the Auth Redis CA, the browser-attempt
key, the PAT signing key and the Form users JSON. `runtime.enabled` requires
production authentication, so every Kubernetes install of the runtime plane
needs them.

Their paths are **not** chart values. They come from the authentication
configuration document, which is the operator's. So the chart could not know
them, rendered no volume for them, and no Kubernetes install could start from
the chart alone.

**Put the document in the chart.** `fileConfig.authConfig.document` takes the
whole authentication configuration. The chart then renders the ConfigMap for
you, reads the five paths out of it, and refuses — while it renders — a path
that `fileConfig.authConfig.material.mountPath` cannot serve.
`fileConfig.authConfig.configMapName` still points at a ConfigMap you provision
yourself, and the two are mutually exclusive. With the external ConfigMap the
chart cannot read the paths, so only the init container can check them.

The five files arrive in a **plain Kubernetes Secret**, named by
`fileConfig.authConfig.material.secretName`. **Its keys are yours, not the
chart's**: each key is the last component of one of the five paths in the
document. Unlike the runtime material, no script fixes those names.

The mechanism is the one issue #404 built, and there is only one copy of it —
`internal/security/materialinstall`. The init container `auth-material` runs the
same image and the same user as the service, copies each Secret key into a
memory-backed `emptyDir` at mode `0600`, removes anything the Secret does not
carry, and reads every file back through `securefile` before it exits.

One difference decides its arguments. The chart owns every runtime file name, so
`elitea-runtime-material` derives its whole destination from the ConfigMap. The
five authentication paths belong to the operator's document, so
`elitea-auth-material` **reads that document**: `-config` gives it the file the
service reads, and it derives the five paths and their directory from it.
`-mount` states what the pod mounts, and the command refuses a disagreement by
name.

`fileConfig.authConfig.material.mountPath` must differ from
`runtime.material.mountPath`, and the chart refuses one shared directory. Each
install container removes anything in its directory that its own Secret does not
carry, so one directory would make the two delete each other's files. Put a copy
of any shared file, such as the Redis CA, in both Secrets.

`fileConfig.authConfig.material.secretDefaultMode` and `.sizeLimit` behave
exactly like their `runtime.material` counterparts, and
`fileConfig.authConfig.material.volume` is the same alternative for a CSI secret
driver.

### The public project: set `platform.aiProjectId`, once

```yaml
platform:
  aiProjectId: "1"   # the project whose `shared = true` configurations everyone uses
```

Three components need this id, and each one used to take its own copy:
`main.env.ELITEA_AI_PROJECT_ID`, `llmGateway.env.ELITEA_AI_PROJECT_ID` and the
SPA's `web.env.VITE_PUBLIC_PROJECT_ID`. `platform.aiProjectId` fills all three.

**Two copies that disagree produce no error.** elitea-main merges that project's
configurations into every other project's option lookups and the admin provider
surface WRITES a shared credential into `p_<id>`; the gateway RESOLVES shared
credentials out of `p_<id>` (issue #316); the SPA decides which project is
public with a third copy. When they differ, the credential is stored, listed and
reported healthy, and resolves for nobody — with every pod Ready and nothing
logged.

`helm template` now refuses a values file whose copies disagree, and elitea-main
refuses to start when the environment names two different projects. A values
file that still sets the component keys keeps working while the values agree.

It must name a project that EXISTS: the id becomes the PostgreSQL schema name
`p_<id>`, so an id with no schema fails every credential read. It ships **empty**
for that reason. Empty also matters for a default install: with
`ELITEA_CONFIGURATIONS_ENABLED` off, elitea-main refuses to start when
`ELITEA_AI_PROJECT_ID` is present at all. The SPA still gets `"1"` when nothing
names a project, which is what every reference deployment here uses.

The SPA no longer depends on its own copy for correctness. elitea-main publishes
the resolved id on `GET /api/v2/elitea_core/platform_settings/prompt_lib`
(`public_project_id`), and the browser prefers that; `VITE_PUBLIC_PROJECT_ID`
is the fallback for a deployment too old to send the key.

### `GATEWAY_EGRESS_ALLOWLIST` is the bootstrap floor, not the whole policy

`llmGateway.env.GATEWAY_EGRESS_ALLOWLIST` names the hosts a provider credential's
`api_base` may point at. It is the FLOOR. The gateway also reads
`egress_allowlist` rows from `gateway.governance_config`, and it enforces the
UNION of the two. Edit the rows at runtime in **Admin → LLM Proxy →
Governance**; no chart edit and no pod restart are necessary.

An authored row can only ADD a destination. Nothing an admin authors withdraws a
host the chart named, because the chart and the admin console are different
authorities: a mistaken admin session must not cut the platform off from its own
provider, when the recovery would then need a database edit rather than a chart
rollback.

Set the environment variable for the hosts that must be reachable before anybody
can log in — the platform's own provider — and author the rest.

**One grammar serves both.** An entry is a host, a `host:port`, a `*.domain`
wildcard, or a CIDR block:

```
api.openai.com
*.openai.azure.com
vllm.ml.svc.cluster.local:8000
192.168.29.60:8000
192.168.29.0/24
```

**The first entry turns the restriction on.** With no entry in either source, a
credential may name any public host. Add one entry anywhere and every credential
must then match the list.

**A private endpoint needs its address named, not only its hostname.** bifrost's
SSRF-safe dialer refuses RFC 1918 and loopback destinations unless an entry
EXPLICITLY names a private address or block. A hostname alone does not unlock it:
the gateway never resolves a name to make this decision, because resolving a name
here and dialing it later is the DNS-rebinding race a name allowlist avoids. To
reach a self-hosted vLLM at `http://192.168.29.60:8000/v1`, name the address; to
reach one behind a name, name both:

```
vllm.ml.svc.cluster.local:8000
10.0.0.0/8
```

Link-local addresses (`169.254.0.0/16`, `fe80::/10`) stay refused whatever the
allowlist says. They carry the cloud instance-metadata endpoints.

**This changed in the G6 release.** The private-network exemption used to follow
from "is any allowlist configured", so an allowlist of public SaaS hosts relaxed
the dialer for the whole private network. It now follows from an entry naming a
private destination. An existing install that reaches a private endpoint through
a HOSTNAME must add that host's block or address.

`GET /governance/status` on the gateway reports the merged list, tagged by
source, and the admin **LLM Proxy → Status** tab shows the same report: what the
chart contributed, what governance contributed, what is in force, and whether a
private destination is reachable at all.

### `GATEWAY_NATS_URL` — budgets are stored without it and enforced only with it

Admin → Budgets writes `gateway.project_budget` and `gateway.user_budget`.
The LLM gateway is what enforces those rows, and it enforces them through a
NATS JetStream counter. `GATEWAY_NATS_URL` names that cluster.

Leave it empty and the gateway starts, logs a warning, and serves `/llm` with
**no budget enforcement**. Nothing else changes shape:

- every budget write still answers 200;
- every limit still reads back exactly as it was authored;
- Settings → Usage still reports the period's accrued spend;
- and no call is ever refused.

That is the failure this key produces. An operator sets a ceiling, sees it on
the screen, and it stops nothing. Two surfaces report the real state:

- the gateway's `GET /governance/status`, proxied at
  `GET /api/v2/admin/gateway/status`. Its `rate_limits_enforceable` is false
  exactly when the gateway holds no counter, and that counter is the one the
  budget path admits against too;
- Admin → Budgets, which raises a warning banner from that field once any
  budget row exists.

`deploy/docker-compose.standalone-full.yml` runs **no NATS service** and leaves
`GATEWAY_NATS_URL` unset deliberately — see the comment above its
`elitea-llm-gateway` service. Budget authoring and the usage read are fully
exercisable there; budget enforcement is not, and the banner says so. Add a
JetStream service and set the key to exercise enforcement.

Spend accounting needs the gateway but not the counter: the accumulators the
Usage page reads are written by elitea-scheduler's write-back consumer from the
gateway's billing deltas.

## What a Kubernetes install does NOT give you

Stated plainly, because the gap between compose and Helm is where deploys break:

- **No PostgreSQL and no Redis.** No chart here provisions them. The migration
  hook fails against a cluster where they do not already exist, and against one
  where `postgresql.existingSecret` and `redis` have not been pointed at them.
  The table in [Values an operator supplies](#values-an-operator-supplies-and-where-each-one-goes-475)
  names both.
  The database itself may be **empty**. `elitea-migrate` embeds the pylon-era
  schema (`internal/infra/db/migrations/001_initial.sql`) and applies it first
  when the database does not carry it, then applies the shared and tenant
  histories. So the migration hook does build the schema, and nobody runs SQL
  by hand before the install (#556). Two preparations stay outside it, because
  both need rights the migrating role does not hold: `CREATE DATABASE` for the
  agent-state store and `CREATE EXTENSION vector`. The `dbInit` Job does them,
  and it needs an administrator DSN.
- **No browser edge by default, and no securityContext hardening.** The charts
  render neither an Ingress nor an HTTPRoute until `main.ingress.enabled` is
  on, so a default install leaves `elitea-web` and `elitea-main` reachable
  in-cluster only. Turning it on renders ONE `/` rule with elitea-main as the
  only backend. A cluster that also serves the web app from `/app` needs a rule
  per root-mounted Go route, or the SPA backend swallows them:
  `deploy/gateway-api/httproute.yaml` is the reviewed edge for that shape, and
  it must stay in step with `deploy/traefik/dynamic.yml`. Both are walked by
  `services/elitea-main/tests/deployedge/`, so a new root-mounted family fails
  CI until every edge routes it (#568).
- **Cross-namespace DNS.** NATS and the gateway live in `elitea-gateway`; the
  rest live in `elitea`. Short names do not resolve across namespaces, so
  `LLM_GATEWAY_URL` and `GATEWAY_NATS_URL` must be FQDNs
  (`…​.elitea-gateway.svc.cluster.local`).
- **Cross-namespace *Secrets*, which DNS advice does not solve.** The gateway
  chart's cert-manager `Certificate` for the edge issues Secret
  `elitea-main-gateway-client-tls` **into the gateway's own namespace**
  (`elitea-gateway`), and its comment says elitea-main mounts it. Secrets are
  namespace-scoped, so elitea-main running in `elitea` **cannot read it**. To
  wire the elitea-main → gateway mTLS hop you must do one of:
  1. install elitea-main into `elitea-gateway` (override the Application's
     `destination.namespace`), or
  2. replicate the Secret into `elitea` (reflector/kubed, external-secrets, or
     a second `Certificate` in `elitea` from the same `elitea-internal-ca`
     ClusterIssuer — the issuer is cluster-scoped, so this works), or
  3. there is no third option: plain HTTP is **not** one.
     `internal/llmproxy/proxy.go` builds an mTLS transport whenever
     `Config.Transport` is nil, and nothing binds that field to an environment
     variable, so an `http://` gateway URL still loads a client keypair and
     still fails at boot without one.
  The mount itself is no longer missing. `LLM_GATEWAY_CLIENT_CERT` and its two
  siblings are *file paths* (`llmproxy.Config.ClientCertFile`), and issue #463
  moved them out of the `secrets:` block — where a `secretKeyRef` had been
  setting each variable to a PEM block instead of a path — without mounting
  anything at them, so every Kubernetes install with a gateway URL exited at
  boot. `fileConfig.llmGatewayClientMaterial` now mounts a Secret at the
  directory those paths live in, and the chart refuses, while it renders, a
  gateway URL with no material and a path the mount does not serve.
- **No secrets.** Every chart sources sensitive values from Kubernetes Secrets
  that must be provisioned out-of-band. `elitea-main`'s and the gateway's
  `GATEWAY_IDENTITY_SECRET` are `optional: false` — pods do not start without
  them, and the two sides must carry the **same** value. `SECRETS_MASTER_KEY`
  is `optional: false` for the same reason, described next.

## `SECRETS_MASTER_KEY` — one key for the whole stack

`elitea-main` and `elitea-llm-gateway` both read `centry.secrets_key`. Each one wraps a project key with `SECRETS_MASTER_KEY`
when it holds that value, and stores the project key in the clear when it does
not. Two services with two answers put two row formats in one table, and
neither can read what the other wrote. `pylon-indexer` was the third reader
until issue #339 deleted it, which is one fewer place for those two answers to
disagree.

So the rule is: **one stack, one value, given to every service that reads that
table.** Give it in the environment, never in a file. A committed default is a
second key source, and the one `pylon-indexer` used to ship is treated as
exposed (issue #418).

Set it to a base64url-encoded 32-byte Fernet key:

```bash
export SECRETS_MASTER_KEY=$(python3 -c \
  'import base64, os; print(base64.urlsafe_b64encode(os.urandom(32)).decode())')
```

### The three states

The variable has three states. They are not equivalent:

| State | What `elitea-main` does | What is stored |
|---|---|---|
| Set, valid | Wraps each project vault key with the master key. | The key row is a Fernet token. |
| **Not set** | Starts, and writes a **warning** to the log. | The key row is the project key **in the clear**. Anyone who can read the database can open every project secret. |
| **Set, malformed** | **Refuses to start.** The message names the variable. | Nothing. |

A malformed key stops the service on purpose (#412). Before that change the
service ignored the bad value and stored the keys unwrapped. An operator who
set the variable got plaintext storage, and no report of it.

A trailing newline is **not** malformed. Go and Python both ignore `\r` and
`\n` when they decode base64, so a key mounted from a file keeps working. A
stray space or tab **is** malformed.

### Which stack sets it

- `deploy/docker-compose.yml` requires the variable for both services that read
  the table, and compose fails if you do not export it (#418).
- `docker-compose.staging.yml` requires it from your shell, and compose fails
  if you do not export it.
- No chart under `deploy/helm/` sets it. Supply it through a Kubernetes
  Secret, or accept unwrapped storage.
- The E2E stack sets no key on purpose. It seeds unwrapped key rows, so it
  needs none.

### Changing the key

Rows written under a different key, or under no key, do not become readable
when the key changes. Convert them with
[`scripts/rewrap-centry-vault.py`](scripts/rewrap-centry-vault.py), on a copy
first. It rewraps the project key and never rewrites the secret values.

## CI

`.github/workflows/helm-lint.yml` has three jobs:

1. **Helm Lint** — `helm lint` over every chart under `deploy/helm/`. New
   charts are picked up automatically. The job also runs
   `deploy/helm/tests/render-capabilities.sh`, because `helm lint` never reads
   the rendered environment: it stayed green for as long as the chart set no
   capability flag at all (#382). Two coverage checks live here
   rather than in the jobs they guard: every chart directory must appear in
   the **Helm Template** matrix below or be excluded by name with a reason,
   and every chart directory must appear in the `chart` matrix of
   `publish.yml`. A chart that no release publishes is a chart nobody outside
   this repository can install, and neither `helm lint` nor the template
   matrix would ever go red for it.
2. **Helm Template (per chart)** — `helm template` with the chart's values
   files, *and* a second pass with its non-default toggles (HPA, PVC, optional
   Services and probes, hook Jobs render zero objects otherwise, so a break in
   them would be invisible). `elitea` gets a third pass with
   `values-standalone.yaml`, which renders the runtime material Secret volume,
   the material init container and the three runtime listener ports that no
   other pass produces. Every pass is
   validated with `kubeconform -strict`. A new chart must be **added to this
   matrix** by hand.
3. **ArgoCD Applications** — `kubeconform -strict` against the real
   `argoproj.io` Application CRD schema, plus structural checks that no schema
   can make: a stray manifest directly in `deploy/argocd/` that the root never
   renders, a `spec.source.path` pointing at a chart that does not exist, a
   non-Application manifest in `applications/`, and a child with no sync-wave.
   The **Helm Lint** job adds the value check that no schema can make either
   (#475): every Application that syncs an in-repo chart must declare
   `spec.source.helm`, and the `elitea` Application must render from what it
   declares. An Application that declares nothing renders the chart from its
   own defaults, and the platform chart refuses those defaults.

Validation is `kubeconform`, **not** `kubectl apply --dry-run=client`, even
though the latter is what issue #240 asked for: that command needs API
discovery from a live cluster (`couldn't get current server API group list`)
and fails on a runner with no cluster, so it would gate nothing. kubeconform is
the offline equivalent and is strictly stronger — `-strict` rejects fields the
schema does not define, which client dry-run does not. Its CRD schema source is
pinned to a catalog release tag (`CRD_SCHEMAS` in the workflow), so CI does not
depend on a third-party branch.
