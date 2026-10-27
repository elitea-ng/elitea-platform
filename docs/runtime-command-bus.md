# Runtime command bus: the NATS JetStream contract

Status: normative for this repository. The architecture decision is the
ADR-0010 amendment "Transport binding: NATS JetStream" in `elitea-docs`; this
file is the wire and deployment contract the three parties implement:

| Party | Code |
| --- | --- |
| Producer (elitea-main) | `services/elitea-main/internal/transport/commandbus` |
| Rust worker | `services/elitea-worker-rust/src/transport/nats_jetstream.rs` |
| Python worker | `services/elitea-worker-python/src/elitea_worker/transport/nats_jetstream.py` |
| Streams, consumers, dead-letter bucket | `deploy/helm/elitea/files/nats-bootstrap.sh` (the only owner) |
| Who may do what | `deploy/helm/elitea/values.yaml` (the permission table) |

The Redis Streams transport is deleted. There is no transport flag and no
mixed mode: a deployment runs this bus or no runtime plane.

## What the bus is for

PostgreSQL is the authority. The outbox owns dispatch intent; the claim
(`ClaimCommand`) owns execution; results and settlement travel over gRPC.
The bus provides six properties and nothing else:

1. at-least-once delivery to one shared consumer per route;
2. a bounded live set with a synchronous "full" signal (backpressure leaves
   the row in the outbox);
3. de-duplication of PostgreSQL re-offers of a delivery that is still live;
4. redelivery of work whose consumer died;
5. a heartbeat that keeps long work owned;
6. a readable "drained" state.

## Streams

One WorkQueue stream per route. Two dispatch routes may share a stream (the
standalone profile runs index ingest on the agent stream); sharing means the
same stream AND the same durable consumer.

| Stream | Subjects | Durable consumer | MaxAge (default) |
| --- | --- | --- | --- |
| `ELITEA_RT_V1_VALIDATE` | `elitea.rt.v1.validate.d.*` | `elitea-configuration-worker-v1` | 3h |
| `ELITEA_RT_V1_AGENT` | `elitea.rt.v1.agent.d.*` | `elitea-agent-worker-v1` | 26h |
| `ELITEA_RT_V1_INDEX` | `elitea.rt.v1.index.d.*` | `elitea-index-worker-v1` | 26h |

A stream name is `ELITEA_RT_V1_<ROUTE>`; the route token in its subjects is
`<route>` lower-case. Every stream has the same shape:

| Setting | Value | Why |
| --- | --- | --- |
| Retention | WorkQueue | an ack removes the message (capacity is released on settlement, like the old XACK+XDEL+HDEL); the server refuses a second consumer with an overlapping filter |
| Storage | File | |
| Replicas | 1 (scale-1) / 3 (HA) | the bootstrap's `replicas` |
| MaxMsgs | the route's capacity, at most 1024 | Discard=New makes a full stream refuse the publish |
| MaxBytes | 64 MiB | the old encoded-byte budget |
| MaxMsgSize | 65536 | `max_transport_message_bytes`: payload plus headers |
| Discard | New | never evict a live command |
| MaxMsgsPerSubject | 1 | one live message per delivery subject ... |
| DiscardNewPerSubject | true | ... and a second live copy is refused atomically |
| MaxAge | the route's longest deadline + 2h | a safety net: it removes only commands PostgreSQL has already expired |
| Duplicates | 2m | `Nats-Msg-Id` window, for an ambiguous publish retry |
| AllowDirect | true | the producer's conflict check reads the live copy with a direct get |
| DenyDelete / DenyPurge | true | only an ack removes a command; no client can delete one |

Durability (owner decision Q2): every write is synced. The NATS server runs
with `jetstream { sync_interval: always }` (deploy/helm/elitea), so a publish
or an ack is on disk before the client sees its reply.

## Subjects and headers

```
elitea.rt.v1.<route>.d.<sha256(delivery_id)>
```

