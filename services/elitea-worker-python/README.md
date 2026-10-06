# ELITEA Python runtime worker

## Current indexing runtime capability profile

The production worker image carries one explicit
`elitea.indexing-runtime-capability-profile.v1` profile. Its evidence source is
`elitea-sdk.lock.json`; the profile is also packaged into the wheel so the
`serve` entry point can verify the deployed artifact before it reads workload
credentials or opens NATS, gRPC, HTTPS or PostgreSQL connections.

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

## Command bus (NATS JetStream)

`docs/runtime-command-bus.md` at the repository root is the normative contract;
`src/elitea_worker/transport/nats_jetstream.py` is this worker's side of it and
the only module that imports the NATS client. The Redis Streams transport is
deleted: there is no transport flag and no mixed mode.

Each route has one WorkQueue stream (`ELITEA_RT_V1_VALIDATE`,
`ELITEA_RT_V1_AGENT`, `ELITEA_RT_V1_INDEX`) and one durable pull consumer
created by the NATS bootstrap Job. Every worker replica of a route binds to that
durable; a worker never creates, edits or deletes a stream, a consumer or the
dead-letter bucket, and at start it refuses to run when the stream or the durable
is absent or not the contract's shape (subjects, WorkQueue, explicit ack,
AckWait 60s, MaxDeliver -1, pull, the route's filter). A command is published on
`elitea.rt.v1.<route>.d.<sha256(delivery_id)>`; the body is exactly the signed
envelope, and after verifying the signature the worker requires the subject's
hash token to equal `sha256(command.idempotency_key)` before it claims.

| Situation | What the worker does |
| --- | --- |
| intake | reserves delivery permits first, then pulls at most that many, waiting at most `nats_fetch_expires_millis` |
| owned (queued or running) | `+WPI` every `nats_in_progress_interval_millis`; it resets AckWait |
| terminal PostgreSQL receipt (settled, obsolete, retired) | double ack (`AckSync`), only after the receipt; an already-acknowledged message is idempotent success |
| retry later, lease held elsewhere, recovery not possible now, retryable failure | `NakWithDelay(nats_retry_delay_millis)` |
| poison that can never verify: the signed envelope's digest or signature fails, or the subject does not name the signed command | one record in the `ELITEA_RT_V1_DEADLETTER` bucket FIRST, then `Term` (frees its stream capacity) and the ERROR line `worker_command.dead_lettered`. A PostgreSQL re-offer of the same delivery fails the same check before any claim and is terminated again |
| any other poison: a decode failure, a command it cannot serve, any non-retryable failure | one record in the `ELITEA_RT_V1_DEADLETTER` bucket FIRST, then `NakWithDelay(24h)` and an ERROR line `worker_command.dead_lettered`. Never `Term`: that frees the subject and PostgreSQL would re-offer the poison every 30s |
| poison whose record the bucket refused | not parked: `NakWithDelay(nats_retry_delay_millis)`, an ERROR line `worker_command.dead_letter_write_failed` carrying `dead_letter_write_failures_total`; the next delivery retries the record without running the command again |
| SIGTERM | stop pulling; nak WITHOUT delay every fetched message no worker task has started (another replica takes it now); keep heartbeating running work until it ends or the deadline passes, exit without acking or nak'ing it (AckWait redelivers) |

The dead-letter record (`schema: elitea.runtime.dead-letter.v1`) holds the
stream, consumer, subject, stream sequence, delivery count, a stable reason code,
the worker's client name and the time; never envelope bytes, the delivery ID or
any command field. The worker also keeps the dead-letter keys it refused in
`<spool_root>/quarantine.v2`, so the one redelivery after the 24h delay (before
the stream's MaxAge removes the message) is parked again without running.

The connection is mTLS as the `elitea-worker` identity (`nats_ca_path`,
`nats_certificate_path`, `nats_private_key_path`: TLS 1.3, the deployed CA only,
never host system roots) on the inbox prefix `_INBOX_elitea-worker`, the only
one the NATS permission table grants. `nats://` without client material is for
compose only; `tls://` requires all three paths, and user information in the URL
is refused. The TLS material is read from disk again before every reconnect, so
a rotated certificate is presented without a restart.

With an identity the worker is in its own NATS account, `WORKER`, not in the
`RUNTIME` account that holds the command streams: the server answers a
JetStream API request on the requester's reply subject without checking it
against the requester's permissions, so a pull or info request naming a command
subject as its reply would make the server store the answer in a command
stream. `RUNTIME` exports to `WORKER` only `CONSUMER.INFO` and
`CONSUMER.MSG.NEXT` of the three durables, imported under the JetStream API
prefix `JS.RUNTIME.API`, and their ack subjects. So the worker opens two
JetStream contexts: the durable through `JS.RUNTIME.API`, and the dead-letter
bucket — which lives in `WORKER` — through the default `$JS.API`. It reads no
stream information (elitea-main verifies the streams it writes); the bind
checks the durable's configuration only. Without an identity (compose's
plaintext posture, one global account) both use `$JS.API`.

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
  "limits_revision": "elitea.runtime.limits.conformance.v3",
  "workload_session_id": "session-issued-by-elitea-main",
  "producer_id": "python-worker-pod-1",
  "consumer_id": "python-worker-pod-1-consumer",
  "nats_url": "tls://nats.elitea.svc:4222",
  "nats_ca_path": "/run/secrets/nats/ca.crt",
  "nats_certificate_path": "/run/secrets/nats/tls.crt",
  "nats_private_key_path": "/run/secrets/nats/tls.key",
  "nats_stream": "ELITEA_RT_V1_INDEX",
  "nats_consumer": "elitea-index-worker-v1",
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
    "nats_fetch_batch": 8,
    "nats_fetch_expires_millis": 1000,
    "nats_in_progress_interval_millis": 5000,
    "nats_retry_delay_millis": 60000,
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

