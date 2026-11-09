---
title: "Model calls from the worker and the engines go over ModelService (gRPC) on :9446"
issue: 1366
date: 2026-10-21
order: 12
---

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
`deepwiki` profile can run only the fixture engine runner. (Since fixed: see
"Dev compose: object storage, the platform listener and elitea-vector for
the `deepwiki` profile" above.)
