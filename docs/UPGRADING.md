# Upgrading

Changes that need an operator to act when a deployment moves to a newer
release. Each entry says what changed, who is affected, and what to do. The
newest entry is first.

## A direct client is keyed by its socket address; compose trusts no proxy

**Affects:** every install with a Form authentication document
(`trusted_proxy_cidrs`), and compose stacks built from
`deploy/runtime/auth.form.yml` (#1359).

**What changed.**

* elitea-main is now the compose browser edge, with no proxy in front. The
  compose document used to trust all of RFC1918 (the compose network), so a
  browser's own `X-Forwarded-For` was believed and a client could name a
  fresh address per request to get a fresh login attempt window.
  `deploy/runtime/auth.form.yml` now trusts loopback only (`127.0.0.1/32`,
  `::1/128`), which in a compose stack means no proxy at all.
* A request from a peer outside `trusted_proxy_cidrs` is keyed by its socket
  address, and its `X-Forwarded-For` is ignored. Before, the Form login and
  logout routes answered 503 to such a peer.

**What to do.**

* Compose: nothing. Recreate the stack to pick up the new document.
* Kubernetes: `trusted_proxy_cidrs` (and `ELITEA_TRUSTED_PROXY_CIDRS`) must
  name only the ranges your Gateway or ingress controller forwards from. The
  example overlays (`values-auth-minimal.yaml`, `values-standalone.yaml`,
  `deploy/kind/values-kind.yaml`) name the whole RFC1918 space as a pod
  network placeholder; narrow it to your pod CIDR. Never include a range a
  browser reaches elitea-main from directly (a NodePort or LoadBalancer
  Service). The chart sets no default.

## Artifacts and internal MCP tools go over gRPC on :9446; the HTTP callback origins are gone

**Affects:** every install with `main.runtime.enabled`, every install that
runs the agent worker, DeepWiki or Inventory, compose stacks built from
`docker-compose.standalone-full.yml`, `docker-compose.yml` and
`docker-compose.e2e-standalone.yml`, and the kind stack (#1359 U5).

**What changed.**

* The last internal HTTP hops into elitea-main are gRPC on its platform
  listener (`:9446`), beside ModelService:
  * **`elitea.runtime.v1.ArtifactObjectService`**
    (`libs/proto/elitea/runtime/v1/artifact_object.proto`): the worker's
    artifact reads and writes (Office toolkits, indexing artifact sources),
    the DeepWiki/Inventory **hosts'** page and graph uploads, and the
    engines' artifact-folder reads. Each RPC runs the same artifact route a
    browser request takes. Refusals carry the `elitea-artifact-refusal-bin`
    trailer.
  * **`elitea.runtime.v1.InternalToolService`**
    (`libs/proto/elitea/runtime/v1/internal_tool.proto`): the worker's
    internal MCP toolkits (`ListTools`, `CallTool`, which replaced its MCP
    HTTP calls to `/app/{project}/mcp/{scope}`) and the Inventory engine's
    `investigate` source-tool calls (**`InvokeToolkit`**, which replaced its
    HTTP call to `POST /api/v2/elitea_core/test_tool/prompt_lib/{project}/{toolkit}`).
    Refusals carry the `elitea-internal-tool-refusal-bin` trailer. External
    MCP clients keep the HTTP MCP route on the public listener.
* **New elitea-main settings** `ELITEA_ARTIFACT_SERVICE_CLIENTS` and
  `ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS`: the workload identities
  (`dns:<name>` or `spiffe://…`, comma-separated) admitted to each service.
  **An empty list leaves the service unregistered**, so every caller is
  refused; elitea-main still starts.
* **Retired, refused at boot:** `ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL` (the
  worker's internal MCP origin), `ELITEA_DEEPWIKI_CALLBACK_BASE_URL` and
  `ELITEA_INVENTORY_CALLBACK_BASE_URL` (the providers' artifact origin), when
  set to any non-empty value; the chart refuses the keys even when empty. An
  empty Inventory callback base no longer means "source expansion off":
  source tools always go over InvokeToolkit. elitea-main hands the providers a fixed
  `llm_settings.api_base` of `https://elitea-platform.invalid/llm/v1`, which
  only names the route.
* **Internal MCP catalogue entries use the origin
  `https://internal-tools.elitea.invalid`** (`/app/{project}/mcp/{scope}`
  below it). The origin only identifies an entry as internal; the worker
  never dials it, and calls InternalToolService instead. Anything that builds
  or matches internal MCP entries by the old platform-listener or
  `elitea-platform-edge` origin must use this one.
* **The provider HOST containers now dial the platform listener too**, with
  `ELITEA_<APP>_ARTIFACT_SERVICE_URL` (`https://elitea-main:9446`) and the same
  `ELITEA_<APP>_CALLBACK_CA_FILE` / `_CERT_FILE` / `_KEY_FILE` as the engine,
  so **every** DeepWiki or Inventory install needs the runtime plane and the
  callback client certificate, whatever its engine runner (the fixture runner
  included).
* No provider pod reaches elitea-main on 8080 any more.

**What to do (Helm).**

1. Remove `main.env.ELITEA_DEEPWIKI_CALLBACK_BASE_URL`,
   `main.env.ELITEA_INVENTORY_CALLBACK_BASE_URL`,
   `main.env.ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL`,
   `main.runtime.agentExecutionDispatch.currentMainBaseUrl` and
   `deepwiki.callbackViaPlatformListener` (any value). The chart refuses each
   one with a pointer here.
2. `deepwiki.enabled` and `inventory.enabled` now **always** need
   `main.runtime.enabled` and `<app>.callbackClient.secretName` (`tls.crt` /
   `tls.key` from `subapp-callback-client.crt` / `.key`; one Secret can serve
   both). The chart mounts it with the runtime CA into the host container as
   well, and sets `ELITEA_<APP>_ARTIFACT_SERVICE_URL`; `deepwiki.env` /
   `inventory.env` may not set it.
3. The chart renders both allowlists: the worker's
   `worker.runtimeSession.expectedIdentity` (when `worker.enabled`),
   `dns:elitea-subapp-callback` (artifacts: DeepWiki or Inventory enabled;
   internal tools: Inventory enabled), plus the new
   `main.runtime.artifactService.extraClients` and
   `main.runtime.internalToolService.extraClients`. A worker fleet outside
   this release goes in those lists (`values-standalone.yaml` names the
   default standalone worker identity there). Do not set the env names
   yourself: the chart refuses them.
4. The elitea-main NetworkPolicy no longer admits the DeepWiki or Inventory
   pods on 8080; they are admitted on 9446 only.

New render refusals: the five retired keys above; DeepWiki or Inventory
without the runtime plane or `callbackClient.secretName` (no longer only for
a native engine); `ELITEA_<APP>_ARTIFACT_SERVICE_URL` in `deepwiki.env` /
`inventory.env`; a hand-set `ELITEA_ARTIFACT_SERVICE_CLIENTS` /
`ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS`; a malformed or non-list
`extraClients`; `extraClients` with the runtime off.

**What to do (compose).** `docker-compose.standalone-full.yml` drops the
three retired variables, sets both allowlists on elitea-main
(`${RUNTIME_WORKER_SPIFFE_ID}` plus `dns:elitea-subapp-callback`), and gives
`elitea-deepwiki` / `elitea-inventory` the artifact URL and callback material
from a new volume, `standalone_runtime_subapp_host`, which `runtime-material`
fills for the hosts' uid (65532; the engines' copy is owned by 10001 with a
0600 key). `deploy/runtime/install-material.sh` is updated for it, so recreate
`runtime-material`. `standalone-stack.sh check` now also probes
ArtifactObjectService and InternalToolService with grpcurl.
`docker-compose.e2e-standalone.yml` has no runtime plane but now opens the
platform listener anyway: **elitea-main opens it whenever
`ELITEA_RUNTIME_PLATFORM_ADDRESS` is set, runtime on or off**, because its
services run public routes in process and need no runtime plane. Its
material comes from the new `deploy/scripts/gen-platform-listener-certs.sh`
(off the provider CA), which `apps/elitea-web/scripts/e2e-stack.sh up` runs.
`docker-compose.yml` opens no platform listener, so its providers have no
artifact transport: an invocation that carries a callback bearer fails its
upload, and the error names `ELITEA_<APP>_ARTIFACT_SERVICE_URL`.

**What to do (kind).** `deploy/kind/kind-stack.sh up` now installs the
in-repo NATS chart and its bootstrap, creates `elitea-runtime-material`,
`elitea-worker-material` (the runtime CA only) and
`elitea-deepwiki-callback-client` from `gen-runtime-certs.sh`, and
`values-kind.yaml` turns on the runtime plane's base. Recreate an existing
kind cluster (`down`, then `up`).

## Compose stacks no longer run Traefik; elitea-main is the browser edge

**Affects:** local and CI compose stacks only: `deploy/docker-compose.yml`,
`deploy/docker-compose.standalone-full.yml` and
`deploy/docker-compose.e2e-standalone.yml` (#1359). Kubernetes installs are
unaffected: the Gateway API HTTPRoute (`deploy/gateway-api/httproute.yaml`)
still routes `/app` and `/docs` to elitea-web and terminates TLS, and the chart
never sets the new variable.

**What changed.**

* The `traefik` service and `deploy/traefik/` are gone. elitea-main publishes
  its own `:8080` listener on the port Traefik used, so browser origins stay
  the same:

  | Stack | Browser origin | Override |
  |---|---|---|
  | `docker-compose.yml` | `http://localhost:8080` | (fixed) |
  | `docker-compose.standalone-full.yml` | `http://localhost:8084` | `STANDALONE_PORT` |
  | `docker-compose.e2e-standalone.yml` | `http://localhost:8082` | `E2E_PORT` |

* `ELITEA_WEB_UPSTREAM` (e.g. `http://elitea-web:80`) turns on elitea-main's
  compose edge (`internal/api/webedge`). It relays `/app…` and `/docs…` to
  that origin, except the paths elitea-main serves under `/app`
  (`/app/<n>/mcp[/…]`, `/app/application_icon/`, `/app/application_tool_icon/`),
  redirects `/` to `/app/` with a 302 that keeps the query string, and answers
  404 for `/readyz` and `/startupz` to anything but a loopback caller. The
  client identity headers are stripped by elitea-main itself, as before.
  Unset, nothing changes: elitea-main serves its API and nothing else.
* The relay is plain HTTP. It carries browser traffic, not a service call.

**What to do.**

* Recreate the stack (`podman compose ... up -d --remove-orphans`, or
  `deploy/scripts/standalone-stack.sh up`) so the old `traefik` container is
  removed. Nothing else changes for a browser, Playwright or the OIDC
  redirect URIs, because the published ports are the same.
* A custom compose file that used `deploy/traefik/dynamic*.yml` must set
  `ELITEA_WEB_UPSTREAM` on elitea-main and publish its `:8080` instead.
* Do not set `ELITEA_WEB_UPSTREAM` in a cluster.

## Runtime content listener (:9445) and supervisor owner calls are gRPC

**Affects:** every install with the runtime plane enabled
(`main.runtime.enabled`, `ELITEA_RUNTIME_CONTENT_ADDRESS`): elitea-main, the
agent worker, and the sandbox supervisor; also installs that set
`ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG` (#1359).

**What changed.**

* elitea-main's content listener (`:9445`, same address, TLS files and client
  CA) now serves **`elitea.runtime.v1.RuntimeContentService` over gRPC** and
  no HTTP at all: agent inputs, runtime context and tokens, artifacts, Code
  workspace / debug / platform / recovery documents, toolkit discovery
  results, sandbox bundles and compiled snapshots. Inputs and bundle files
  stream in 64 KiB chunks after a header carrying length and SHA-256. The
  execution claim and fence travel as `elitea-claim-id` /
  `elitea-claim-fence-bin` call metadata (they replaced `X-Elitea-Claim-Id` /
  `X-Elitea-Fence`); a sandbox grant travels in the request (it replaced
  `X-Elitea-Sandbox-Bundle-Grant` / `-Compiled-Grant`). The JSON documents
  themselves are unchanged. The listener also answers `grpc.health.v1` to
  any runtime-CA workload identity, so a client certificate must carry
  exactly one DNS or SPIFFE SAN (the certificates `gen-runtime-certs.sh` and
  the chart issue already do).
* Main's owner calls to the sandbox supervisor (node Code recovery read /
  seal-no-effect, Code platform retained runtime / pending call / reply) are
  RPCs on `elitea.runtime.v1.SandboxSupervisorService`; the JSON-over-HTTP/2
  routes `/elitea.runtime.node-code-recovery.v1/...` and
  `/elitea.runtime.code-platform.v1/...` are gone.
* Health and metrics are unchanged: elitea-main's probes stay plain HTTP on
  `:8080`, the supervisor's readiness stays a TCP check, and neither needs a
  client certificate.

**What to do.**

1. **Roll elitea-main, the agent worker and the sandbox supervisor
   together.** An old worker or supervisor cannot read content from a new
   elitea-main, nor the other way round. With the chart, one `helm upgrade`
   does it.
2. Keep `content_origin` (worker) and the supervisor's content `origin` in
   their `https://host:port` form; the port is now required and is dialled
   as gRPC. `ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG.supervisors[].https_origin`
   keeps its name and form but must now include the port of the
   supervisor's gRPC listener (the one `SandboxSupervisorService` already
   used); an origin without a port is refused at startup.
3. Replace any HTTP probe or `curl` check against `:9445` with
   `/elitea-grpc-health --addr elitea-main:9445 ...` from the elitea-main
   image (the standalone stack's `check` does this). A plain TLS handshake
   check still works.

## Model calls from the worker and the engines go over ModelService (gRPC) on :9446

**Affects:** every install with `main.runtime.enabled` (the platform
listener), every install that runs the agent worker, DeepWiki or Inventory,
compose stacks built from `docker-compose.standalone-full.yml`, and anyone
running `cutover-ctl sse-flush-check` / `overhead-check` (#1359 U2).

**What changed.**

* The agent worker and the DeepWiki/Inventory engines no longer POST to an
  HTTP `/llm/v1` callback. They call **`elitea.llm.v1.ModelService`**
  (`libs/proto/elitea/llm/v1/model.proto`) on elitea-main's platform listener
  (`:9446`), which runs each call through the same `/llm` pipeline a browser
  request takes (bearer, project selection, budget and policy gates). A
  refusal comes back as a gRPC status plus the `elitea-model-refusal-bin`
  trailer carrying the original HTTP status and body.
* **New required elitea-main setting `ELITEA_MODEL_SERVICE_CLIENTS`**: the
  workload identities (`dns:<name>` or `spiffe://…`, comma-separated) allowed
  to call ModelService. elitea-main refuses to start without one while the
  platform listener is open. The worker is its certificate's SPIFFE ID; both
  engines are `dns:elitea-subapp-callback` (the
  `subapp-callback-client.crt` that `deploy/scripts/gen-runtime-certs.sh`
  mints).
* **New engine settings** `ELITEA_DEEPWIKI_MODEL_SERVICE_URL` /
  `ELITEA_INVENTORY_MODEL_SERVICE_URL` (`https://elitea-main:9446`) on the
  engine container, with `ELITEA_<APP>_CALLBACK_CA_FILE` (the runtime CA) and
  `ELITEA_<APP>_CALLBACK_CERT_FILE` / `_KEY_FILE` (the callback client
  certificate). `llm_settings.api_base` (from
  `ELITEA_<APP>_CALLBACK_BASE_URL`) now only names the route and stays the
  **artifact** hop's HTTP origin until #1359 U5. Without `MODEL_SERVICE_URL`
  the engine dials the api_base origin and refuses anything but https (or
  loopback http), so a `http://elitea-main:8080` callback base alone means no
  model calls.
* `deepwiki.callbackViaPlatformListener` now moves only DeepWiki's artifact
  hop. **Keep it off until #1359 U5**: the platform listener serves gRPC
  only, so with the flag on the host's artifact uploads and the engine's
  artifact reads (HTTP) fail there.
* `cutover-ctl sse-flush-check` and `overhead-check` now drive elitea-main's
  public `/llm` with a bearer (`--base-url`, default `$DEPLOYMENT_URL`;
  `--api-key` / `$API_KEY`; optional `--project-id` / `--projects-file`)
  instead of the gateway's old HTTP port; `budget-check --gateway-url` names
  the gateway's gRPC listener.

**What to do (Helm).**

1. **`worker.runtimeSession.expectedIdentity` is required whenever
   `worker.enabled`**: the worker certificate's SPIFFE ID (its one URI SAN;
   `gen-runtime-certs.sh` default
   `spiffe://elitea.standalone/ns/default/sa/agent-worker`). The chart puts it
   in `ELITEA_MODEL_SERVICE_CLIENTS`, and the runtime-session Job checks it
   against the certificate, so a wrong value fails the install. Read it with
   `openssl x509 -in agent-worker-client.crt -noout -ext subjectAltName`.
2. Callers outside this release (a worker fleet of its own, a callback
   certificate with another SAN) go in `main.runtime.modelService.extraClients`
   (list). Do not set `main.env.ELITEA_MODEL_SERVICE_CLIENTS`: the chart
   renders it and refuses a hand-set one.
3. DeepWiki and Inventory with the native engine now **need
   `main.runtime.enabled`** and a callback client Secret: create it from
   `subapp-callback-client.crt` / `.key` (`tls.crt` / `tls.key`) and name it
   in `deepwiki.callbackClient.secretName` (default
   `elitea-deepwiki-callback-client`) and the new
   **`inventory.callbackClient.secretName`** (default
   `elitea-inventory-callback-client`; DeepWiki's Secret can be reused). The
   chart mounts the runtime CA (only `runtime-ca.crt` of
   `worker.materialSecretName`) and the Secret into the engine container and
   sets the four engine variables; `deepwiki.env` / `inventory.env` may not
   set them. The fixture engine runner calls no model and needs neither.
4. Remove `inventory.engine.callbackCa` (refused): the model origin is always
   the platform listener, whose CA the chart mounts.
5. The NetworkPolicy on elitea-main now admits the DeepWiki and Inventory pods
   on 9446 whatever `callbackViaPlatformListener` says; 8080 still admits
   them for the artifact hop.
6. Do **not** add `https://elitea-main:9446` to
   `llmGateway.env.GATEWAY_SELF_LLM_ORIGINS`: that list is HTTP `/llm`
   origins a credential's `api_base` could name, and 9446 is gRPC-only behind
   a runtime-CA client certificate.

New render refusals: an empty `ELITEA_MODEL_SERVICE_CLIENTS` with the runtime
on; the worker without (or with a non-`spiffe://`) `expectedIdentity`; a
malformed `extraClients` entry; `extraClients` with the runtime off; a
hand-set `ELITEA_MODEL_SERVICE_CLIENTS`; a native DeepWiki/Inventory engine
without `main.runtime.enabled` or its `callbackClient.secretName`; the
engines' model-hop variables in `deepwiki.env` / `inventory.env`; and
`inventory.engine.callbackCa`.

**What to do (compose).** Re-run `deploy/scripts/gen-runtime-certs.sh` if
`deploy/certs/runtime/subapp-callback-client.*` is missing.
`docker-compose.standalone-full.yml` sets `ELITEA_MODEL_SERVICE_CLIENTS` on
elitea-main (`${RUNTIME_WORKER_SPIFFE_ID}` plus `dns:elitea-subapp-callback`;
export the same `RUNTIME_WORKER_SPIFFE_ID` you minted with), and
`runtime-material` installs the engines' CA, certificate and key into the new
`standalone_runtime_subapp` volume. `standalone-stack.sh check` now calls
ModelService with grpcurl and expects the pipeline's 401 for its invalid
bearer. The dev `docker-compose.yml` opens no platform listener, so its
`deepwiki` profile can run only the fixture engine runner.

## DeepWiki and Inventory: provider SPI and engine sidecar over gRPC; cancel fan-out over NATS

**Affects:** every install with `deepwiki.enabled` or `inventory.enabled`
(or `ELITEA_DEEPWIKI_ENABLED` / `ELITEA_INVENTORY_ENABLED` on elitea-main),
and compose stacks that run `elitea-deepwiki` / `elitea-inventory` (#1359).

**What changed.**

* elitea-main reaches each sub-application host over **gRPC on mutual TLS**,
  `elitea.subapp.v1.ProviderService` (describe, health, slots, invoke as a
  server stream, get-invocation, cancel), on the host's platform port
  (`9443`, `platform-grpc`). The host's HTTPS SPI listener (`8443`) and
  elitea-main's reverse proxy to it are gone. The browser-facing
  `/api/v2/deepwiki` and `/api/v2/inventory` routes are unchanged.
* The host and its engine sidecar speak **gRPC over the shared Unix socket**,
  `elitea.engine.v1.EngineService` (no TLS: the socket directory is the only
  way in). The NDJSON-over-HTTP protocol (`/engine/invoke`, `/engine/health`)
  is gone. **Roll the host image and the engine image together**: an old
  host cannot talk to a new engine, nor the other way round. Both run in one
  Pod, so one `helm upgrade` does it.
* A cancel that lands on one host replica now stops the run on whichever
  replica holds it: the hosts publish it on core NATS,
  `elitea.subapp.<app>.cancel.<invocation>`, in a new **`SUBAPP`** account
  (identities `elitea-deepwiki`, `elitea-inventory`; no JetStream).

**What to do.**

1. Remove `main.env.ELITEA_DEEPWIKI_BASE_URL` / `ELITEA_INVENTORY_BASE_URL`.
   The chart and elitea-main refuse them. The chart derives
   `ELITEA_<APP>_PLATFORM_GRPC_TARGET` (`elitea-<app>-svc:9443`); set it
   yourself (host:port, no scheme) only for a provider outside this release.
2. Remove `deepwiki.service.port` / `inventory.service.port` (refused): the
   Service exposes `platformGrpc.port` only. `platformGrpc.mtls` is
   mandatory and `platformGrpc.clients` must name elitea-main (the default).
3. Unset `ELITEA_<APP>_LISTEN_ADDR` on the host (refused at start); it
   listens on `ELITEA_<APP>_PLATFORM_GRPC_ADDR` (default `:9443`) and
   requires `ELITEA_<APP>_PLATFORM_CLIENTS`.
4. Upgrade the NATS chart (`deploy/helm/nats`) **before** the platform: it
   adds the `SUBAPP` account, and its guards refuse a values file that
   declares accounts without it. With NATS TLS on, the platform chart issues
   the `elitea-deepwiki` / `elitea-inventory` client certificates and mounts
   them. Without `ELITEA_<APP>_NATS_URL` the host starts and logs that the
   fan-out is off; that is fine for one replica only.
5. Compose: the hosts serve gRPC on 9443 only; elitea-main reads
   `ELITEA_<APP>_PLATFORM_GRPC_TARGET=elitea-<app>:9443`
   (`deploy/scripts/gen-nats-certs.sh` now also mints the two SUBAPP
   identities for the secured NATS overlay).

## The platform-edge Traefik is gone; elitea-main serves the worker over gRPC + mTLS on :9446

**Affects:** every install that runs the agent worker (`worker.enabled=true`),
sets `main.runtime.currentMainBaseUrl` to an https origin, or sets
`deepwiki.callbackViaPlatformEdge`. Compose: `docker-compose.standalone-full.yml`
(#1359).

**What changed.** The worker's `platform_origin` (its `/api/v2` + `/llm` calls,
the Office host, indexing artifact reads and internal MCP tools) used to go
through `elitea-platform-edge`, a second Traefik with its own certificate.
elitea-main now serves that surface itself on an internal **platform
listener**, `main.runtime.listeners.platformAddress` (default `:9446`), over
TLS 1.3 that **requires a client certificate signed by the runtime CA**. It
reuses the content listener's keypair (`content-server.crt`, SAN
`elitea-main`). The NetworkPolicy admits the worker (and, with the new flag
below, DeepWiki) to 9446.

**:9446 is a gRPC listener, not HTTP.** Every internal hop is gRPC over mTLS:
the port speaks HTTP/2 gRPC only (ALPN `h2`), and each gRPC service on it
admits only the workload identities (certificate SPIFFE URI or DNS SAN)
configured for it. It serves no REST route: `curl`, `openssl s_client` with an
HTTP request line, or an HTTP liveness probe against 9446 no longer gets an
answer. Check it with gRPC health (`grpc.health.v1.Health/Check`, open to any
runtime-CA client with a workload identity), for example with the
`elitea-grpc-health` tool in the elitea-main image:
`/elitea-grpc-health --addr elitea-main:9446 --ca runtime-ca.crt --cert
agent-worker-client.crt --key agent-worker-client.key`. The address,
certificate and chart/env names (`ELITEA_RUNTIME_PLATFORM_*`,
`main.runtime.listeners.platformAddress`) are unchanged.

**What to do.**

1. Delete `worker.platformEdge` from your values. The chart refuses to render
   while `worker.platformEdge.enabled` is true, or while
   `worker.runtime.platformOrigin` still names `elitea-platform-edge`.
2. Set `worker.runtime.platformOrigin: https://elitea-main:9446` (the new
   default) if you override it.
3. Delete the `elitea-platform-edge-tls` Secret; nothing reads it.
4. elitea-main no longer calls itself: the next-input-suggestion policy
   lookup is in process. `main-platform-client.crt` / `.key` and
   `ELITEA_RUNTIME_PLATFORM_CLIENT_CERT_FILE` / `_KEY_FILE` are not read any
   more; remove them from the runtime material Secret and any env overrides.
   `main.runtime.currentMainBaseUrl` stays (it is the internal MCP origin
   handed to the worker); `https://elitea-main:9446` needs no Main client
   certificate. `deploy/scripts/gen-runtime-certs.sh` deletes the retired
   files from an existing tree.
5. `deepwiki.callbackViaPlatformEdge` was removed and now fails the render; it
   is not mapped onto the new key because the new hop needs a client
   certificate you have to mint. Use `deepwiki.callbackViaPlatformListener:
   true` and create the `deepwiki.callbackClient.secretName` Secret
   (default `elitea-deepwiki-callback-client`, keys `tls.crt` / `tls.key`) from
   `subapp-callback-client.crt` / `.key` (runtime CA, clientAuth EKU).
6. If `GATEWAY_SELF_LLM_ORIGINS` lists the worker's model origin, replace
   `https://elitea-platform-edge/llm/v1` with `https://elitea-main:9446/llm/v1`.
7. Compose: re-run `deploy/scripts/gen-runtime-certs.sh` (it removes the
   retired `platform-edge.*` and `main-platform-client.*` files and issues the
   sub-application callback identity), then
   recreate the stack. The `standalone_runtime_edge` volume is unused and can
   be removed.

## The X-Auth-* edge identity plane is removed; Form sessions resolve in elitea-main — BREAKING for external auth edges

**Affects:** a deployment whose own edge (outside this repository) calls
`elitea-main`'s `/internal/auth/main` or `GET /auth` as a forward-auth target
(Traefik `forwardAuth`, a Gateway API `ExtensionRef`, NGINX `auth_request`),
or that sends `X-Auth-*` identity headers to `elitea-main` in any other way.
A deployment that uses only this repository's chart or compose files has
nothing to do.

**What changed.** (#1359)

- `GET /internal/auth/main`, `GET /auth` (the edge-auth token check) and the
  Form plane's `/auth/check` are gone and answer 404. No `elitea-main` route
  emits an `X-Auth-*` header any more, and none reads one.
- **Form sign-in sessions now resolve in `elitea-main` itself.** The
  `elitea_browser_auth` cookie that `/auth/form/authorize` sets is read
  in-process by every API route and the admin console, exactly like the single
  sign-on `elitea_session` cookie (including the cross-site write check). No
  edge ForwardAuth is needed for `ELITEA_FORM_LOGIN_ENABLED=true` — before
  this change, a Form sign-in authenticated nothing behind an edge that did not
  run one, which was every edge this repository ships.
- `elitea-main` deletes the inbound identity headers (`X-Auth-Type`,
  `X-Auth-ID`, `X-Auth-User-ID`, `X-Auth-Reference`, `X-Auth-Signature`,
  `X-Auth-Avatar`, `X-Auth-Avatar-State`, `X-Auth-Session-Id`,
  `X-Auth-Session-Name`, `X-Auth-Session-Endpoint`,
  `X-Elitea-Identity-Signature`, `X-Elitea-Project-Id`, `X-Elitea-User-Id`,
  `X-Elitea-Tenant-Id`) on every request before any route runs, so it no
  longer depends on an edge to strip them. `X-Elitea-Execution-Id` is kept: the
  desktop and the runtime worker send it on `/llm`, and `elitea-main` verifies
  it against a live execution of the caller before using it.
- The worker's private edge (`platform-edge`) no longer authenticates the
  execution-events stream: the worker's bearer token is read by
  `elitea-main` directly, as on every other route.

**What to do.** Remove any forward-auth middleware that points at
`/internal/auth/main` or `/auth`; with it in place every request is refused,
because the target answers 404. Route browser traffic straight to
`elitea-main` — it authenticates the Form cookie, the single sign-on cookie and
bearer tokens itself. Keep stripping the identity headers at the edge if you
like; it is now defence in depth rather than the only control. The auth
document is unchanged; its `mappers`, `authorization` and `credential_headers`
sections are still validated but no longer used.

## Default-deny NetworkPolicies; an ingress decision is required

**Affects:** every install of the `deploy/helm/elitea` chart. A CNI that
enforces NetworkPolicy (Calico, Cilium, etc.) is needed for the objects to take
effect.

**What changed.** The chart now renders ingress NetworkPolicies, on by default
(`networkPolicies.enabled: true`):

- `elitea-main`: port 8080 admits the platform edge, the DeepWiki and Inventory
  pods that call back directly, and the peers you list; the runtime listeners
  admit the worker pods only.
- the platform edge: port 443 from the worker, `elitea-main` and (when
  `deepwiki.callbackViaPlatformEdge` is set) DeepWiki; egress only to
  `elitea-main` and cluster DNS.
- the worker: no inbound connections. Worker egress is not restricted.
- the sandbox supervisor: its profile ports admit the worker and `elitea-main`.

**What to do.** The chart cannot see the pods that deliver outside traffic to
`elitea-main:8080`, so it refuses to render until you state who they are:

- list the Gateway API gateway or ingress controller as NetworkPolicyPeer
  entries in `networkPolicies.main.ingressFrom` (or `extraIngressFrom`). This
  includes a Gateway API HTTPRoute applied outside the chart, such as
  `deploy/gateway-api/httproute.yaml`; and `main.ingress.enabled=true` needs
  `ingressFrom` itself; or
- set `networkPolicies.main.noExternalIngress=true` when nothing outside the
  chart's own pods reaches `elitea-main` (for example access by
  `kubectl port-forward` only). It is refused together with
  `main.ingress.enabled=true`.

If your own policy already covers these workloads, set
`networkPolicies.enabled=false` together with
`networkPolicies.externallyManaged=true`. If your CNI applies policy to kubelet
probes, list the probe sources in `networkPolicies.probeFrom`. The Argo CD
sample has a new parameter for the gateway namespace
(`networkPolicies.main.ingressFrom[0]...`) next to the other values to fill in.

**Related, unchanged checks.** A plain `networking.k8s.io/v1` Ingress with Form
sign-in is still refused unless the controller removes the inbound `X-Auth-*`
and `X-Elitea-*` identity headers and
`main.ingress.identityHeadersStrippedByController=true` says so. (Superseded
for `elitea-main` itself by the entry above: it now strips those headers and
serves no forward-auth endpoint.)

## DeepWiki indexes are scoped by project; existing indexes are deleted — BREAKING

**Affects:** every deployment that runs the DeepWiki native engine
(`deepwiki.engine.runner: native`, or `ELITEA_DEEPWIKI_RUNNER=native`).

**Why.** The engine kept every wiki index in one `deepwiki` database keyed
by `wiki_id` alone, and a `wiki_id` is derived from the repository name
(`owner--repo--branch`, `artifact--bucket--prefix--branch`), not from the
project. Each index row now belongs to one project, and every access is
scoped by `(project_id, wiki_id)`. Wiki ids are no longer shared between
projects.

**What changed.**

- Engine migration `0005_project_scope.sql` recreates the index tables
  (`wikis`, `wiki_nodes`, `wiki_edges`, `wiki_node_embeddings`,
  `wiki_bm25_*`) with `project_id` leading every key, and adds
  `project_id` to `deepwiki_build.builds`.
- **Every existing index is deleted**, and every build in progress with it.
  A row written before 0005 cannot be attributed to a project (the same
  wiki id may have been generated by several projects), so it is not kept.
- The generated wiki pages are **not** touched: they live in the platform
  artifact store (the project's `wiki-artifacts` bucket), not in this
  database. The wiki browser lists and shows them as before.
- Until a wiki is generated again, `ask` and `deep_research` on it answer
  "No wiki index found for … Please generate a wiki first." (and the
  wiki_query `resolve_and_ask` / `resolve_and_deep_research` report the same
  failure in band).
- The Go sub-application host stamps the authenticated project on every
  engine call (`_elitea_project_id`), from the verified identity signature
  when `ELITEA_DEEPWIKI_IDENTITY_SECRET` is set on the host, else from the
  `llm_settings.organization` the platform facade writes. The engine
  refuses `generate_wiki`, `ask` and `deep_research` without it.

**What to do.**

1. Upgrade the engine image and the Go host together, and let the rollout
   finish before anyone regenerates a wiki. The migrate Job applies 0005
   before the new pods start. An engine pod from before 0005 cannot open or
   publish a build (it does not write the `NOT NULL` `project_id`), and it
   does not filter its reads by project. Finish the rollout (or scale the
   DeepWiki deployment down) before anyone regenerates a wiki.
2. Set `ELITEA_DEEPWIKI_IDENTITY_SECRET` on BOTH elitea-main and the
   DeepWiki host (`deepwiki.secrets.ELITEA_DEEPWIKI_IDENTITY_SECRET`) if you
   have not: without it the host has no verified identity and falls back to
   the facade-written `llm_settings.organization`, which is only as
   trustworthy as the mTLS hop to the host.
3. Regenerate the wikis whose chat (`ask` / `deep_research`) your users
   need. The pages of a wiki keep working without it.

## DeepWiki runs the native engine; the Python engine image is gone — BREAKING

**Affects:** every Helm install with `deepwiki.enabled: true`, every values
file or Argo CD Application that sets `deepwiki.engine.*` or
`deepwiki.env.ELITEA_DEEPWIKI_RUNNER`, and compose stacks started with
`deploy/docker-compose.deepwiki-native.yml`.

**What changed.**

- The DeepWiki engine sidecar is always the Rust-native engine,
  `ghcr.io/elitea-ng/elitea-deepwiki-engine-native` (ADR-0026). The chart no
  longer runs the Python `ghcr.io/elitea-ng/elitea-deepwiki:<tag>-engine`
  image, as the sidecar or as the migrate Job.
- The default changes from `legacy` to `native`:
  `deepwiki.engine.runner: native` and
  `deepwiki.env.ELITEA_DEEPWIKI_RUNNER: "native"`.
- `deepwiki.engine.runner` accepts `native` (the engine) and `fixture`
  (canned results, for a stack that proves the hop). Both run the native
  image; `fixture` reads no database.
- `legacy` is **refused**, not aliased, on both settings, with a message
  that names this entry. It named the Python engine, and a silent alias would
  hide that the engine, its image and its resource needs all changed.
- `deepwiki.engine.image` and `deepwiki.engine.resources` (the Python
  sidecar's) are gone, and the chart **refuses to render** a values file
  that still sets either. The native sidecar's keys are unchanged:
  `deepwiki.engine.native.image`, `.resources`, `.workerCpuSeconds`.
- The `native` runner needs an `ELITEA_DEEPWIKI_DATABASE_URL` secret:
  `deepwiki.secrets.ELITEA_DEEPWIKI_DATABASE_URL`, else
  `postgresql.existingSecret`. The chart refuses to render without one (a
  plain `deepwiki.env` value does not reach the migrate Job). The engine
  keeps every index in that database.
- The migrate Job always runs the native image's `migrate`. It writes the
  same `schema_migrations` ledger with the same checksums as the Python
  runner, so a database the Python runner migrated needs no extra step.
- Compose: `deploy/docker-compose.standalone-full.yml` runs the native
  sidecar and its `elitea-deepwiki-migrate` service itself, with the
  `fixture` runner by default (`DEEPWIKI_NATIVE_RUNNER=native` for the
  engine). `deploy/docker-compose.deepwiki-native.yml` is deleted. The dev
  `deploy/docker-compose.yml` `deepwiki` profile runs the Go host, the
  native sidecar and the native migration.
- Outside Helm too: the Go host (`elitea-subapp-host`) no longer has a
  `legacy` runner. `ELITEA_DEEPWIKI_RUNNER=legacy` stops it at start with
  a pointer to this entry, as the native engine binary already did.
- The Python service (`services/elitea-deepwiki`) is deleted, and
  `ghcr.io/elitea-ng/elitea-deepwiki` is no longer built or published.
  Tags already published stay in the registry; nothing in the chart pulls
  them.

**What to do.**

1. Remove `deepwiki.engine.image` and `deepwiki.engine.resources` from every
   values file. Move any image or resource override to
   `deepwiki.engine.native.image` / `deepwiki.engine.native.resources`
   (`limits.memory` in Gi or Mi, at least 1205Mi; it sets the generation
   worker's address-space cap).
2. Replace `runner: legacy` and `ELITEA_DEEPWIKI_RUNNER: "legacy"` with
   `native`, or delete them to take the default.
3. Make sure a database URL secret reaches the component (step above). Its
   role needs `CREATE` on the database for migration 0003 (see
   `services/elitea-deepwiki-engine/README.md`).
4. Mirror `ghcr.io/elitea-ng/elitea-deepwiki-engine-native` instead of
   `elitea-deepwiki` if you pull through a registry mirror.
5. Compose: drop `deploy/docker-compose.deepwiki-native.yml` from
   `STANDALONE_OVERLAY`; the base file now does what it did.

## runtime-redis removed: the platform runs without Redis — BREAKING

**Affects:** every Helm install whose values still carry a `runtimeRedis`
block (including `enabled: false`), every Argo CD Application built on those
values, every compose stack started from `deploy/docker-compose.standalone-full.yml`,
and anyone using the mixed `deploy/centry-hybrid` stack.

**What changed.**

- Nothing reads Redis any more. The runtime command bus is NATS JetStream
  (`docs/runtime-command-bus.md`), the Form sign-in state is in PostgreSQL
  (entry below), and live updates and canvas presence are on NATS. The plain
  `redis`/`elitea-valkey` was already gone.
- The chart no longer has `templates/runtimeRedis/` or a `runtimeRedis` values
  block, and it **refuses to render** a values file that still sets
  `runtimeRedis` at all (`runtimeRedis.enabled`, `.materialSecretName`,
  `.persistence`, `.bootstrap`, `.service`, … — any key under it), with a
  message naming this entry. The worker keys `worker.runtime.redisUrl`,
  `redisStream`, `redisGroup`, the `redis_*` limits, `main.runtime.redis` and
  an auth document `redis:` block were already refused.
- Compose: the `runtime-redis` service, its `standalone_runtime_redis` material
  volume, its `standalone_runtime_redis_data` AOF volume and
  `deploy/runtime/redis.conf` are gone.
- `deploy/scripts/gen-runtime-certs.sh` no longer mints `redis-server.crt`/`.key`,
  `redis-users.acl` or the `redis-{producer,worker,bootstrap}-password` files,
  and deletes them (and a leftover `redis-auth-password`) from an existing
  `deploy/certs/runtime/` tree in place. Nothing else in the tree is rotated.
  The runtime CA stays: it signs elitea-main's control/output/content listeners
  and the worker's workload identity.
- `deploy/centry-hybrid` is **retired** (owner decision: retire, don't port).
  It depended on the private legacy centry repository, and its runtime still
  wrote the Redis command-bus fields both workers refuse. The `hybrid:*` Task
  targets, the cutover-rehearsal suite and the Centry-profile reliability
  harnesses (`TestExistingComposeIndexReliability`, the issue #5681
  production-scale gate) went with it. `deploy/scripts/migrate-artifacts.sh`
  (the libcloud → object-store copy, formerly under the hybrid's `scripts/`)
  is kept; `deploy/ARTIFACT_CUTOVER.md` describes its use.

**What to do.**

1. Delete the `runtimeRedis` block from every values file (and from Argo CD
   `valuesObject`/`helm.values`). The render fails until you do.
2. After the upgrade, delete what the old release or your own tooling left
   behind. A `helm upgrade` removes the chart-owned Deployment, Service and
   ConfigMap; the data PVC always survives (the chart annotated it
   `helm.sh/resource-policy: keep`), as does anything an Argo CD
   `Prune=false` or a storage-class retain policy kept, and the material
   Secret was always yours:

   ```bash
   kubectl -n <ns> delete deployment,service elitea-runtime-redis --ignore-not-found
   kubectl -n <ns> delete configmap elitea-runtime-redis-config --ignore-not-found
   kubectl -n <ns> delete pvc elitea-runtime-redis-data --ignore-not-found
   kubectl -n <ns> delete secret elitea-runtime-redis-material --ignore-not-found
   ```

   (Names are the chart defaults; use your `runtimeRedis.service.name` /
   `fullnameOverride` if you changed them.) Then release the PersistentVolume
   if its reclaim policy is `Retain`.
3. Drop any `redis-*` keys from the runtime material Secret
   (`main.runtime.material.secretName`) and the auth material Secret if they
   are still there. Nothing reads them, and they are live credentials to a
   server that no longer exists.
4. Compose: `podman compose -f deploy/docker-compose.standalone-full.yml down`
   with `--remove-orphans`, then `podman volume rm <project>_standalone_runtime_redis
   <project>_standalone_runtime_redis_data` (the second exists only on stacks
   that ran the AOF-persistent runtime-redis).
   Re-run `deploy/scripts/gen-runtime-certs.sh` (or `task standalone:up`,
   which runs it) to prune the retired files from `deploy/certs/runtime/`.
5. Mixed-deployment users: there is no in-repo replacement for
   `deploy/centry-hybrid`. Move to the Helm chart or the standalone stack.

## NATS is secured (mTLS, one account per plane) and moves into the platform namespace — BREAKING; budget counters reset

**Affects:** every Kubernetes deployment of `deploy/helm/nats`,
`deploy/helm/nats-bootstrap` and `deploy/helm/elitea`, and every Argo CD
install from `deploy/argocd`. Compose is not affected unless you opt into
`deploy/docker-compose.nats-secure.yml`.

**What changed.**

- The NATS server requires TLS and a client certificate on 4222. Each client's
  certificate carries a URI SAN (`spiffe://elitea.internal/nats/<identity>`)
  that maps to a user (`verify_and_map`); there is no anonymous or password
  access. All URLs are `tls://…` with no credential.
- Users live in one NATS account per plane — `MAIN` (elitea-main: presence
  KV, presence subject), `GATEWAY` (gateway and scheduler: budget and
  rate-limit counters, write-behind deltas, cooldown KV) and `RUNTIME`
  (reserved for the command bus). Only GATEWAY's per-project soft-alert
  subject crosses (into MAIN).
- Canvas presence is published on `elitea.events.project.<id>.presence`
  (was `gateway.events.project.<id>.events`); the SSE route accepts each
  event type only from its own subject.
- The `nats-bootstrap` Job creates every stream, KV bucket and the
  scheduler's `budget-writeback` consumer, once per account with a
  per-account certificate that exists only during the install (a one-hour
  hook Certificate). The scheduler only binds, and keeps retrying until the
  consumer exists (`GET /readyz/budget-writeback`). Its
  `BUDGET_WRITEBACK_ACK_WAIT` / `BUDGET_WRITEBACK_MAX_DELIVER` moved to the
  bootstrap chart's `deltas.writeback.ackWait` / `maxDeliver`.
- In HA, routes use their own route CA and certificate.
- An ingress NetworkPolicy admits 4222 only from elitea-main,
  elitea-llm-gateway, elitea-scheduler and nats-bootstrap pods.
- The Argo CD sample installs NATS and its bootstrap into **`elitea`** (the
  platform's namespace) instead of `elitea-gateway`, because the NATS CA
  Issuer is namespaced and signs the platform's client certificates.

**What this resets.** Moving the NATS Application to another namespace
creates a new StatefulSet with a **new, empty JetStream volume**; nothing is
migrated (greenfield, by decision). After the switch:

- budget counters and rate-limit windows start at zero for the current
  period, so budget **enforcement** for the period restarts from zero (spend
  already written back to `gateway.llm_budget_accumulators` stays, and the
  budget screens keep showing it);
- write-behind deltas the scheduler had not drained yet are lost;
- the soft-alert cooldown resets, so an 80% alert can fire once more;
- canvas presence rosters reset (they rebuild within one heartbeat).

**What to do.**

1. Install cert-manager (a prerequisite now). Optionally install
   cert-manager approver-policy and disable cert-manager's built-in approver;
   the NATS chart then renders policies that pin what its issuers sign
   (`security.approverPolicy`).
2. Restrict `create` on cert-manager `Certificate`/`CertificateRequest` and
   read on `Secret` in the NATS namespace to the deployer and operators: those
   rights can mint any NATS identity (`deploy/helm/nats/README.md`, "Who can
   mint a NATS identity").
3. Install NATS, `nats-bootstrap` and the platform into one namespace (or use
   a `ClusterIssuer`; the platform chart refuses a different `nats.namespace`
   with the namespaced Issuer). With Argo CD, sync the updated
   `deploy/argocd` Applications; the bootstrap runs as a PostSync hook.
4. Optional, to keep the undrained deltas: before switching, scale the LLM
   gateway to zero, wait until the old NATS's `budget-writeback` consumer
   shows no pending messages, then switch.
5. Delete what the old install left in `elitea-gateway`: the JetStream PVC
   (`elitea-nats-js-elitea-nats-0`, and `-1`/`-2` in HA) is not pruned by
   Argo CD, and the namespace itself if nothing else uses it.
6. Remove `BUDGET_WRITEBACK_ACK_WAIT` / `BUDGET_WRITEBACK_MAX_DELIVER` from
   any scheduler values override (they are ignored), and set
   `deltas.writeback.*` on the bootstrap if you had tuned them.

## Form sign-in state moved from Redis to PostgreSQL; auth document v2 — BREAKING

**Affects:** every deployment that sets `ELITEA_AUTH_CONFIG_FILE` (Helm:
`main.fileConfig.authConfig.enabled: true`), whether Form sign-in is on or off.

**What changed.**

- The Form graph's sign-in state — browser sessions, one-time login
  transactions and the attempt limiter — is in PostgreSQL now, in the
  `elitea_auth` tables of shared migration 0153. The auth Redis client and the
  `auth` Redis ACL user are gone, and readiness no longer checks Redis.
- The authentication document schema is `elitea.auth.form.v2`. It has no
  `redis:` block, and the attempt key moved to `credentials.attempt_key_file`.
  A `elitea.auth.form.v1` document is **refused**: elitea-main stops at boot,
  `elitea-auth-material` stops at pod start, and the chart refuses to render.
- The material is three files: `attempt_key_file`, `pat_signing_key_file` and
  `users_json_file`. The Redis password and the Redis CA are not read.
- A pre-login session (between the login page and the password submit) lives
  five minutes. An authenticated session still lives `cookie.lifetime_seconds`.
- elitea-scheduler deletes expired sign-in rows, including the OIDC and SAML
  `browser_sessions` rows that nothing deleted before.

**What to do.**

1. In the authentication document: set `schema_version: elitea.auth.form.v2`,
   delete the `redis:` block, and add
   `credentials.attempt_key_file` with the value of the old
   `redis.attempt_key_file`.
2. Run the migrations (the chart's migration Job does) before the new
   elitea-main starts.
3. Optional: drop the `redis-auth-password` and Redis CA keys from the auth
   material Secret, and the `user auth` line from the runtime Redis ACL file
   (the runtime Redis itself is removed since; see the entry above).
   An install still works with them present; nothing reads them.

People signed in through Form sessions are signed out once. OIDC and SAML
sessions are not affected.

## Form (username/password) sign-in is off by default — BREAKING

**Affects:** every deployment whose people sign in through the Form plane —
the local users in the Form users JSON file named by the
`ELITEA_AUTH_CONFIG_FILE` document (Helm: `main.fileConfig.authConfig`) — on a
deployment with no OIDC or SAML sign-in configured. Deployments that sign in
through OIDC or SAML are not affected: Form sign-in was already unmounted
there.

**What changed.** The local username/password sign-in is now controlled by
one switch, `ELITEA_FORM_LOGIN_ENABLED` (Helm: `main.env.ELITEA_FORM_LOGIN_ENABLED`),
and it is **off unless set to `true`**. Sign-in is meant to be OIDC or SAML,
with SCIM provisioning. With the switch off:

- the authentication document is still read and still required where it was
  — the gateway edge, personal access token validation and the runtime's
  forwarded-identity check depend on it;
- no `/auth/form/*` route and no Form `/auth/login` page is mounted, and the
  Form handler holds no users, so no configured password is accepted
  anywhere. With OIDC or SAML configured, `/auth/login` is the SSO sign-in
  page; with neither, it answers 404 and elitea-main logs
  `no browser sign-in is enabled` at start-up;
- a Form session created before the upgrade stops authorizing at once
  (people signed in with a Form password are signed out);
- configured Form users are ignored, and elitea-main logs that at start-up
  (`Form sign-in is disabled …; the configured Form users are ignored`).

Any value other than `true`/`false`/`1`/`0` (any letter case) or empty stops
the boot.
OIDC, SAML and SCIM behave exactly as before.

**What to do.**

- *You sign in with Form users and want to keep doing so:* set
  `ELITEA_FORM_LOGIN_ENABLED=true` before upgrading
  (`values-auth-minimal.yaml` already does). Read the next entry too: every
  Form user now needs a real email address.
- *You sign in with OIDC or SAML:* nothing to do. If your authentication
  document's `identity.initial_global_admins` lists Form logins, they no
  longer match anyone; list `oidc:<sub>`, `saml:<nameid>` or
  `email:<address>` entries instead.

**Making the first administrator of a fresh install** without Form sign-in:

- *OIDC:* set `OIDC_ISSUER_URL` (and the client settings) and name yourself
  in `identity.initial_global_admins` or `ELITEA_INITIAL_GLOBAL_ADMINS` —
  `email:<you@example.com>` works when your identity provider marks the
  address verified, `oidc:<sub>` always does. Your first OIDC sign-in receives
  the administration role.
- *SAML only:* a SAML identity provider is configured in the admin console
  (Configuration › Authentication), so it needs an administrator first, and
  there is no environment variable that configures SAML. Either bootstrap
  through OIDC as above, or, once: set `ELITEA_FORM_LOGIN_ENABLED=true` with
  one Form user (with an `email`) listed in `identity.initial_global_admins`,
  sign in, author the SAML provider and add your `saml:<nameid>` to the
  list, then set `ELITEA_FORM_LOGIN_ENABLED=false` and restart — the restart
  also mounts the SSO sign-in page, which is decided at boot.

## Form users must have a real email address

**Affects:** deployments that sign people in through the Form plane (the
local username/password users in the Form users JSON file named by
`provider.form.users_json_file` in the `ELITEA_AUTH_CONFIG_FILE` document).

**What changed.** A Form user's address is now read from the user's
top-level `email` field — the field the admin schema writes — and, when that
is absent, from `attributes.email`. Before, the top-level field was accepted
and ignored, and a user with no `attributes.email` signed in as
`<login>@centry.user`. That domain is the platform's reserved system-identity
domain (`system@centry.user`, `system_user_<n>@centry.user`): the Users page,
analytics, budgets and SCIM all treat an address in it as a platform account,
so the person disappeared from those surfaces.

No address is synthesized any more, on any sign-in plane. A Form user with no
address, with a top-level `email` that is not a usable address (it contains
whitespace or control characters), or with an address in `@centry.user` is a
configuration error:

- the users file still loads, so one bad entry does not lock the others out;
- elitea-main logs a warning at start-up naming the affected logins
  (`Form users have no usable email address and will be refused at sign-in`);
  `elitea-auth-validate` prints the count;
- that user's sign-in is refused with the ordinary "sign-in failed" page, and
  the server log says why (`Form sign-in refused: the Form user has no email
  address configured`, with the login).

**What to do before upgrading.** Give every Form user a real address:

```json
{"users": [
  {"login": "alice", "password": "…", "email": "alice@example.com"}
]}
```

**Accounts already created as `<login>@centry.user`.** Nothing is rewritten
automatically: which real address belongs to each person is your decision,
and changing an address changes how the account matches future sign-ins. At
start-up elitea-main logs a warning listing such accounts
(`sign-in accounts have a synthesized address in the reserved @centry.user
domain`, with their provider references). An existing account keeps signing
in — it is found by its provider link, not by its address — so you can fix
the rows at your own pace:

1. List them:

   ```sql
   SELECT u.id, u.email, p.provider_ref
   FROM public.auth_core__user AS u
   JOIN public.auth_core__user_provider AS p ON p.user_id = u.id
   WHERE lower(u.email) LIKE '%@centry.user'
     AND lower(u.email) <> 'system@centry.user'
     AND lower(u.email) NOT LIKE 'system\_user\_%@centry.user';
   ```

2. For each person, set the address you configured for them in the users
   file, after checking that no other account already holds it:

   ```sql
   UPDATE public.auth_core__user SET email = 'alice@example.com' WHERE id = <id>;
   ```

The warning stops once no such account remains.