* `delivery_id` is the outbox ID, which is also the signed command's
  `idempotency_key`. The subject carries `hex(sha256(delivery_id))`, 64
  lower-case hex characters, because a delivery ID may contain any byte but
  CR, LF and NUL, including `.`, `*`, `>` and spaces. Vector:
  `sha256("outbox-1") = ff7cc06fb9d124826b7f491676dc63e28a1572194bbe1dda72437bfe84b42164`.
* The body is exactly the `SignedWorkerCommandEnvelopeV1` bytes. The Ed25519
  signature input is unchanged: `"elitea.runtime.worker-command.ed25519.v1\0"
  || u64be(len) || command_bytes`. It binds no transport coordinate.
* Headers (unsigned, never authority):
  * `Nats-Msg-Id`: the subject's hash token (JetStream de-duplication);
  * `Elitea-Delivery-Id`: the raw delivery ID, for diagnostics;
  * `Nats-Expected-Stream`: the stream name (the producer's
    `WithExpectStream`), so a publish that a misconfigured server would route
    elsewhere fails instead.
* A worker compares the subject's hash token with
  `sha256(command.idempotency_key)` after verifying the signature and before
  it claims. A mismatch is poison (below).

## Producer (elitea-main)

| Publish result | Action |
| --- | --- |
| PubAck | success; the entry ID is `<stream>:<sequence>` |
| PubAck with Duplicate | success (an ambiguous retry inside the 2m window) |
| per-subject limit (`maximum messages per subject exceeded`, JetStream error 10077 family) | direct-get the subject's last message; equal bytes is success (the PostgreSQL re-offer of a live delivery), different bytes is `CONTROL_DELIVERY_CONFLICT` |
| stream full (`maximum messages exceeded` / `maximum bytes exceeded`) | `CONTROL_STREAM_SATURATED`, which is `ErrDispatchBackpressured`: the row stays in the outbox |
| timeout, no responders, disconnect | a transport error; PostgreSQL retries, and a retry of a publish that did land meets the per-subject limit and compares equal |

The exact JetStream error codes are pinned by an integration test against a
real server, not string-matched in production code alone.

