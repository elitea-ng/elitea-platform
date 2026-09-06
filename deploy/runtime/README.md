# The runtime plane — what `ELITEA_RUNTIME_ENABLED=true` actually costs

This directory holds the non-secret half of the runtime plane for the full
standalone stack: the TLS Redis config, the production auth config, and the two
scripts that install generated material and pre-create the dispatch streams. The
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

1. **TLS Redis, not the plaintext one.** Agent dispatch is Redis Streams —
   `internal/transport/redisdispatch/agent.go` does the `XADD`; there is no NATS
   in this path. `ELITEA_RUNTIME_REDIS_URL` must be `rediss://` with an ACL
   username, no password in the URL (it comes from `_REDIS_PASSWORD_FILE`), and
   an explicit `/0` database. The client pins `MinVersion: TLS 1.3`.
2. **Three mTLS listeners, not one.** Control gRPC `:9443`, output gRPC `:9444`,
   content HTTPS `:9445`, each with its own `_TLS_CERT_FILE`, `_TLS_KEY_FILE`
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
   `ELITEA_RUNTIME_ENABLED=true` unless a `PrincipalValidator` and a
   `ForwardedIdentityVerifier` are composed, and those exist only in the
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
`nonroot`, uid 65532) or redis (uid 999) — and "chmod 644 on the host" is not a
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

## Consumer groups are created by the control plane

The worker's Redis ACL user deliberately has no `XGROUP`. A consumer that can
create its own group can, after losing one, recreate it at the stream head and
silently skip every command in flight. [`bootstrap-streams.sh`](bootstrap-streams.sh)
creates them from `0-0` as a bootstrap user that can do nothing else.

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
See [`worker-runtime.json`](worker-runtime.json); its `redis_stream` /
`redis_group` must match the compose env on elitea-main, and its
`workload_session_id` / `producer_id` must match the row `seed-runtime` writes.

**`platform_origin` needs its own TLS front.** The worker rejects a non-https
origin outright (`config.py:187`), and elitea-main has no TLS listener for its
ordinary `/api/v2` + `/llm` surface — its three mTLS listeners speak the private
control/output/content protocols, not the product API. Hence `platform-edge`, a
second Traefik that terminates TLS with the runtime CA's `platform-edge`
certificate and forwards to `elitea-main:8080`. It is not the browser edge and
publishes no host port. (`content_origin` is different: that one is elitea-main's
own :9445 listener, reached over mTLS, and the worker requires **HTTP/2** on it
specifically — `serve.py:721` sets `http1=False`.)

**Its TLS trust needs three environment variables, or nothing works.** The SDK
routes verification through the OS trust store on import
(`elitea_sdk/_system_ca.py` injects `truststore`), which cannot contain a CA
minted on this machine. `ELITEA_DISABLE_SYSTEM_CA=1` restores the standard
mechanisms, and then `REQUESTS_CA_BUNDLE` covers the `requests` calls to
`/api/v2` while `SSL_CERT_FILE` covers the httpx/openai model call. A
runtime-CA-only bundle is both correct and tighter than adding public roots:
every host the worker dials is signed by that CA, and model traffic egresses
through elitea-main's `/llm` proxy rather than directly to a provider.

## Execution actors need a PAT

When the worker claims an execution it asks the content listener for a client
token, which resolves through `ActorTokenIssuer` → `LocalIssuer.IssueToken`.
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

## What still does not work

The chat loop has not been driven end to end from a message to a streamed
reply. Verified: an authenticated dispatch reaches the agent-execution use case,
the worker joins the consumer group over TLS, and a completion through
elitea-main → gateway → mock returns the mock's echo (streaming and unary).
Joining those into one journey is #284.

Toolkit-bearing agents will fail here regardless. The SDK resolves toolkits
through `/api/v2/elitea_core/tools_list/{project_id}`, which `deploy/centry-hybrid`
routes to **pylon**; Go's equivalent is a different path and answers 501 by
design. Nested application references and the artifact toolkit are pylon-backed
in the hybrid for the same reason. A plain adhoc turn touches none of them.

The web chat surface also still emits into a noop socket.io client rather than
subscribing to `{events_url}`; that port is #93.

Next-input suggestions are inert. This is a deliberate gap (#334). Each turn asks
the current platform for the suggestion policy at
`GET /api/v2/elitea_core/next_input_suggestion_config/prompt_lib/{projectID}`,
over `ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL`. **No readable repository implements
that path.** This monorepo does not. The pylon runtime and the plugin
repositories do not.

Both stacks make the call fail. The hybrid edge sends the path to legacy Centry,
which registers no such rule. The standalone stack aims an `https` origin at a
cleartext port. The turn is not affected. The policy is optional metadata, and
the caller fails open by design. Each turn therefore carries
`next_input_suggestion: null`.

The cost is one bounded request of 3 s for each send, regeneration, continuation
and ad-hoc turn. The failure is now visible. The client writes the cause to the
log once for each process, at warning level. It names the endpoint and the
reason.

The product owner must settle #334. The Go stack can own the policy, or the team
can drop the feature. Nobody can read the original policy shape, because it is on
an unpublished branch.
