# Edge identity propagation hardening

Branch `fix/edge-identity-propagation`, 2026-10-08. Tracks audit item SEC-01 and the related hardening items. This
document states what is enforced and which tests prove it. Finding details stay in the private audit record.

## Business behaviour

The current platform authenticates a browser or token at its edge and forwards the resulting identity to the
platform services as request headers. The new platform keeps that shape: Main's own forward-auth handler (EdgeAuth)
authenticates the caller's credential and projects `X-Auth-Type`, `X-Auth-ID` and `X-Auth-User-ID` onto the
upstream request.

Deliberately not ported:

- Trust in a source-address range. A peer inside `trusted_proxy_cidrs` is no longer sufficient. Main accepts a
  projection only with EdgeAuth's signature over the identity and the exact request.
- Forwarding of caller-supplied identity headers on any edge router.
- Unrestricted pod-to-pod reachability of Main in Kubernetes.

## Changes

| Layer | Change | Path |
|---|---|---|
| Main, signer | EdgeAuth adds `X-Auth-Signature: v1.<expiry>.<HMAC-SHA256>` over version, expiry, method, request URI, type, ID and owner ID. The key is HKDF-SHA256 of the Form document's attempt key with a dedicated label. Lifetime 30 s, clock skew 5 s. A public decision carries the placeholder `-`. A projection that cannot be signed is withdrawn before the error response. | `services/elitea-main/internal/api/browserauth/identity_projection.go:22-146`; `main_handler.go:237`; `core_handler.go:252,286-296` |
| Main, verifier | `VerifyForwardedIdentityPeer` requires a configured proxy peer AND a valid, unexpired signature over the request's own method and `RequestURI`. A missing or short key refuses construction. | `browserauth/forwarded.go:84,213-223`; `internal/authcomposition/graph.go:197-204` |
| Main, token scope | `PrincipalValidator` reloads a token's project binding and the bound project's state from the same tables the bearer path reads. A token validated from a projection therefore carries the same binding as the same token used as a bearer. | `internal/db/queries/auth_principal.sql:1-21`; `internal/infra/authsvc/principal_validator.go:84-89` |
| Main, outbound | Provider and LLM-gateway hops drop the whole `X-Auth-*` family, including the signature. | `internal/llmproxy/identity.go:290-297` |
| Edge configs | Every router to Main deletes caller `X-Auth-*` (signature included) and `X-Elitea-*` identity headers before any forwardAuth runs. Every forwardAuth copies `X-Auth-Signature`. The worker platform edge catch-all no longer routes `/internal`. `X-Elitea-Execution-Id` stays allowed on the platform edge (the runtime worker sets it on model calls, and Main re-signs it). | `deploy/runtime/platform-edge-dynamic.yml`; `deploy/helm/elitea/templates/worker/platform-edge.yaml`; `deploy/traefik/dynamic.yml`; `deploy/traefik/dynamic.e2e.yml`; `deploy/gateway-api/httproute.yaml`; `deploy/helm/elitea/values.yaml` (`stripIdentityHeaders`) |
| Helm guards | Refuse a plain Ingress with Form auth unless `main.ingress.identityHeadersStrippedByController=true`. Refuse a Gateway API strip list missing an identity name. Refuse a platform edge pointed at a Main Service the edge NetworkPolicy does not admit. | `deploy/helm/elitea/templates/guards.yaml` |
| NetworkPolicies | Default-deny, on by default. Main `:8080` admits the platform edge, direct-callback providers and the operator-named gateway peers. Main's runtime listeners admit the worker only. The platform edge admits worker, provider and Main pods, and may reach only Main and DNS. The worker admits no inbound traffic. The sandbox supervisor admits the worker and Main on its profile ports. Every install declares `networkPolicies.main.ingressFrom` or `noExternalIngress`. Disabling requires `externallyManaged`. | `deploy/helm/elitea/templates/main/networkpolicy.yaml`; `worker/networkpolicy.yaml`; `worker/platform-edge-networkpolicy.yaml`; `sandbox/supervisor-networkpolicy.yaml`; `_networkpolicies.tpl`; `guards.yaml:26-48` |
| Worker toolkits | The platform's identity header names (`X-Auth-Type`, `-ID`, `-User-ID`, `-Reference`, `-Signature`, `-Avatar`, `-Avatar-State`, `-Session-*`) and any `X-Elitea-*` name (case-insensitive) are refused at configuration and validation time. Third-party names such as `X-Auth-Token` or `X-Auth-Key` stay allowed. This covers OpenAPI (configured headers, custom auth header name, spec header parameters, per-call headers), MCP, Postman, Azure, GCP and Kubernetes. | `libs/rust/agent-runtime/src/toolkits/mod.rs` (`is_reserved_platform_header`) and its callers |
| Live-stack check | `standalone-stack.sh check` sends unsigned projections through the platform edge and directly to `elitea-main:8080` from inside the compose network. Each must be refused, and the same request with a real PAT must still answer as the PAT's own user. | `deploy/scripts/standalone-stack.sh` ("Edge identity projection, from inside the network") |
| Docs | Upgrade steps (NetworkPolicy decision, plain-Ingress acknowledgement, out-of-repo edges must copy `X-Auth-Signature`). | `docs/UPGRADING.md` |