At boot the producer reads each configured stream's info and refuses to start
unless it is the shape above (subjects, WorkQueue, Discard=New,
DiscardNewPerSubject, MaxMsgsPerSubject=1, MaxMsgSize at most
`max_transport_message_bytes`, MaxMsgs in 1..1024, MaxAge zero or at least
the route's deadline plus margin). The bootstrap owns the values; main only
verifies them.

"Drained" (the index v2 cutover preflight, `cmd/index-v2-preflight`):
`StreamInfo.State.Msgs`, `ConsumerInfo.NumAckPending + NumRedelivered +
NumPending`, and `StreamInfo.State.NumSubjects` must all be zero.

The execution-replay wake-up is core NATS on `elitea.rt.v1.replay.wake`
(advisory; PostgreSQL polling is the fallback).

The governed-credential invalidation fan-out (#1121) is core NATS on
`elitea.rt.v1.security.invalidation`: after an agent binding, mandate,
delegation, credential grant or external identity transition commits, Main
publishes the record's kind, id, new epoch and change time (no tenant
secret, no value), and every replica drops the credential cache partitions
filled at an older epoch. Advisory: every cache hit and redemption re-checks
the epochs in PostgreSQL, so a lost message costs latency only. Workers are
denied the subject (`elitea.rt.v1.>`).

## Consumers and workers

One durable pull consumer per stream, created by the bootstrap. Workers bind
to it (`js.Consumer`, async-nats `get_consumer`, nats-py
`pull_subscribe_bind`) and refuse to start if it is absent or misconfigured.

| Setting | Value |
| --- | --- |
| FilterSubject | `elitea.rt.v1.<route>.d.*` |
| DeliverPolicy | All |
| AckPolicy | Explicit |
| AckWait | 60s (twice the 30s claim lease) |
| MaxDeliver | -1 (PostgreSQL bounds retries, not the broker) |
| BackOff | unset (the worker chooses the delay per disposition) |
| MaxAckPending | 1024 |
| MaxWaiting | 512 |
| MaxRequestBatch | 64 |
| MaxRequestExpires | 30s |

Worker behaviour:

| Situation | Action |
| --- | --- |
| intake | pull only as many messages as there are worker tasks free to start one now, plus at most a small fixed prefetch (semaphore before pull), so the queue behind running work stays short and a second replica can take what one replica cannot start. A pull waits up to its expiry for the FIRST message and returns as soon as it has one (it never holds a delivered command until the expiry) |
| owned and working | `+WPI` (in progress) every 5s for every owned message, queued or active; it resets AckWait |
| settled (terminal receipt from main), `SETTLED`, `OBSOLETE`, `RETIRED` | double ack (`AckSync`), after the settlement receipt only; "already acknowledged" is idempotent success |
| `RETRY_LATER`, active lease held elsewhere, recovery not possible now | `NakWithDelay(60s)` |
| poison that can never become valid: the envelope signature fails, or the subject's hash token is not `sha256(idempotency_key)` | dead-letter record, then `Term` (`+TERM`): it frees the stream's capacity at once. PostgreSQL may re-offer the same outbox row; it is checked, recorded and terminated again |
| other poison: decode failure, a command this worker cannot serve | dead-letter record, then `NakWithDelay(24h)` + an ERROR log `worker_command.dead_lettered` + the dead-letter counter |
| the dead-letter record cannot be written | no `Term` and no 24h park without a record: `NakWithDelay(retry)`, an ERROR log `worker_command.dead_letter_write_failed` and its counter; the next delivery retries the record |
| any delayed nak | the delivery leaves the heartbeat set FIRST, and no heartbeat round still in flight may cross it: a `+WPI` after a delayed `-NAK` resets the redelivery timer to AckWait (nats-server `progressUpdate`), which would turn a 24h park into a 60s loop |
| graceful shutdown | stop fetching; deliveries fetched but not started are given back with a zero-delay nak (another replica takes them now); running work keeps heartbeating until it ends or the shutdown deadline passes, then stops without ack or nak, so AckWait redelivers |

A redelivery of a message whose earlier delivery is still running elsewhere
reaches the claim, which answers active-lease; the worker naks it with the
retry delay. An ack from an earlier delivery is accepted by the server; that
is safe because an ack only ever follows a terminal PostgreSQL receipt.

### Dead letter (owner decision Q3)

KV bucket `ELITEA_RT_V1_DEADLETTER`, in the workers' own WORKER account,
created by that account's bootstrap, history 1, TTL
7 days. Key `<route>.<sha256(delivery_id)>`. Value (JSON,
`schema: "elitea.runtime.dead-letter.v1"`): `stream`, `consumer`, `subject`,
`stream_sequence`, `num_delivered`, `reason` (a stable low-cardinality code),
`worker` (the worker's client name), `recorded_at_unix_millis`. No envelope
bytes, no delivery ID, no command field: the record says where to look, not
what the command said. The alert is new records being written —
`EliteaRuntimeCommandDeadLettered` in deploy/helm/elitea/templates/nats/prometheusrule.yaml,
`max(increase(nats_stream_last_seq{stream_name="KV_ELITEA_RT_V1_DEADLETTER"}[1h])) > 0` —
plus the workers' ERROR log line `worker_command.dead_lettered` and their
in-process counter. Any write moves the bucket's last sequence, so the alert
fires for records written within the last hour and clears on its own (an
operator's `nats kv del` writes a delete marker, which echoes it for at most
that hour). `nats kv ls` is the current list. `EliteaRuntimeCommandStreamNearlyFull` fires before a
stream starts backpressuring dispatch.

## Governed commands (ADR-0039, #1115)

A governed admission carries an execution security context: Main stores the
deterministic `ExecutionSecurityContextV1` bytes beside the execution and
names them on the signed command as `WorkerCommandV1.security_context_ref`
(field 65: content id `elitea.security-context/<execution_id>`, the SHA-256
of the bytes, the schema revision). A legacy command never carries field 65,
so its bytes are unchanged.

A governed command rides the same stream on its own subject lane, pulled by
its own durable:

```
elitea.rt.v1.<route>.g.<sha256(delivery_id)>
```

| Stream | Governed subjects | Governed durable |
| --- | --- | --- |
| `ELITEA_RT_V1_VALIDATE` | `elitea.rt.v1.validate.g.*` | `elitea-configuration-worker-governed-v1` |
| `ELITEA_RT_V1_AGENT` | `elitea.rt.v1.agent.g.*` | `elitea-agent-worker-governed-v1` |
| `ELITEA_RT_V1_INDEX` | `elitea.rt.v1.index.g.*` | `elitea-index-worker-governed-v1` |

The governed durable has the legacy durable's settings (table above) with the
`.g.*` filter; the bootstrap creates it and the `elitea-worker` identity gets
the same INFO / MSG.NEXT / ACK grants on it. The legacy durables (`.d.*`)
never see a governed command, so a worker that did not opt in can never pull
one.

A worker opts in with `ELITEA_WORKER_GOVERNED_CONTEXT=true` (the Rust worker;
default off, which is the legacy worker byte for byte). An opted-in worker:

* binds the stream's governed durable beside the legacy one, on the same
  connection, and runs a second intake over it. Both are verified against the
  contract at boot; an absent governed durable refuses startup like an absent
  legacy one;
* sets `ClaimCommandRequestV1.governed_security_context_v1` on every fresh
  claim;
* after the claim and `BeginExecution`, before reading any input, fetches the
  context with `RuntimeContentService.GetInput` (content id
  `elitea.security-context/<execution_id>`, immutable version
  `sha256:<hex digest>`) under the live claim, and verifies it: the content
  id is this execution's, at most 16 KiB, SHA-256 equal to the signed
  reference, a strict decode refusing an unknown field anywhere, schema
  revision `elitea.runtime.security-context.v1`, profile
  `elitea.credential-profile.governed.v1`. A failed check fails the execution
  closed (`AUTHORIZATION_FAILED`, never retryable).

Before it claims, a worker checks that the lane and the command agree: a
command with field 65 on a worker that did not opt in is an unsupported
command (dead-letter, 24h park); a field-65 command on a `.d.` subject, or a
command without it on a `.g.` subject, is a subject mismatch (dead-letter,
`Term`).

Main enforces the same pairing at claim:

| Claim | Answer |
| --- | --- |
| a field-65 command without `governed_security_context_v1` | `GOVERNED_WORKER_REQUIRED`, non-retryable (the worker maps it to an unsupported capability) |
| a command without field 65 whose execution is pinned governed | `GOVERNED_CONTEXT_MISSING`, non-retryable (an authorization failure) |

### Governed toolkit credentials (#1118, #1120)

On a governed agent turn no toolkit uses a credential from its claim-time
settings. Each credentialed configured toolkit is served through its
`AuthAdapter` (`libs/rust/agent-runtime/src/toolkits/governed`), and every
tool call:

1. is mapped to one canonical operation of
   `testdata/proto/runtime/v1/governed-operations/catalogue.json`
   (`operation_id`, `resource_ref` `<provider>:<kind>/<id or *>`, SHA-256 of
   the canonical JSON arguments). A tool the adapter does not map is refused
   (`governed.operation_not_mapped`) and nothing is redeemed;
2. is redeemed with `RuntimeContentService.RedeemCredential` under the live
   claim (claim id and fence in metadata), naming a fresh UUID v7
   `invocation_id` and the command's security context digest;
3. refuses, before any provider I/O, an envelope whose `target_identity` is
   not the toolkit's canonical configured target, an expired one
   (`not_after`), and one whose class or material keys the adapter does not
   allow;
4. builds a fresh provider client from the envelope alone, runs the call and
   drops both. There is no client cache: two runs, or two calls of one run,
   never share a client. An envelope is never logged or persisted.

The canonical target is the `https` URL `scheme://host[:port][/path]`
(host lowercased, no trailing slash, query or fragment): the configured base
URL for Jira, Confluence, Aha! (origin only) and qTest, the site URL with its
site path for SharePoint.

A `CredentialRedeemDenialV1` with `AUTHORIZATION_REQUIRED` raises the
delegated-authorization signal (`mcp.authorization_required`) with the
toolkit, its target as `server_url` and the opaque `authorization_reference`;
any other code is a tool error carrying the stable code only
(`governed.credential_denied`). A transport or binding failure is
`governed.redeem_unavailable`.

Refused as `toolkit_not_governable` on a governed run: a configured toolkit
whose type has no adapter (named in the run notice; a pipeline fails), every
MCP connection, a nested agent or saved pipeline that carries a credentialed
toolkit, and every governed toolkit command (`toolkit.*`, validation, index
ingest) on the toolkit lane. Credential-free toolkits (`artifact`, `memory`,
`pptx`, `docx`, `xlsx`) are served as before. `GetEliteaClientToken` stays
the model-gateway credential only and never reaches a toolkit. A legacy
command installs none of this.

## Limits (owner decision Q6)

`libs/proto/elitea/runtime/v1/limits.proto` fields 4 and 5 are
`max_transport_payload_bytes` (the body, the signed envelope) and
`max_transport_message_bytes` (body plus headers, the stream's MaxMsgSize).
They were `max_redis_field_bytes` and `max_redis_entry_bytes`; the field
numbers are unchanged, and the limits revision moved to
`elitea.runtime.limits.conformance.v3` so a v2 producer and a v3 worker never
agree by accident.

## Identities and permissions (the RUNTIME and WORKER accounts)

The NATS server has one account per plane (`deploy/helm/elitea/values.yaml`:
MAIN, GATEWAY, SCHEDULER, RUNTIME, WORKER). The three streams, their
durables and the replay wake-up subject live in **RUNTIME**, with the
producer. The workers are in **WORKER**, whose JetStream holds the
dead-letter bucket and nothing else, and reach RUNTIME's three durables only
through service imports: RUNTIME exports, to WORKER only, each durable's
`CONSUMER.INFO`, `CONSUMER.MSG.NEXT` (response type stream) and
`$JS.ACK.<stream>.<durable>.>`; WORKER imports the two API subjects under the
JetStream API prefix `JS.RUNTIME.API` (`natsconn.WorkerRuntimeJSAPIPrefix`),
which both workers use when they present an identity (async-nats
`jetstream::with_prefix`, nats-py `nc.jetstream(prefix=...)`), and WORKER's
own `$JS.API` for the bucket. Without an identity (compose's plaintext
posture, one global account) both use the default prefix.

Why the worker is not a RUNTIME user: the server answers a permitted
JetStream API request on the requester's reply subject without checking it
against the requester's permissions. In RUNTIME the worker could name
`elitea.rt.v1.<route>.d.<token>` as the reply of a pull or an info request
and have the server store the answer in a command stream: a signed command
copied across routes, or info answers filling a stream to its MaxMsgs so the
producer is refused, with no grant anywhere that could remove them. In WORKER
the answer lands in WORKER. The bucket is WORKER's for the converse reason:
in RUNTIME the producer could steer one of its own answers into
`$KV.ELITEA_RT_V1_DEADLETTER.<key>` and forge a dead letter.
`TestSecuredWorkerCannotStoreIntoCommandStreamsByReplySubject` proves both.

No other plane's identity can publish a command, read one or see the
streams, and neither command bus identity can reach MAIN's or GATEWAY's
subjects. Nobody may delete or purge a stream; only an account's bootstrap
creates or updates one, or creates a consumer.

| NATS user (URI SAN `spiffe://elitea.internal/nats/<id>`) | Account | Grants |
| --- | --- | --- |
| `elitea-main-runtime` (elitea-main's runtime plane) | RUNTIME | publish `elitea.rt.v1.{validate,agent,index}.d.*`; stream info and direct get on the three streams; consumer info on their durables; publish and subscribe `elitea.rt.v1.replay.wake` and `elitea.rt.v1.security.invalidation`; inbox `_INBOX_elitea-main-runtime.>`. Denied: `$KV.>`, `$JS.ACK.>`, `MSG.NEXT`, stream and consumer admin |
| `elitea-nats-bootstrap-runtime` (the `nats-bootstrap` hook Job, short-lived certificate) | RUNTIME | creates and reconciles the three streams and their durables (`bootstrap_runtime` in `bootstrap.sh`); no delete or purge |
| `elitea-worker` (Rust and Python workers) | WORKER | `JS.RUNTIME.API.CONSUMER.{INFO,MSG.NEXT}` and `$JS.ACK` on each stream's own durable only (the imports); dead-letter KV puts and the bucket's stream info; inbox `_INBOX_elitea-worker.>`. No stream info on the command streams (elitea-main verifies those). Denied: `elitea.rt.v1.>`, stream and consumer admin |
| `elitea-nats-bootstrap-worker` (the hook Job) | WORKER | creates and reconciles the dead-letter bucket (`bootstrap_worker`); no delete or purge |

## Autoscaling (owner decision Q8)

KEDA uses the `nats-jetstream` scaler on the worker's stream and durable. Its
metric is `num_pending + num_ack_pending` (KEDA v2.10 through v2.21,
`pkg/scalers/nats_jetstream_scaler.go` `getMaxMsgLag`): commands waiting AND
commands a replica has pulled and not acked. Because a worker pulls only for
its free delivery slots plus a small prefetch, a replica's share of that
metric is its running work; `lagThreshold` defaults to
`delivery_max_concurrency`, so KEDA asks for enough replicas to run
everything waiting or running, and a scale-in does not strand a queue that
only one pod could see. An activation threshold of 1 wakes a scaled-to-zero
fleet. The scaler reads `/jsz?acc=RUNTIME` on the NATS monitoring port of the
headless service (`worker.autoscaling.natsAccount`; a wrong account reads as
zero lag, so `natstest` pins the answer); on the HA cluster it locates the
consumer leader through `/varz`. The NATS chart's NetworkPolicy admits the
KEDA operator there only.

## Worker configuration (`runtime.json`, schema `elitea.runtime-deploy.v1`)

Both workers read the same document (Helm renders one for either image). The
command-bus fields replace every `redis_*` field; unknown fields are refused.

| Field | Meaning |
| --- | --- |
| `nats_url` | `tls://host:4222` with the client material below, `nats://host:4222` without it (compose only). No user information. A comma-separated list is a cluster seed list |
| `nats_ca_path`, `nats_certificate_path`, `nats_private_key_path` | the `elitea-worker` identity's mTLS material: all three or none. Read again on every reconnect where the client library allows it |
| `nats_stream` | `ELITEA_RT_V1_VALIDATE`, `ELITEA_RT_V1_AGENT` or `ELITEA_RT_V1_INDEX` |
| `nats_consumer` | that stream's durable (table above); any other pairing is refused |
| `consumer_id` | the NATS connection name (observability only; not an identity) |
| `limits.nats_fetch_batch` | messages per pull, 1..64 (never more than the free delivery permits) |
| `limits.nats_fetch_expires_millis` | how long one pull waits, 100..30000 |
| `limits.nats_in_progress_interval_millis` | the `+WPI` period, 1000..15000 (at most a quarter of AckWait) |
| `limits.nats_retry_delay_millis` | the retry-later nak delay, 1000..300000 |

The Rust worker reads one more setting from its environment, not from the
document: `ELITEA_WORKER_GOVERNED_CONTEXT` (`true`/`false`, also `1`/`0`,
`yes`/`no`, `on`/`off`; unset or blank is false; anything else refuses
startup) opts it into governed commands (above). A `governed_security_context`
field in the document is refused like any unknown field.

The inbox prefix is `_INBOX_elitea-worker` (the permission table allows that
one only). The poison delay (24h) and the dead-letter bucket name are
constants of the contract, not configuration.
