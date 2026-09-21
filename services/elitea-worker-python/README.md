# ELITEA Python runtime worker

## Current indexing runtime capability profile

The production worker image carries one explicit
`elitea.indexing-runtime-capability-profile.v1` profile. Its evidence source is
`elitea-sdk.lock.json`; the profile is also packaged into the wheel so the
`serve` entry point can verify the deployed artifact before it reads workload
credentials or opens Redis, gRPC, HTTPS or PostgreSQL connections.

The current Pylon Indexer baseline installs `elitea-sdk[all]==0.8.30` from
`centry/pylon_indexer/plugins/sdk_plugin/requirements.txt`. The standalone
worker separately admits `elitea-sdk==0.9.8` at
`b5113a129329b85d23c2d5c2bf55f18e307414ec` after a bounded source/dependency
delta review. The worker applies the two exact, upstream-merged MCP discovery
timeout/cleanup revisions recorded in `elitea-sdk.lock.json` without changing
the admitted 0.9.8 dependency graph. The current `0.8.30` behavior remains the comparison authority
until each covered family passes the target parity harness. The target image
maps the required indexing dependencies as follows:

| Current behavior | Standalone worker dependency evidence |
| --- | --- |
| Confluence API, current hosting/auth modes and page loader | `atlassian-python-api`, `elitea_sdk.tools.confluence` and its current SDK loader |
| Confluence image and PDF attachment analysis through the selected LLM | Pillow, `pdf2image`, Poppler, the SDK Confluence/image loaders, ReportLab/SVGLib and Cairo |
| Current OCR fallback when attachment LLM analysis is disabled | The admitted SDK 0.9.8 profile pins `langchain-community==0.4.1`; its corresponding methods import `pytesseract`. The image pins the OCR wrapper, its wheel digest and license, Tesseract, and the deterministic DejaVu probe font. Behavioral equivalence to the current 0.8.30 Confluence attachment path remains a parity-harness gate |
| Direct PDF loading, page extraction and image extraction | PyMuPDF, PyPDF, pypdfium2, the SDK PDF loader and Poppler |
| Project-specific PGVector writes and reads | `langchain-postgres`, `pgvector`, psycopg 3 binary/pool and the unchanged SDK vector adapter |
| Externalized chat and embedding calls | the current SDK client with the pinned OpenAI/Anthropic LangChain clients, against `elitea-llm-gateway` through elitea-main at `/llm/v1`; model credentials remain claim-scoped runtime data, not image content |
| SDK eager document-loader registry imports | the pinned DOCX, XLS/XLSX, PPTX, HTML, Markdown, NumPy/SciPy/Gensim dependencies required when the current loader map is imported |

This is intentionally smaller than the SDK `all` extra. It excludes unrelated
cloud/provider SDKs, browser automation, analytics/data-science toolkits,
Chroma, local embedding models, sentence-transformers, the Unstructured local
inference stack and development/test packages. Those omissions are capability
boundaries, not claims that their current behavior has been ported. A future
toolkit or MCP slice must introduce and test its own profile before the worker
advertises it.

The image build and every production `serve` startup verify:

- the exact SDK distribution, source archive and installed Python package-tree
  identity;
- the canonical hash of the exact indexing dependency profile;
- exact versions of every admitted direct indexing distribution;
- the SHA-256 and declared Apache-2.0 license of the pinned `pytesseract`
  wheel before installation;
- importability of the Confluence, image, PDF and PGVector execution paths;
- availability of `pdfinfo`, `pdftoppm`, `tesseract` and the Cairo shared
  library;
- one deterministic OCR call through `pytesseract` into the image-local
  Tesseract executable before any workload is accepted.

Failures return only the stable `DEPENDENCY_UNAVAILABLE` public diagnostic.
The chained local cause contains dependency identifiers and exception class
names only; it never includes credentials, URLs, configuration values or
filesystem paths.

## Redis command stream lifecycle

Runtime v1 permits one worker consumer group per Redis command stream. The
group may contain any bounded number of worker consumers or pods. A worker
reads or reclaims one of the bounded reference-only commands, completes the
business operation and durable settlement, then retires that exact stream
entry with one restricted Redis Lua operation. Before mutation, the operation
requires `<stream>:delivery-index.v1` to map the verified command
`idempotency_key` to that entry ID, requires `XRANGE` to return exactly the
same one-field `signed_envelope`, and requires the entry to be pending in the
configured group under that exact worker consumer. It then atomically applies
`XACK`, `XDEL`, and delivery-index `HDEL`. Exact `(1, 1, 1)` confirms the first
terminal mutation. A retry that finds the mapping, exact entry, and PEL record
all absent returns the distinct idempotent-success status `(2, 0, 0)`. Every
partial or conflicting state is a failure; absence is accepted only after the
caller has already validated durable settlement and requested terminal
retirement for that same verified delivery. The all-absent state is a safe
desired-state result under that durable authority; it is not proof that this
exact Lua call previously ran. Runtime v1 deliberately avoids an unbounded
Redis retirement-tombstone key.