## Tests

| Suite | Result |
|---|---|
| Main signer and verifier (`browserauth/identity_projection_test.go`) | Accept signed user and token. Refuse an unsigned projection from a trusted peer. Lifetime boundary at expiry and expiry + 1 s, and skew and skew + 1 s. Refuse every tampered field (ID, owner, type, method, route, query, removed or duplicated signature, placeholder, unknown version, extended expiry, truncated MAC, untrusted peer). Refuse another key and a missing or short key. Key not aliased. Unsignable values refused. |
| Main authentication matrix (`browserauth/edge_identity_matrix_test.go`) | Real EdgeAuth → edge header copy → real `Auth` middleware → real verifier. 12 rows: unauthenticated; session user; token; unsigned user; unsigned token; another user; another owner; another route; another method; peer outside ranges; unsigned projection beside the caller's own bearer (caller wins); no verifier composed. Also public projection, expiry, and the Auth Core RPC projection. |
| Mutation check | With signature verification disabled, 9 tests fail. With the binding reload removed, 4 fail (1 PostgreSQL). |
| Token binding (`authsvc/principal_validator_test.go`, `principal_binding_postgres_integration_test.go`) | Unit: bound-active, bound-suspended, unbound replaces a claimed binding. Real PostgreSQL with the real 0071 migration: projection-path binding equals bearer-path binding for all three. |
| Outbound strip (`llmproxy/strip_auth_headers_test.go`) | The whole `X-Auth-*` family is removed; `X-Authz-*` survives. |
| Main packages | After the rebase onto `main` (`0def77b2`), against a local pgvector PG18 throwaway database (`ELITEA_TEST_DATABASE_URL`, `ELITEA_AUTH_TEST_DATABASE_URL`): full `go test ./...` 193 packages with tests, all ok. The first run had 3 harness failures, fixed above; `internal/infra/db/repos` needs `-timeout 20m`, as it does on `main`. 22 packages have no tests. `-race` on browserauth, middleware, llmproxy, authcomposition and authsvc: ok. `go vet`: clean. `sqlc generate`: no drift. |
| Dependency scanners | No dependency change in this branch. `govulncheck ./...` (Main): standard library only, in the local `go1.26.5` toolchain, fixed in `go1.26.6`; pre-existing. `cargo deny --all-features check advisories` (Worker): RUSTSEC-2023-0071 (`rsa`, no fix); pre-existing and tracked. |
| Deploy-edge gates (`services/elitea-main/tests/deployedge`) | ok. New `edge_platform_identity_test.go`: strip first on every router to Main, floor coverage, signature copied by forwardAuth, `/internal` excluded. `requiredStrippedHeaders` includes `X-Auth-Signature`. |
| Helm render tests | 18/18 `deploy/helm/tests/render-*.sh` pass. New: `render-platform-edge-identity.sh` (66 assertions, rendered/runtime parity) and `render-network-policies.sh` (221 policy assertions across 8 renders; every selector and peer checked against the rendered pod templates; 10 guard checks). `render-compiled-snapshots.py`: 22 OK. `helm lint`: ok. CI's exact `helm template` commands for the staging, standalone and auth-minimal profiles and the toggles pass render. |
| Worker toolkits (Rust, `libs/rust/agent-runtime` after the crate move on `main`) | 4 new refusal tests (OpenAPI ×3, MCP) plus reserved names in the Kubernetes, Azure and GCP forbidden-header loops; shared predicate unit test. After the rebase (`--offline`): `agent-runtime` fmt and clippy (`-D warnings`) clean, `cargo test --lib --all-features` 484 passed, 0 failed; worker workspace clippy clean and lib 1732 passed, 0 failed, 71 ignored (pre-existing). Integration tests under `tests/` not run. |
| Test harnesses | The hand-written PostgreSQL schemas in `promptcontextreads`, `social` (feedback) and `runtimecomposition` (index RBAC) now include `centry.project.create_success` and `elitea_identity.token_project_binding`, which production always has (shared migration 0071), because the token principal reload joins them. On clean `main` and on this branch: 47/47 `social` top-level tests and 12/12 `promptcontextreads`, no skips. |

