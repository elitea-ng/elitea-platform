# The runtime plane — what `ELITEA_RUNTIME_ENABLED=true` actually costs

This directory holds the non-secret half of the runtime plane for the full
standalone stack: the production auth config and the script that installs the
generated material. The command bus's streams are created by `nats-bootstrap`. The
secret half is minted by [`../scripts/gen-runtime-certs.sh`](../scripts/gen-runtime-certs.sh)
into `deploy/certs/runtime/`, which is gitignored and must stay that way.

Enabling the runtime is what turns on the agent-execution HTTP surface — POST
`/api/v2/elitea_core/messages/prompt_lib/{projectID}/{conversationID}` returning
`{events_url}`, and the SSE stream at
`/api/v2/executions/{projectID}/{executionID}/events`. Nothing else registers
those routes.

It is not a feature flag. `internal/runtimecomposition/config.go` reads the
whole env block all-or-nothing and refuses to boot on a partial one, and each
piece pulls in real provisioning. Read this before setting the flag anywhere
else.

## The six things it drags in

1. **The command bus: NATS JetStream.** Dispatch publishes signed envelopes on
   the JetStream streams `nats-bootstrap` creates
   ([`docs/runtime-command-bus.md`](../../docs/runtime-command-bus.md)).
   `ELITEA_RUNTIME_NATS_URL` is `nats://nats:4222` in compose (plaintext, no
   identity); `docker-compose.nats-secure.yml` switches it to `tls://` with
   the `elitea-main-runtime` client certificate. There is no Redis (Form auth
   keeps its state in PostgreSQL).