Do not attach a second consumer group to the same command stream: post-settlement
deletion would remove entries before that group necessarily consumes them. A
second group requires a separate stream or a future multi-group retention
protocol.

The production #5681 failure mode is solved structurally by keeping settings,
files, images and outputs off Redis, enforcing a producer-side entry limit and
using a non-dropping stream-capacity gate. Phase-one deployment therefore uses
a dedicated, TLS/ACL-protected Redis control endpoint and treats a client-side
pre-decode RESP allocation cap as additional hardening against a compromised
or badly misconfigured control endpoint, not as the primary incident fix.

Atomic deletion bounds retention only for settled entries. It does not bound
the command backlog while workers or control dependencies are unavailable.
The producer-side capacity gate must retain work durably in PostgreSQL when the
stream is full. Do not use `MAXLEN` trimming for this purpose: trimming may
silently remove an unconsumed or pending command before durable settlement.

## Production serve composition

Run the standalone process with an absolute, regular JSON configuration file:

```console
elitea-worker serve --config /run/elitea/runtime.json
```

The configuration is validated with `extra="forbid"`. It contains identities,
targets, bounds and paths only; passwords and private keys are never embedded.
The current `elitea.runtime-deploy.v1` shape is:

```json
{
  "schema_version": "elitea.runtime-deploy.v1",
  "limits_revision": "elitea.runtime.limits.conformance.v2",
  "workload_session_id": "session-issued-by-elitea-main",
  "producer_id": "python-worker-pod-1",
  "consumer_id": "python-worker-pod-1-consumer",
  "redis_url": "rediss://elitea-worker@redis-control.internal:6379/0",
  "redis_password_path": "/run/secrets/redis-password",
  "redis_stream": "elitea.runtime.commands.v1",
  "redis_group": "elitea-python-workers",
  "control_target": "elitea-main-control.internal:8443",
  "output_target": "elitea-main-output.internal:8444",
  "content_origin": "https://elitea-main-content.internal:8445",
  "platform_origin": "https://elitea-main.internal:8443",
  "ca_path": "/run/secrets/runtime-ca.pem",
  "certificate_path": "/run/secrets/worker-chain.pem",
  "private_key_path": "/run/secrets/worker-key.pem",
  "ed25519_keyring_path": "/run/config/command-signing-keys.json",
  "spool_root": "/var/lib/elitea-worker/output-spool",
  "spool_key_path": "/run/secrets/output-spool-key",
  "limits": {
    "redis_read_batch": 8,
    "redis_block_millis": 1000,
    "redis_reclaim_idle_millis": 60000,
    "redis_reclaim_interval_millis": 5000,
    "dependency_retry_millis": 250,
    "delivery_max_concurrency": 4,
    "delivery_queue_capacity": 8,
    "sync_max_workers": 2,
    "sync_max_in_flight": 4,
    "admission_timeout_millis": 1000,
    "grpc_deadline_millis": 5000,
    "content_timeout_millis": 15000,
    "http_max_connections": 8,
    "http_max_keepalive_connections": 4,
    "output_max_queued_frames": 2,
    "output_max_queued_bytes": 131072,
    "output_max_sessions": 2,
    "output_ack_timeout_millis": 15000,
    "output_stream_deadline_millis": 300000,
    "lease_poll_interval_millis": 10000,
    "shutdown_timeout_millis": 30000
  }
}
```

Redis entry/field size, complete gRPC request/response size, content-body size
and output-frame size are selected by `limits_revision` and are not repeated as
deployment values. In runtime v1 they are 64 KiB, 48 KiB, 64 KiB, 80 KiB,
256 KiB and 64 KiB, respectively. The response allowance is larger because a
claim response nests the bounded input manifest inside its receipt. Changing
one requires a new compatible protocol limits revision, not an
environment-specific JSON override.

Runtime v1 also fixes the initial liveness profile: the Go business-claim lease
is 30 seconds, the Redis PEL heartbeat interval is at most 10 seconds, and an
entry cannot be reclaimed until it has been idle for at least 60 seconds. A
heartbeat atomically checks that the entry is still pending under the local
consumer before refreshing it; stale local state cannot take ownership back
from a peer that already reclaimed the delivery.