## Performance

**Budget.** Verification adds no I/O and at most 16 allocations per forwarded request. It runs before the principal
reload, which already costs a PostgreSQL round trip.

**Result.**
- `BenchmarkVerifyIdentityProjection`: 1.6–2.1 µs/op after warm-up (first run 6.2 µs/op), 704 B/op, 12 allocs/op
  (Apple M-series, 5 runs).
- `TestIdentityProjectionVerificationAllocationBudget` pins the 16-allocation budget.
- The binding reload adds two LEFT JOINs, by primary key and by an indexed column, to an existing query. The round
  trip count is unchanged.
- The edge adds one header to an existing forwardAuth response.

## Durability

**Threat.** None of these changes write state. Authentication is evaluated per request.

**Result.**
- No new crash window. Any Main replica verifies what any replica signed, because the key is derived from the same
  Form document.
- Rotating the attempt key invalidates projections at most 30 s old. Those requests fall through to the caller's own
  credential or answer 401, and the client retries.

## Resilience

**Bounds.**
- 30 s lifetime and 5 s skew (named constants).
- Signature header present exactly once.
- MAC exactly 32 bytes; `hmac.Equal` comparison.
- No field may contain CR, LF or NUL.

**Failure behaviour.**
- An empty key, a nil resolver, or an empty method or URI fails closed.
- A projection that does not verify is ignored. The request then authenticates with its own bearer or cookie if it
  has one; otherwise it gets 401.
- Unknown-outcome external effects: none.

## Security

| Category (`rules/security.md`) | Applies | How checked |
|---|---|---|
| Identity only from a verified credential | Yes | Signature required; source address alone refused (matrix rows, tamper tests) |
| Internal identity headers stripped at every edge | Yes | Deploy-edge gates, `render-platform-edge-identity.sh`, live-stack probes |
| Service-to-service authenticated; default-deny network | Yes | NetworkPolicies + `render-network-policies.sh`; compose limit below |
| Object-level authorization | Unchanged | Project gates unchanged; token binding restored on the projection path (PostgreSQL parity test) |
| Authorization fails closed | Yes | Verifier and guards fail closed (tests above) |
| Untrusted input bounds | Yes | Header shape and count, MAC length, expiry window |
| Injection / construction | No new SQL text; query regenerated with `sqlc` 1.31.1 (CI pin) | `sqlc generate` diff limited to the two generated files |
| Egress / SSRF | Partly | Toolkit identity header names refused; worker egress hardening is separate work |
| Secrets | Yes | Derived key, never logged; signature stripped on outbound hops; no secrets in the diff (scanned before each commit) |
| Supply chain | No dependency change | `crypto/hkdf` is in the Go standard library |

**Compose limit.** Compose networks filter by membership, not by port. The worker must share a network with Main for
the runtime listeners (`9443`–`9445`), so it can still open `elitea-main:8080`. That port no longer accepts an unsigned
projection, and the in-network probes in `standalone-stack.sh check` prove it. User code runs in network-less sandbox
containers, so no network split would remove reachability from untrusted code.

