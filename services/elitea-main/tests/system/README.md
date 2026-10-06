# Production runtime cross-process system test

## Existing Centry index reliability harness

`TestExistingComposeIndexReliability` is an opt-in, state-changing black-box
test for the already-running Centry hybrid runtime profile. It does not start a
second PostgreSQL or Redis and does not read `envs/override.env` itself. Docker
Compose resolves the existing service environment, while the test supplies
only paths, a non-secret start request, and a short-lived authenticated browser
cookie from owner-only local files.

The test fails closed unless there are zero active `index.ingest.v1`
executions, zero index-stream entries, zero delivery-index mappings, and zero
pending deliveries. It then runs two uniquely named indexes:

1. It stops the real `elitea-indexer-worker`, admits through the public HTTPS
   start route, and uses the worker's existing Redis ACL inside the Redis
   container to fetch and age one signed reference as
   `reliability-crashed-consumer`. This synthetic consumer is the only
   transport test double. Starting the real worker must reclaim that pending
   reference, create a PostgreSQL claim, settle durably, publish replay events,
   and retire the Redis reference.
2. With the worker stopped, it admits a second execution, opens public SSE,
   calls the public Stop route twice, and requires idempotent `204` responses
   plus durable `CANCELLED` state with no worker claim and a replay event. It
   then restarts the real worker, which must receive only the durable retired
   decision, invoke no SDK work, create no business claim, and atomically drain
   the Redis reference and delivery-index mapping.

Both requests inject a random, non-secret content-plane canary. The signed
Redis reference and public SSE data must not contain it. The test never prints
the cookie, Redis ACL passwords, signed envelope, input bundle, or SSE data.

Create a request file containing only caller-safe references and ordinary tool
parameters. Replace `123` with one visible toolkit Configuration ID; do not put
provider credentials or Configuration settings in this file:

```json
{
  "toolkit_config": {"toolkit_id": 123},
  "tool_name": "index_data",
  "tool_params": {
    "index_name": "replaced-by-the-harness",
    "repository": "owner/small-fixture-repository"
  },
  "llm_model": null,
  "llm_settings": {}
}
```

Store the raw `Cookie` request-header value from an authenticated local Form
session in a private file, for example `elitea_session=v1...`; do not include a
`Cookie:` prefix. Run from the `elitea-platform` repository root:

```bash
chmod 600 /absolute/private/path/index-cookie

ELITEA_INDEX_RELIABILITY_SYSTEM_TEST=1 \
ELITEA_CENTRY_DIR=/absolute/path/to/centry \
ELITEA_AUTH_POV_RUNTIME_DIR=/absolute/private/path/auth-pov-runtime \
ELITEA_INDEX_TEST_COOKIE_FILE=/absolute/private/path/index-cookie \
ELITEA_INDEX_TEST_REQUEST_FILE=/absolute/path/index-request.json \
ELITEA_INDEX_TEST_PROJECT_ID=1 \
go test -count=1 -v ./services/elitea-main/tests/system \
  -run '^TestExistingComposeIndexReliability$'
```

The default public origin is `https://localhost:18443`; override it with
`ELITEA_INDEX_TEST_BASE_URL` only when it is still an HTTPS origin.
`ELITEA_INDEX_TEST_TIMEOUT` may be set from `1m` through `15m` (default `5m`).
The runtime profile must already be bootstrapped and running with the checked-in
index route, stream, group, certificates, and ACL files. The authenticated
actor needs the route permissions. A current active PAT is additionally
required to cross the worker's runtime-context bridge into SDK/provider work;
without it the reclaim slice may validly prove a durable `FAILED` settlement.

Boundary classification:

| Boundary | Evidence in this harness |
| --- | --- |
| Public start / Stop / SSE over HTTPS | Real gateway, TLS, authentication, routing, Main handlers, and durable SSE replay |
| PostgreSQL | Real existing database, migrations, admission/outbox/claim/settlement/replay rows |
| Redis | Real existing TLS/ACL Redis, stream, consumer group, PEL, delivery-index hash, worker reclaim, and retirement |
| Worker control/content/output | Real restarted worker and its mTLS gRPC plus HTTPS content path; a PostgreSQL claim proves it crossed control admission |
| Crash window | Test double: one synthetic Redis consumer fetches and ages a real signed reference but never calls Main or executes SDK code |
| Progress safety | Deterministic non-secret canary injected into the private input and asserted absent from Redis references and public SSE |
| Provider, source system, and PgVector effects | External and deliberately unasserted |