2. **Three mTLS listeners, not one.** Control gRPC `:9443`, output gRPC `:9444`,
   content gRPC `:9445` (`elitea.runtime.v1.RuntimeContentService`, #1359), each with its own `_TLS_CERT_FILE`, `_TLS_KEY_FILE`
   and `_TLS_CLIENT_CA_FILE`. All three demand
   `RequireAndVerifyClientCert` — a listener that accepts an anonymous client is
   a misconfiguration, not a relaxed mode.
3. **A worker certificate with exactly one SAN.**
   `internal/auth/workloadidentity` accepts one SPIFFE URI SAN *or* one DNS SAN
   and never falls back to CommonName. Email SANs, IP SANs, several DNS names,
   or a mix of URI and DNS are all rejected. A cert with **no** SAN — which is
   what `gen-gateway-certs.sh` produces for the gateway hop — cannot be reused
   here.
4. **An Ed25519 command-signing keypair.** `ELITEA_RUNTIME_SIGNING_KEY_FILE`
   must be one PKCS#8 `PRIVATE KEY` PEM block and nothing else; the public half
   must appear in the verification keyring JSON under exactly
   `ELITEA_RUNTIME_SIGNING_KEY_ID`. Composition cross-checks the two and refuses
   to start on a mismatch.
5. **Production authentication.** `cmd/elitea-main/main.go:686-688` rejects
   `ELITEA_RUNTIME_ENABLED=true` unless a `PrincipalValidator` and the Form
   session reader (`FormSessions`, #1359) are composed, and those exist only in the
   `ELITEA_AUTH_CONFIG_FILE` branch. See [`auth.form.yml`](auth.form.yml) for
   why that means configuring a Form provider nobody signs in through.
6. **`ELITEA_CONFIGURATIONS_ENABLED=true`.** Agent dispatch composes through
   `dependencies.CurrentConfigurations`, which is nil unless configurations are
   enabled; it then fails a layer deep rather than at a guard. It also needs
   `ELITEA_AI_PROJECT_ID`.

Plus one ordering constraint that is easy to miss: composition verifies the
**shared migration head** at boot. A stack that applies its schema after
bringing services up will crash-loop elitea-main until it does. The standalone
compose therefore runs `db-init` and `elitea-migrate` as services rather than as
steps inside `seed`.

## File permissions are part of the contract

Everything above is read through `internal/security/securefile`, which requires
an absolute, canonical, non-symlink path and enforces exact permission bits:

| Material | Profile | Allowed mode |
|---|---|---|
| private keys, passwords, keyring signing key, vault key, form users | `PrivateMaterial` | owner-only (`0600`/`0400`) |
| CA certs, server certs, verification keyring, auth config | `PublicMaterial` | not executable, not group/other writable |

It also rejects two references that resolve to one file, or that carry equal
bytes — so each secret must be generated independently at random rather than
derived from one seed.

This is why compose does **not** bind-mount `deploy/certs/runtime` into the
services. Under rootless podman a bind-mounted host file arrives owned by
container uid 0, so a `0600` file is unreadable by elitea-main (distroless
`nonroot`, uid 65532) or the worker (uid 10001) — and "chmod 644 on the host" is not a
fix, because `PrivateMaterial` rejects any group/other bit.
[`install-material.sh`](install-material.sh) copies into per-consumer named
volumes instead, where owner and mode can be set independently, and gives each
consumer only the material it needs.

Kubernetes has the same problem, for the same reason, and the Helm chart
answers it the same way (issue #404). A Secret volume mounted whole is a
symlink farm; mounted per file with `subPath`, its files belong to root while
the pod runs as nonroot. So `runtime.material.secretName` in
[`../helm/elitea-main/values.yaml`](../helm/elitea-main/values.yaml) takes a
plain Kubernetes Secret, and an init container copies each key into a
memory-backed `emptyDir` at mode `0600`. That container is
`cmd/elitea-runtime-material`, and it runs the same image and the same user as
the service. It reads every file back through `securefile` before it exits, so
a missing key stops the pod there rather than in a restart loop. The Secret's
key names are the file names that `gen-runtime-certs.sh` writes.

## Streams and durable consumers are created by the control plane

The worker may not create a consumer (its NATS permissions deny it). A
consumer that can create its own durable can, after losing one, recreate it in
a way that skips every command in flight. The `nats-bootstrap` service runs
[`deploy/helm/elitea/files/nats-bootstrap.sh`](../helm/elitea/files/nats-bootstrap.sh),
the same script the cluster's hook Job runs, as the only identity that may.

## Workload sessions are provisioned, never self-registered

`WorkloadSessionsRepository` exposes no registration endpoint and no fallback
allowlist: a worker's calls are authorized against a row in
`elitea_runtime.workload_sessions` matching its certificate identity, session id
and producer id, all three checked in one query. No product code inserts that
row — the deployment control plane must, and each deployment has its own:

| Deployment | Provisioned by |
|---|---|
| compose / standalone | `standalone-stack.sh seed-runtime` |
| Helm / Kubernetes | the `runtimeSession` hook Job in `deploy/helm/elitea-worker-python` |

The Helm path reads the identity out of the worker's own
`agent-worker-client.crt` rather than taking it as a value, so the row cannot
name an identity the worker does not present. It also re-stamps `expires_at` on
every upgrade, which matters more than it looks: the column is `NOT NULL`, the
verifier requires `issued_at <= now() < expires_at AND revoked_at IS NULL`, and
nothing else ever writes this table — so a cluster that is provisioned once and
never upgraded goes dark on a timer.

**A missing row fails closed and silently.** Every `ClaimCommand` is refused,
elitea-main logs nothing on its control plane, and the worker durably
quarantines each refused command under `{spool_root}/quarantine.v1` — so the
affected turns are lost even after the row is added, and the worker needs a
restart. The tell is `execution_jobs` rows sitting at `DISPATCHED` /
`NOT_STARTED` with `execution_claims` empty.

## Verifying it

```bash
deploy/scripts/standalone-stack.sh certs
deploy/scripts/standalone-stack.sh up
deploy/scripts/standalone-stack.sh seed
deploy/scripts/standalone-stack.sh seed-runtime
deploy/scripts/standalone-stack.sh seed-llm
deploy/scripts/standalone-stack.sh check
```

`seed-llm` is in that list because `check` asserts the model hop as well as the
runtime plane. Read the last line of the output: it reports the passes, the
failures, the skips, and how many of the expected assertions reported a result.
A skipped assertion exits non-zero unless you pass `--allow-skips`, and a
shortfall against the expected count exits non-zero even with that flag (#422).

Note what `check` does **not** assert, and why. The intuitive probe — "POST
`…/messages/…` must not be 404" — does not discriminate: auth runs before route
matching, so every `/api/v2` path answers 401 to an unauthenticated caller
whether or not the route is registered. Measured on a runtime-disabled stack,
`GET`/`POST`/`PUT`/`DELETE` on both the messages and events paths all return 401
too. Authenticating does not rescue it either, because the runtime routes carry
no session-cookie fallback (`internal/api/production_runtime.go:31-37`).

The signals that do separate the cases are the ones `check` uses: the three mTLS
listeners (with the runtime off, nothing binds 9443/9444/9445 and the probe gets
ECONNREFUSED rather than a TLS handshake), the consumer group, the active
workload-session row, and elitea-main's `runtime_enabled=true` boot log.

## The worker (#282)

`elitea-worker` is the NodeEvent producer. Go admits and projects executions but
has no agent-token producer of its own, so without it an admitted run streams
nothing. Three things about it are not guessable from the compose file:

**It has no environment configuration.** Config is one JSON file —
`elitea-worker serve --config /run/elitea/runtime.json`, `extra="forbid"`. The
only environment variable the worker itself reads is `ELITEA_SENSITIVE_TOOLS`.
See [`worker-runtime.json`](worker-runtime.json); its `nats_stream` /
`nats_consumer` must match the compose env on elitea-main, and its
`workload_session_id` / `producer_id` must match the row `seed-runtime` writes.

**`platform_origin` is elitea-main's platform listener.** elitea-main serves a
fourth mTLS listener, `https://elitea-main:9446`
(`ELITEA_RUNTIME_PLATFORM_ADDRESS`, #1359), over TLS 1.3 with the content
listener's keypair, and like the other three it **requires a runtime-CA client
certificate**: the worker presents `agent-worker-client.crt`. It is a **gRPC**
listener (ALPN h2 only): each gRPC service on it admits only the workload
identities configured for it, `grpc.health.v1` answers any verified workload,
and it serves no HTTP route — probe it with `/elitea-grpc-health` from the
elitea-main image, not curl. It replaced `platform-edge`, the proxy that used
to terminate TLS for this origin. It is not the browser edge and publishes no
host port.

**Model calls are `elitea.llm.v1.ModelService` on that listener** (#1359 U2,
`libs/proto/elitea/llm/v1/model.proto`), not HTTP `/llm`. elitea-main admits
only the identities in `ELITEA_MODEL_SERVICE_CLIENTS` and refuses to start
with none while the listener is open: the worker's SPIFFE ID (its
certificate's one URI SAN, `RUNTIME_WORKER_SPIFFE_ID` in
`gen-runtime-certs.sh`; in Helm, `worker.runtimeSession.expectedIdentity`) and
`dns:elitea-subapp-callback`, the identity the DeepWiki and Inventory engines
present with `subapp-callback-client.crt` / `.key`. The engines dial
`ELITEA_<APP>_MODEL_SERVICE_URL=https://elitea-main:9446` with the runtime CA
(`ELITEA_<APP>_CALLBACK_CA_FILE`) and that certificate
(`ELITEA_<APP>_CALLBACK_CERT_FILE` / `_KEY_FILE`); compose installs the three
files into the `standalone_runtime_subapp` volume (`install-material.sh`), the
chart mounts `runtime-ca.crt` of `worker.materialSecretName` plus
`deepwiki.callbackClient` / `inventory.callbackClient`. `standalone-stack.sh
check` calls ModelService with grpcurl: the callback identity with an invalid
bearer must get `Unauthenticated` plus the `elitea-model-refusal-bin` trailer
(the pipeline's 401), and an unlisted runtime-CA identity `PermissionDenied`
without it.

**Artifacts and internal MCP tools are gRPC on that listener too** (#1359 U5,
`libs/proto/elitea/runtime/v1/artifact_object.proto` and
`internal_tool.proto`). Each RPC runs the same artifact route or MCP endpoint
in process, under the bearer in `authorization` metadata:

| Service | Callers | Allowlist |
|---|---|---|
| `elitea.runtime.v1.ArtifactObjectService` | the worker (Office toolkits, indexing artifact sources); the DeepWiki/Inventory hosts (page and graph uploads) and engines (artifact-folder ingest) | `ELITEA_ARTIFACT_SERVICE_CLIENTS` |
| `elitea.runtime.v1.InternalToolService` | the worker (its internal MCP toolkits: `ListTools`, `CallTool`); the Inventory engine (`InvokeToolkit`, its `investigate` source-tool calls, which replaced HTTP `test_tool`) | `ELITEA_INTERNAL_TOOL_SERVICE_CLIENTS` |

An empty allowlist leaves that service unregistered. The chart renders both
lists (worker identity, `dns:elitea-subapp-callback` when a provider needs it,
`main.runtime.<service>.extraClients`); compose names the worker and the
callback identity. The provider HOSTS (distroless `nonroot`, uid 65532) read
the callback material from their own volume, `standalone_runtime_subapp_host`,
because the engines' copy is owned by uid 10001 with a 0600 key. There is no
HTTP callback origin any more: `ELITEA_<APP>_CALLBACK_BASE_URL` and
`ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL` are refused at boot, and the chart
refuses `deepwiki.callbackViaPlatformListener` and
`main.runtime.agentExecutionDispatch.currentMainBaseUrl`. The worker addresses
its internal MCP catalogue entries by the fixed origin
`https://internal-tools.elitea.invalid`, which only names the entry; the call
itself is InternalToolService. `standalone-stack.sh check` probes both services
the same way as ModelService (invalid bearer → `Unauthenticated` plus
`elitea-artifact-refusal-bin` / `elitea-internal-tool-refusal-bin`).
(`content_origin` is different: that one is elitea-main's
own :9445 listener, reached over mTLS. It keeps the `https://host:port` form,
but since #1359 it is gRPC only — `elitea.runtime.v1.RuntimeContentService`,
with the execution claim and fence in call metadata — and, like :9446, it
serves no HTTP route and answers `grpc.health.v1` to any verified workload.)

**Its TLS trust is split.** The native worker hands the runtime CA to every
internal client (control, output, content, the model gateway) in code. The
process-wide trust store, `SSL_CERT_FILE`, governs only the outbound clients —
the remote MCP client and the toolkits — and must be a bundle that keeps the
public roots, because the variable replaces the platform store rather than
adding to it. On the standalone stack it is the `mcp-mock-trust` bundle
(public roots + mcp-mock CA + runtime CA), so a toolkit calling a public
service such as api.github.com verifies (#1203); the Helm chart builds the
equivalent `trust-bundle.crt` in an init container.

## Execution actors need a PAT

When the worker claims an execution it asks the content listener for a client
token (`RuntimeContentService.GetEliteaClientToken`), which resolves through `ActorTokenIssuer` → `LocalIssuer.IssueToken`.
That issuer "never creates, rotates, or stores a PAT" — it re-signs an
**existing active one** (`GetActivePATForUser`). With no row the claim fails at
stage `actor_pat_issuance`, before any model call, and nothing in the error says
a database row was missing. The E2E seeder creates users but no
`auth_core__token` rows, so `standalone-stack.sh seed-runtime` issues one per
user.

## The mock LLM (#283)

`seed-llm` defaults to an offline mock when no provider key is present, so a
model turn completes with no network and no billing. Three things are easy to
get wrong:

**The credential must be type `vllm`.** bifrost lifts its SSRF-safe dialer only
for the self-hosted provider classes — `GetConfigForProvider` sets
`AllowPrivateNetwork` for `schemas.VLLM` and `schemas.Ollama` alone. An
`open_ai` credential pointing at a compose address is refused by the dialer no
matter what the allowlist says.

**The allowlist must name the mock AND a private block.** `llm-mock:8090` is a
compose service name that resolves to a private address. The host entry permits
the NAME; a private block entry is what relaxes the dialer, because the gateway
never resolves a name to make that decision (gap G6 — resolving here and dialing
later is the DNS-rebinding race a name allowlist avoids). Without both, the hop
fails with `provider_connection_failed`, which reads like a broken service
rather than a policy decision.

The compose default therefore carries the two hosts and the three RFC 1918
blocks, and it is written with a single `-`
(`${GATEWAY_EGRESS_ALLOWLIST-…}`): the `:-` form treats empty and unset alike,
so an operator disarming the allowlist would silently get the mock allowlisted
back.

**The environment variable is only the floor.** The gateway also reads
`egress_allowlist` rows from `gateway.governance_config` and enforces the union,
so a destination can be added at runtime in Admin > LLM Proxy > Governance
without touching compose. `GET /governance/status` reports the merged list with
its source, and says whether a private destination is reachable at all.

**The wire model carries a provider prefix**, `vllm/E2E-MOCK-MODEL`. bifrost
resolves the provider from the model string alone (`ParseModelString`) with an
**empty** default, so a bare `E2E-MOCK-MODEL` reaches core with no provider and
never gets as far as the credential. The seeded `llm_model` row is titled with
the prefix so the picker hands the SDK a name that routes.

`api_base` is `http://llm-mock:8090` with no path — bifrost appends
`/v1/chat/completions` itself.

With `OPENAI_API_KEY` or `ANTHROPIC_API_KEY` set, `seed-llm` behaves exactly as
it did before: the real credential is seeded and the mock is not, so nothing can
quietly answer a request meant for a provider.

## Changing the admission cap (#966)

`ELITEA_RUNTIME_MAX_OUTSTANDING` (Helm `main.runtime.maxOutstanding`, default
64, at most 4096) is the number of durable executions one capability may have
outstanding. The first start of each capability persists the starting
replica's value in `elitea_runtime.execution_admission_policies`, and from then
on every start re-locks that row and **fails closed** while the replica's value
differs from the persisted one — two replicas must never admit under two
ceilings. A cap change is therefore two coordinated changes, and until both
are made every start answers 500 "Failed to start agent execution":

1. Give every elitea-main replica the new value: `helm upgrade … --set
   main.runtime.maxOutstanding=2048`, or the env var and a recreate on compose.
2. Once, from any elitea-main container, bring the persisted cap to the value
   that replica runs with:

   ```sh
   kubectl exec deploy/<release>-main -- /elitea-admission-ctl set
   # compose: podman compose exec elitea-main /elitea-admission-ctl set
   ```

   With no `-max` it writes that container's own
   `ELITEA_RUNTIME_MAX_OUTSTANDING` to every persisted capability, then prints
   them all. `-max N` and `-capability ID` narrow it; `show` only prints.

Starts fail closed between the rollout and step 2, so run step 2 right after
the rollout completes. Each replica logs `runtime admission cap` at startup
with `configured` and `persisted` per capability, as an ERROR with the fix
when they differ, and the mismatch error of a failed start names both values
and this command.

## What still does not work

The chat loop has not been driven end to end from a message to a streamed
reply. Verified: an authenticated dispatch reaches the agent-execution use case,
the worker joins the consumer group over TLS, and a completion through
elitea-main → gateway → mock returns the mock's echo (streaming and unary).
Joining those into one journey is #284.

Toolkit-bearing agents will fail here regardless. The SDK resolves toolkits
through `/api/v2/elitea_core/tools_list/{project_id}`, which the retired `deploy/centry-hybrid`
stack routed to **pylon**; Go's equivalent is a different path and answers 501 by
design. Nested application references and the artifact toolkit are pylon-backed
in a mixed deployment for the same reason. A plain adhoc turn touches none of them.

The web chat surface also still emits into a noop socket.io client rather than
subscribing to `{events_url}`; that port is #93.

Next-input suggestions are inert (#334). No server owns a suggestion policy,
so elitea-main wires no policy source and every turn carries
`next_input_suggestion: null` with no lookup at all. The policy is optional
metadata, and the turn never waits on it. `ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL`,
which once named the origin of that lookup and then of the worker's internal
MCP, is retired (#1359 U5) and refused at boot.

The product owner must settle #334. The Go stack can own the policy, or the team
can drop the feature. Nobody can read the original policy shape, because it is on
an unpublished branch.