Transport message/payload size, complete gRPC request/response size, content-body size
and output-frame size are selected by `limits_revision` and are not repeated as
deployment values. In runtime v1 they are 64 KiB, 48 KiB, 64 KiB, 80 KiB,
256 KiB and 64 KiB, respectively. The response allowance is larger because a
claim response nests the bounded input manifest inside its receipt. Changing
one requires a new compatible protocol limits revision, not an
environment-specific JSON override.

Runtime v1 also fixes the initial liveness profile: the Go business-claim lease
is 30 seconds, the consumer's AckWait is 60 seconds, and the `+WPI` heartbeat
period is at most 15 seconds (a quarter of AckWait). A heartbeat on a message
the server has since redelivered elsewhere is ignored by the server, and the
claim, not the transport, decides who may execute.

The workload/spool private keys must be non-empty regular files with no group
or world permission bits. The spool key is exactly 32 raw bytes. The command verificationThe command verification
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

`serve` connects as described in "Command bus" above, binds the durable and the
dead-letter bucket, and only then starts pulling. An unreachable server is
retried; an absent or drifted stream, durable or bucket refuses to start.

The command bus has no worker result/output API. Generated gRPC clients use mTLS and exact
workload session/producer metadata. Content uses an mTLS `httpx.AsyncClient`
with `http2=True`, HTTP/1 disabled, CA verification, redirects disabled,
bounded connections/timeouts, and an explicit negotiated-HTTP/2 response check.

Every signed delivery derives a separate AES-256-GCM spool key and opaque
directory from its complete execution identity. A spool is never shared by the
whole stream. The existing delivery transaction retains authority across
claim, HTTPS input, bounded synchronous SDK execution, output ACK, durable
settlement, then and only then the double ack of that exact message.

SIGINT/SIGTERM arms one shutdown deadline shared by delivery drain and all
dependency closure. HTTP, gRPC, NATS and supervisor closure run concurrently;
they cannot each consume a fresh copy of the configured timeout. Expiry returns
the stable retryable dependency error and never creates a shutdown-path command
ack or nak, so unfinished messages are redelivered after AckWait. Intake, heartbeat, and delivery
tasks are supervised as one runtime: an unexpected sibling exit fails the
process instead of leaving an apparently live worker stalled on its stop event.
The phase-one synchronous SDK
bridge cannot preempt a Python thread already inside SDK code; the deployment
supervisor must enforce its process termination grace after the worker deadline.
The planned async SDK phase removes that cancellation limitation.

The repository tests include unit/component coverage and a worker CLI
subprocess retry/SIGTERM lifecycle test against an unreachable NATS server;
that test is not end to end. `tests/service/test_nats_commands_service.py` runs
the real transport as the `elitea-worker` identity against the NATS chart's own
rendered config, permission table and bootstrap script (`natstest-serve`; the
`nats-runtime-worker` job in `ci-python.yml`) and fails on any permission
violation. The opt-in harness under
`services/elitea-main/tests/system` separately starts PostgreSQL 16, the
command bus, the Go binaries and independent Python `serve`
processes. It proves the small configuration-validation topology, redelivery after
three authorization failures, settlement, retirement and SSE replay. It does
not prove production load/soak, process failover, restart-based certificate
rotation, the large-artifact path or the complete production-scale #5681
scenario; those remain separately reported release gates.