The Redis password and workload/spool private keys must be non-empty regular
files with no group or world permission bits. A Redis password file may contain
at most 514 raw bytes. The worker removes at most one terminal LF and its
immediately preceding CR, then requires 1..512 bytes of valid UTF-8 with no
remaining CR, LF, or NUL; it preserves those exact text bytes when authenticating
to Redis. The spool key is exactly 32 raw bytes. The command verification
keyring contains public keys and has this strict form:

```json
{
  "schema_version": "elitea.runtime-ed25519-keyring.v1",
  "keys": [
    {
      "key_id": "runtime-signing-2026-07",
      "public_key_base64": "base64-of-exact-32-byte-ed25519-public-key"
    }
  ]
}
```

`serve` opens a binary `redis.asyncio` TLS connection with the canonical ACL
username from the URL and the password from its protected file, and bounds both
new reads and `XAUTOCLAIM` pages.
The connection uses only the deployed private CA, never host system roots. The
stream and its one consumer group must be provisioned from `0-0` by the Go
control-plane/operator before workers start. Initial creation and recreation
must never start at `$`: an existing mapped delivery may predate the group.
Worker ACLs must not grant
`XGROUP`, `XADD`, `PUBLISH`, or arbitrary write authority. A missing group is a
retryable dependency failure, not a reason for a worker to create one. Redis
worker ACLs grant the restricted script only its stream and delivery-index keys
and the `HGET`, `XRANGE`, `XPENDING`, `XACK`, `XDEL`, and `HDEL` operations it
prevalidates or applies. Redis `EVAL` ACLs cannot grant one named script, so the
worker credential remains a bounded control-plane denial-of-service trust
boundary despite those key and command restrictions. It must never be shared
with provider code or the synchronous SDK bridge.

Phase one requires one logical Redis primary, whether deployed standalone,
behind Sentinel, or behind a service endpoint. Redis Cluster is not supported:
the two-key scripts intentionally address `<stream>` and
`<stream>:delivery-index.v1`, whose unchanged names do not guarantee one hash
slot. The endpoint must enable persistence and no-eviction behavior, and backup
or restore the stream and delivery-index hash atomically as one consistency
unit. The producer compares `XLEN` with `HLEN` before every append and fails
with `CONTROL_DELIVERY_INDEX_INCONSISTENT` without mutation when only one side
was lost. PostgreSQL visibility repair may rebuild a delivery after coordinated
loss of both keys, but it must not guess how to repair a partial Redis state.

The delivery-index protocol is not compatible with mixed old and new
producers or workers on one stream. An old worker can retire a stream entry
without removing its new mapping, while a new worker must refuse an unmapped
entry written by an old producer. Deployment therefore requires a coordinated
drain and cutover to a new versioned stream and consumer group; do not perform
an in-place rolling upgrade on the same stream.

Redis has no worker result/output API. Generated gRPC clients use mTLS and exact
workload session/producer metadata. Content uses an mTLS `httpx.AsyncClient`
with `http2=True`, HTTP/1 disabled, CA verification, redirects disabled,
bounded connections/timeouts, and an explicit negotiated-HTTP/2 response check.

Every signed delivery derives a separate AES-256-GCM spool key and opaque
directory from its complete execution identity. A spool is never shared by the
whole Redis stream. The existing delivery transaction retains authority across
claim, HTTPS input, bounded synchronous SDK execution, output ACK, durable
settlement, then and only then stable-ID-bound atomic `XACK` + `XDEL` + `HDEL`.

SIGINT/SIGTERM arms one shutdown deadline shared by delivery drain and all
dependency closure. HTTP, gRPC, Redis and supervisor closure run concurrently;
they cannot each consume a fresh copy of the configured timeout. Expiry returns
the stable retryable dependency error and never creates a shutdown-path command
ACK, so unfinished entries remain reclaimable. Intake, heartbeat, and delivery
tasks are supervised as one runtime: an unexpected sibling exit fails the
process instead of leaving an apparently live worker stalled on its stop event.
The phase-one synchronous SDK
bridge cannot preempt a Python thread already inside SDK code; the deployment
supervisor must enforce its process termination grace after the worker deadline.
The planned async SDK phase removes that cancellation limitation.

The repository tests include unit/component coverage and a worker CLI
subprocess retry/SIGTERM lifecycle test with a fake unavailable Redis module;
that test is not end to end. The opt-in harness under
`services/elitea-main/tests/system` separately starts PostgreSQL 16, the
current-baseline and dedicated TLS/ACL Redis 7 instances, the Go binaries and
independent Python `serve`
processes. It proves the small configuration-validation topology, reclaim after
three authorization failures, settlement, retirement and SSE replay. It does
not prove production load/soak, process failover, restart-based certificate
rotation, the large-artifact path or the complete production-scale #5681
scenario; those remain separately reported release gates.