`SUCCEEDED` and `FAILED` are both valid terminal results for the reclaim slice:
the test is proving cross-process delivery and settlement reliability, not a
provider contract. A failure such as an unavailable actor PAT or provider is
still visible in durable state without being misreported as Redis failure.
Claim provider or PgVector coverage only after supplying a deterministic
provider/source fixture through the existing SDK boundary and separately
checking its expected database effect. The current compose has no such
credential-free deterministic provider fixture, and the harness does not add a
production injection route or weaken the recorded PgVector TLS/DDL gaps.

The ordinary compile/unit check remains non-mutating:

```bash
go test ./services/elitea-main/tests/system \
  -run 'TestDecodeNonSecret|TestPrepareIndex'
```

## Self-contained configuration validation topology

`TestProductionRuntimeCrossProcessSystem` starts a real PostgreSQL 16
container and a nats-server from the NATS chart's own rendered permission
table (`libs/go/natsconn/natstest`, assets created by the real
`bootstrap.sh`), then independently starts the production `elitea-main`
binary and four `elitea-worker serve` processes. elitea-main publishes as
`elitea-main-runtime` in the RUNTIME account; every worker pulls as
`elitea-worker` from the WORKER account, through the `JS.RUNTIME.API`
service imports. It uses a private, typed application admission seam because
the public runtime routes remain deliberately unmounted until their
current-product RBAC and audit contract is ported. Public route/UI evidence
remains a separate deployment gate.

A worker with the wrong verification keyring cannot verify the command's
signature: it records one dead letter and terminates the message, and never
claims (the test then publishes the same bytes again, as PostgreSQL's
visibility repair would). Two more workers prove durable workload-identity
binding and mTLS trust-root enforcement: each leaves the command unclaimed
and unacknowledged, and the test naks it for immediate redelivery instead of
waiting out AckWait. The authorized worker then completes JetStream delivery,
mTLS gRPC claim, HTTP/2 content fetch, the configuration-validation business
handler, mTLS gRPC output/settlement, and the post-settlement double ack that
removes the command from the WorkQueue stream.

The harness adds one test-only, bounded fault proxy; production transport is
unchanged. The output proxy preserves the worker's mTLS identity and
workload-session metadata, receives Main's first positive committed ACK,
withholds it from the worker, and holds reconnects until the harness
restarts Main, PostgreSQL, and the worker.

Before an authorized claim, the same pending command is also preserved
through a NATS server restart (same store, `sync_interval: always`), a
PostgreSQL restart and a Main restart. After the committed-ACK fault, the
restarted worker takes the redelivered command and replays its encrypted
spool. A settled command published again (the counterpart of a lost ack) is
double-acked after the claim answers settled, with no second claim, output or
dead letter. The final assertions require one claim, one inbox record, one
business result, one settlement, one replay event with a valid monotonic
cursor, an empty command stream and worker spool. Thus retry cannot create a
second durable business output.

The test also asserts the bus's least privilege from the server's own
permission-violation log: the worker cannot publish a command, purge the
stream, delete a message or create a consumer; the producer cannot pull, ack
or delete a message; neither reaches another plane's subjects; and the run
ends with exactly those expected violations.

Run it explicitly; the ordinary unit/component suites do not start Docker:

```bash
ELITEA_RUNTIME_SYSTEM_TEST=1 \
ELITEA_SYSTEM_PYTHON=/absolute/path/to/python3.12 \
go test -count=1 -v ./services/elitea-main/tests/system \
  -run '^TestProductionRuntimeCrossProcessSystem$'
```

The selected Python environment must contain the worker dependencies, notably
the pinned SDK, `nats-py`, `h2`, `grpcio`, `httpx`, `pydantic`, and
`cryptography`. `ELITEA_SYSTEM_PYTHONPATH` may point at an additional local
dependency directory.

`ELITEA_SYSTEM_SDK_PATH` may point at the exact admitted Elitea SDK root. Use
it when a sibling `elitea-sdk` checkout exists but has advanced beyond the
worker's pinned revision; the worker intentionally fails closed on a package-
tree digest mismatch.

