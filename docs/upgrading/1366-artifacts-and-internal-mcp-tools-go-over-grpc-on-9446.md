---
title: "Artifacts and internal MCP tools go over gRPC on :9446; the HTTP callback origins are gone"
issue: 1366
date: 2026-10-21
order: 15
---

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
(Since fixed: see "Dev compose: object storage, the platform listener and
elitea-vector for the `deepwiki` profile" above.)

**What to do (kind).** `deploy/kind/kind-stack.sh up` now installs the
in-repo NATS chart and its bootstrap, creates `elitea-runtime-material`,
`elitea-worker-material` (the runtime CA only) and
`elitea-deepwiki-callback-client` from `gen-runtime-certs.sh`, and
`values-kind.yaml` turns on the runtime plane's base. Recreate an existing
kind cluster (`down`, then `up`).
