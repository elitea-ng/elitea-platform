# Runtime command bus: the NATS JetStream contract

Status: normative for this repository. The architecture decision is the
ADR-0010 amendment "Transport binding: NATS JetStream" in `elitea-docs`; this
file is the wire and deployment contract the three parties implement:

| Party | Code |
| --- | --- |
| Producer (elitea-main) | `services/elitea-main/internal/transport/commandbus` |
| Rust worker | `services/elitea-worker-rust/src/transport/nats_jetstream.rs` |
| Python worker | `services/elitea-worker-python/src/elitea_worker/transport/nats_jetstream.py` |
| Streams, consumers, dead-letter bucket | `deploy/helm/nats-bootstrap/files/bootstrap.sh` (the only owner) |
| Who may do what | `deploy/helm/nats/values.yaml` (the permission table) |

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
with `jetstream { sync_interval: always }` (deploy/helm/nats), so a publish
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
| intake | fetch at most as many messages as there are free delivery permits (semaphore before pull), with a bounded expiry (the old BLOCK) |
| owned and working | `+WPI` (in progress) every 5s for every owned message, queued or active; it resets AckWait |
| settled (terminal receipt from main), `SETTLED`, `OBSOLETE`, `RETIRED` | double ack (`AckSync`), after the settlement receipt only; "already acknowledged" is idempotent success |
| `RETRY_LATER`, active lease held elsewhere, recovery not possible now | `NakWithDelay(60s)` |
| poison: decode or signature failure, subject token mismatch, a command this worker cannot serve | `NakWithDelay(24h)` + a dead-letter record + an ERROR log `worker_command.dead_lettered` + the dead-letter counter. Never `Term`: that frees the subject and PostgreSQL re-offers the poison every 30s |
| graceful shutdown | stop fetching, keep heartbeating owned work until it ends or the shutdown deadline passes; then stop without ack or nak, so AckWait redelivers |

A redelivery of a message whose earlier delivery is still running elsewhere
reaches the claim, which answers active-lease; the worker naks it with the
retry delay. An ack from an earlier delivery is accepted by the server; that
is safe because an ack only ever follows a terminal PostgreSQL receipt.

### Dead letter (owner decision Q3)

KV bucket `ELITEA_RT_V1_DEADLETTER`, created by the bootstrap, history 1, TTL
7 days. Key `<route>.<sha256(delivery_id)>`. Value (JSON,
`schema: "elitea.runtime.dead-letter.v1"`): `stream`, `consumer`, `subject`,
`stream_sequence`, `num_delivered`, `reason` (a stable low-cardinality code),
`worker` (the worker's client name), `recorded_at_unix_millis`. No envelope
bytes, no delivery ID, no command field: the record says where to look, not
what the command said. The alert is the bucket being non-empty
(`nats_stream_total_messages{stream_name="KV_ELITEA_RT_V1_DEADLETTER"} > 0`)
plus the workers' ERROR log line.

## Limits (owner decision Q6)

`libs/proto/elitea/runtime/v1/limits.proto` fields 4 and 5 are
`max_transport_payload_bytes` (the body, the signed envelope) and
`max_transport_message_bytes` (body plus headers, the stream's MaxMsgSize).
They were `max_redis_field_bytes` and `max_redis_entry_bytes`; the field
numbers are unchanged, and the limits revision moved to
`elitea.runtime.limits.conformance.v3` so a v2 producer and a v3 worker never
agree by accident.

## Identities and permissions (one account, owner decision Q5)

| NATS user (URI SAN `spiffe://elitea.internal/nats/<id>`) | Grants |
| --- | --- |
| `elitea-main-runtime` (elitea-main's runtime plane) | publish `elitea.rt.v1.*.d.*`; stream info and direct get on the three streams; consumer info on their durables; publish and subscribe `elitea.rt.v1.replay.wake`; inbox `_INBOX_elitea-main-runtime.>` |
| `elitea-worker` (Rust and Python workers) | stream and consumer info, `MSG.NEXT` and `$JS.ACK` on the three streams' durables; dead-letter KV puts and its stream info; inbox `_INBOX_elitea-worker.>`; may NOT publish a command, create a consumer or administer a stream |
| `elitea-nats-bootstrap` | creates and reconciles every stream, consumer and bucket |

## Autoscaling (owner decision Q8)

KEDA uses the `nats-jetstream` scaler on the worker's stream and durable. It
scales on consumer lag (`num_pending`: published, not yet delivered), not on
the old Redis pending-entries count (delivered, not yet acked), so the
threshold is lower and an activation threshold of 1 wakes a scaled-to-zero
fleet. The scaler reads `/jsz` on the NATS monitoring port, which the NATS
chart's NetworkPolicy admits from the KEDA operator only.

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

The inbox prefix is `_INBOX_elitea-worker` (the permission table allows that
one only). The poison delay (24h) and the dead-letter bucket name are
constants of the contract, not configuration.