This is restart and ACK-loss evidence for the small credential-free
configuration-validation slice. It is not a public-route/UI, load, soak,
penetration, multi-node failover, certificate-rotation, or production-scale
issue #5681 test. The current input-content profile is intentionally capped at
256 KiB; large file/image streaming needs its artifact path before that
broader scenario can be claimed closed by a system test.

The configuration-validation handler has no provider/source/PgVector effect,
so this topology cannot honestly prove that an indexing SDK side effect is
invoked only once. The production-scale issue #5681 gate separately requires
its source/model fixture receipt to remain byte-for-byte unchanged across a
post-terminal worker restart. A crash after the SDK/PgVector side effect but
before Main durably commits terminal output is still an ambiguous at-least-once
window; it requires an SDK-owned idempotency key or a durable worker effect
receipt before the platform may claim exactly-once indexing effects.

## Index embedding binding Go-to-Python gate

`TestIndexEmbeddingBindingMainWorkerCrossProcess` is a non-deployment,
cross-process compatibility gate for the index capability-version transition.
The Go process uses the production authoritative-input resolver, embedding
binding resolver and Ed25519 index command producer. It proves that an exact
default `(model_name, model_project_id)` tuple owned by the shared public
project remains distinct from the current LiteLLM proxy's observed
project-first, public-fallback and raw-fallback routes.

A separate Python process uses the production worker delivery processor and
Ed25519 authenticator. It rejects a correctly signed stale
`index.ingest.v1` capability version `1` before the worker calls Main's claim
service, accepts version `2` through that pre-claim boundary, and validates the
claim-scoped binding with the worker's production schema mapper. The signed
control message carries only the immutable binding reference and digest; model
names, credential references, deployment details and endpoints remain absent.

Run it against the worker's exactly pinned SDK:

```bash
ELITEA_INDEX_BINDING_CROSS_PROCESS_TEST=1 \
ELITEA_SYSTEM_PYTHON=/absolute/path/to/python3.12 \
ELITEA_SYSTEM_SDK_PATH=/absolute/path/to/elitea-sdk-at-the-pinned-revision \
go test -count=1 -v ./services/elitea-main/tests/system \
  -run '^TestIndexEmbeddingBindingMainWorkerCrossProcess$'
```

This gate does not contact PostgreSQL, LiteLLM, Redis, PgVector or a provider.
Those service-backed boundaries retain their dedicated integration tests; this
test specifically proves the signed Main-to-worker authorization, version and
reference-only language boundary.

## Same-target synchronous SDK serialization gate

`TestPostgresPgvectorSameTargetSerializationAcrossInstalledSDKProcess` proves
that Stop does not release same-target availability after SDK invocation
authority. It holds the real pinned `EliteAClient.test_toolkit_tool` call in a
separate worker-container process, then checks Main admission exclusion in
`CLAIMED`, `RUNNING/PREPARING`, `RUNNING/MAY_HAVE_STARTED` after Stop, and the
durable post-output `SETTLING` recovery window. After canonical cancellation
settlement, it admits and initializes the next logical generation against real
PostgreSQL/PgVector, then proves old terminal and task-ID writes are fenced.

Run the opt-in gate against a PostgreSQL 16-18 server with `vector` and the
existing index-worker container. The Docker CLI must be installed, configured
to reach a live daemon, and able to inspect and enter that running container:

```bash
ELITEA_INDEX_SDK_SERIALIZATION_GATE=1 \
ELITEA_TEST_DATABASE_URL='postgresql://USER:PASSWORD@HOST:PORT/DATABASE' \
go test -count=1 -v ./services/elitea-main/internal/infra/db/repos \
  -run '^TestPostgresPgvectorSameTargetSerializationAcrossInstalledSDKProcess$'
```

Override the default `centry-elitea-indexer-worker-1` with
`ELITEA_INDEX_SDK_CONTAINER` when needed. Once
`ELITEA_INDEX_SDK_SERIALIZATION_GATE=1`, missing database configuration,
PostgreSQL/PgVector support, Docker CLI/daemon access, or a running configured
container fails the gate; only a disabled gate skips. The tool/provider
boundary is a deterministic blocker underneath the real installed SDK
callable. Command-bus delivery, the production worker serve loop, gRPC,
public authentication and an external source provider remain outside this
gate; the existing compose and cross-process harnesses own those boundaries.