## Recovery guarantees

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Main × admission (authenticate request) | I | `browserauth/forwarded.go:213-223`; `middleware/auth.go` (unchanged fall-through to the caller's own credential) | Matrix row "unsigned projection beside the caller's own bearer"; expiry test |
| Main × admission after key rotation or replica switch | I | Shared derived key (`authcomposition/graph.go:197-204`) | `TestIdentityProjectionFromAnotherKeyIsRefused`; lifetime boundary tests |
| Web/browser edge × admission | I | Strip lists in `deploy/traefik/*.yml`, `deploy/gateway-api/httproute.yaml` | Deploy-edge gates; browser evidence below |
| Worker × tool call (toolkit headers) | F | `libs/rust/agent-runtime/src/toolkits/mod.rs` `is_reserved_platform_header` (typed `InvalidConfiguration` / `InvalidInput`) | Rust refusal tests |
| Worker × output delivery (execution events via the platform edge) | I | `deploy/runtime/platform-edge-dynamic.yml` (strip, then `runtime-auth` with signature copy) | `render-platform-edge-identity.sh`; live-stack check |

No row moves to L.

## Real-browser evidence

**Status: the final proof on a stack built from merged `main` is pending.** The evidence below comes from a
pre-rebase rehearsal (branch base `85cabcc8`). It counts for Main and the edge configuration. It does **not** count
as proof of the Rust worker binary:

- That base does not contain #1160 (the cargo-target cache fix).
- The worker image's binary was not extracted and checked for a branch-unique string (delivery gate §3b).

So the worker-toolkit leg and the post-tool observation below must be repeated on the new stack. The rebased branch
contains #1160.

### Pre-rebase rehearsal (2026-10-08)

**Stack.**
- Local compose rehearsal, project `elitea-sec-edge`, `deploy/docker-compose.standalone-full.yml` plus
  `docker-compose.standalone-rust-agent.yml` and a rehearsal overlay (unique image tags, optional subapps off).
- Images built from this branch at `61a752b4`: `elitea-main:sec-edge-61a752b40c5f` (target `e2e`),
  `elitea-worker-rust:sec-edge-61a752b40c5f`, `elitea-web:sec-edge-61a752b40c5f` and
  `elitea-llm-gateway:sec-edge-61a752b40c5f`.
- Edge configuration mounted from the branch.
- Brought up with `standalone-stack.sh up`, then `seed`, `seed-runtime`, `seed-llm` and `seed-index`.

**Live-stack check** (`standalone-stack.sh check --allow-skips`): 32 passed, 0 failed, 1 named skip (the SDK client
check does not apply to the native worker). The new in-network probes:
- An unsigned projection gets 401 at `https://elitea-platform-edge` and at `http://elitea-main:8080`.
- A real PAT still answers as its own user at both.

**Browser (built-in browser, no response mocks), user `e2e-chat@autotest.local` (id 7), project 90107 (Private):**

1. Normal sign-in through the stack's OIDC provider lands on the chat page.
2. The project selector shows the Private project, and the project's toolkits, artifacts and chats load.
3. Chat 4 has the `standalone-artifact-index` toolkit attached and the Rust worker runs its tools:
   - **Before the bucket existed:** `list_files` and `create_file` returned the typed tool result "no such file or
     bucket".
   - **After creating the bucket `elitea-artifacts` in the Artifacts page:** `create_file` wrote `edge-check.txt`
     (23 B), which the Artifacts page lists.
4. A same-origin GET to `/api/v2/social/author` from the page carried `X-Auth-Type: user`, `X-Auth-ID: 1`,
   `X-Auth-User-ID: 1` and a fake `X-Auth-Signature`:
   - without the cookie: 401;
   - with the session cookie: 200, `id` = 7 (the session user, not the named one).
5. After a reload, the session and all four chat turns persist.

**Observation (not caused by an authentication refusal):**
- After the artifact tool succeeded, the two later turns ended with "The runtime operation failed."
  (`native_agent.event_failed`, upstream `agent.legacy`).
- The worker logged `agent_tool_end` before the failure.
- Main logged no refusal or error, and the edge logged 200 for the model call.
- This was not reproduced against `origin/main` images, and the worker image predates #1160, so it is recorded
  here and not attributed.

**Fixtures.**
- Users, PATs, model rows and the artifact toolkit come from the stack's seed subcommands (SQL and product API).
- The bucket and the chat were created in the browser.

## Fixtures

- Unit and matrix tests build requests in process.
- PostgreSQL tests create an isolated database per run, apply the baseline projections and the real `0071`
  migration, then insert rows with SQL.

## Open items and follow-ups

- The v2 `/auth` handler (`internal/api/v2/auth/edge_auth.go`) still emits an unsigned projection. Main ignores it.
  Follow-up: remove the handler or sign through `signProjection`.
- A signature replays within its 30 s window on the same method and URI over the edge-to-Main hop (plain HTTP inside
  the cluster). Today only `GET` event streams carry one. Before a non-GET route is put behind forwardAuth, add a
  single-use nonce or a body digest.
- Python worker (`elitea-sdk`) toolkits have no equivalent header refusal. The Main signature check covers them.
- Toolkit configurations that already contain one of the reserved names now fail to materialize.
- Traefik rewrites `;` in query strings for the upstream request. Such a URI does not verify, and the request falls
  back to its own credential.
- The post-tool turn failure observed on the rehearsal stack (see browser evidence) needs a comparison run on
  `origin/main` images.
- Not tested against a real Kubernetes CNI. `probeFrom` exists for CNIs that apply NetworkPolicies to kubelet probes.
- CI workflows are unchanged. The bundled profiles declare `noExternalIngress: true`, so CI's existing render
  commands pass.
