# NATS command delivery source mapping

NATS JetStream is Elitea's durable command-delivery transport
(`docs/runtime-command-bus.md` at the repository root is the normative
contract). It is not an ADK session, memory, graph-state or checkpoint
backend. Main remains the command producer and PostgreSQL the durable
business-state authority; JetStream delivery ownership (an un-acked message
kept alive with `+WPI`) is transport liveness only. The Redis Streams
transport, its Lua scripts, the PEL reclaim loop and the connection-generation
owner are deleted; there is no transport flag.

## Source-to-Rust ledger

| Source evidence | Required behavior | Rust owner | Proof / status |
| --- | --- | --- | --- |
| Contract "Subjects and headers"; Main `transport/commandbus/contract.go::{RouteToken,FilterSubject,DeliveryToken,DeliverySubject,DeadLetterKey,ValidateRoute,KnownStreams}` | Streams `ELITEA_RT_V1_<ROUTE>`, durables per stream, subject `elitea.rt.v1.<route>.d.<sha256(delivery_id)>`, header names, dead-letter bucket and key, poison delay, `AckWait`, request bounds | `src/transport/command_bus.rs` constants and `{route_token,consumer_for_stream,valid_route_pair,filter_subject,delivery_token,delivery_subject,dead_letter_key}` | Implemented. `delivery_token("outbox-1")` is pinned to `ff7cc06f…b42164` like the Go test |
| Contract "Worker configuration" | `nats_url` (`tls://` with the mTLS trio, `nats://` without it, no user information, comma-separated seed list of one scheme), `nats_ca_path`/`nats_certificate_path`/`nats_private_key_path` (all or none), `nats_stream` + `nats_consumer` (one contract pair), `consumer_id` = connection name, `limits.nats_{fetch_batch,fetch_expires_millis,in_progress_interval_millis,retry_delay_millis}` | `src/config.rs::{RuntimeDeployConfig,RuntimeLimits}`; `src/bootstrap.rs::nats_transport_config`; `src/transport/nats_jetstream.rs::{NatsJetStreamConfig::validate,parse_server_urls}` | Implemented; `deny_unknown_fields` refuses every former `redis_*` field. `max_transport_message_bytes` (65536) / `max_transport_payload_bytes` (49152) replace the Redis entry/field limits (limits conformance v3) |
| Contract "Identities and permissions"; `deploy/helm/nats/values.yaml` `elitea-worker` row | Bind only (stream info, consumer info, `MSG.NEXT`, `$JS.ACK`, dead-letter KV put and its stream info); inbox `_INBOX_elitea-worker`; never publish a command, create a consumer or administer a stream | `NatsCommandBus::{connect,bind}` (`get_stream`, `get_consumer`, `get_key_value`), `owned_reply` | Implemented. Every ack-subject operation is refused unless the subject is this durable's `$JS.ACK.<stream>.<consumer>.…`. The live test greps the server log for a worker permission violation (none) after proving the grep sees one for another identity |
| Contract "Consumers and workers" table | Refuse to start if the durable is absent or drifted: pull, explicit ack, `AckWait` 60s, `MaxDeliver` -1, `DeliverPolicy` all, no `BackOff`, filter `elitea.rt.v1.<route>.d.*`, request bounds admitting the configured pull; stream is the route's WorkQueue stream | `nats_jetstream.rs::{verify_stream,verify_consumer}` | Implemented; unit tests drift each field. Absence (`get_stream`/`get_consumer`/`get_key_value` failure) is a configuration error at bootstrap |
| Contract mTLS; Go `libs/go/natsconn` | TLS 1.3 only, private CA (no system roots), the `elitea-worker` client certificate; refuse `tls://` without material and material without `tls://` | `nats_jetstream.rs::{reloading_tls_config,ReloadingServerVerifier,ReloadingClientIdentity,read_material,preflight_tls_material}` | Implemented with rustls 0.23 + ring (async-nats `0.50.0`, `default-features = false`, features `jetstream`, `kv`, `ring`, `server_2_10`, `server_2_11`, `server_2_12`). **Reload:** the client identity is a `ResolvesClientCert` and the CA a `ServerCertVerifier` that both read their files on every TLS handshake, so a cert-manager renewal (`..data` symlink swap) takes effect on the next reconnect. The files are read plainly (symlinks followed, size-bounded, no owner-only check), like `natsconn`, because the Secret volume is root-owned and group-readable. The material is also read once before connecting so a malformed identity fails startup. Caveat: async-nats also loads the platform roots when a custom config is supplied (they are not used for verification); a platform store that fails to load fails the handshake |
| Contract worker behaviour "intake" | Semaphore before pull: fetch at most `min(nats_fetch_batch, free permits)` with a bounded expiry; bounded queue | `src/execution/command_delivery.rs::{CommandDeliveryIntake::next_batch,reserve_capacity,bind_deliveries}`; `NatsCommandBus::fetch` (`consumer.batch().max_messages(n).expires(d)`) | Implemented. A redelivery of a message this worker still owns (same stream sequence) is not admitted twice and not nak'd |
| Contract "owned and working" | `+WPI` every interval for every owned message, queued or active | `heartbeat_worker`, `CommandDeliveryIntake::heartbeat_owned`, `NatsCommandBus::in_progress` | Implemented. A round publishes `+WPI` per owned ack subject, then `flush`es; a round that sees a disconnect or spans a reconnect epoch is reported unconfirmed (retried next tick, never assumed delivered) |
| Contract "settled … double ack, after the settlement receipt only; already acknowledged is idempotent success" | Ack only after the terminal PostgreSQL receipt, bound to the verified command, its authority, the exact delivered bytes and the subject token | `command_bus.rs::CommandRetirer::retire_command`; `NatsCommandBus::retire_delivery` (`+ACK` as request/reply = `AckSync`) | Implemented. nats-server 2.12 answers an ack of an already-acknowledged message like a first ack (pinned live: the repeated double ack is confirmed). A timeout is retryable and leaves the settlement marker unset |
| Contract "RETRY_LATER, active lease elsewhere, recovery not possible now" | `NakWithDelay(nats_retry_delay_millis)` | `command_delivery.rs::dispose_processed` | Implemented: any processed-but-not-retired delivery is nak'd (request/reply, confirmed) while still owned |
| Contract "poison" | `NakWithDelay(24h)` (never `Term`) + dead-letter record + ERROR `worker_command.dead_lettered` + counter | `dispose_poison`; `command_bus.rs::{PoisonReason,DeliveryVerdict,DeadLetterRecord}`; `verification_poison`; `CommandDelivery::bind_idempotency_key` in `execution_delivery_processor.rs` and `agent_delivery_processor.rs` | Implemented. Poison = malformed subject or body (transport, never reaches the processor), envelope decode/version failure, signature/key failure, subject token ≠ `sha256(idempotency_key)` (checked after verification, before the claim), unsupported command. Record: `schema`, `stream`, `consumer`, `subject`, `stream_sequence`, `num_delivered`, `reason`, `worker`, `recorded_at_unix_millis`; key `<route>.<sha256(idempotency_key)>`, else the subject token, else `sha256(subject)`. No envelope bytes, delivery ID or command field. The crate has no metrics exporter, so the counter is a process-wide atomic carried on every ERROR event as `dead_lettered_total` |
| Contract "graceful shutdown" | Stop fetching, keep heartbeating owned work until it ends or the deadline; then stop without ack or nak | `CommandDeliveryRuntime::{run,drive_intake}`, `drain_workers` | Implemented. Work that finishes during the drain gets its normal disposition; work still running at the deadline is aborted with no ack/nak (AckWait redelivers). The connection is drained last |
| Contract "reconnect" (risk 6) | async-nats reconnects internally; buffered acks must not be treated as delivered; events observable | `ConnectionObserver` (event callback), request/reply acks/naks, epoch-checked heartbeats | Implemented: `nats_connected`, `nats_disconnected` (with the new epoch), `nats_reconnected`, lame-duck, slow-consumer, server/client error events are logged without server text |

## Failure and logging policy

`NatsJetStreamError` exposes stable low-cardinality codes
(`nats_jetstream.*`) and retryability; library error text is dropped because
it can echo subjects or server text. Configuration, authentication and drift
are startup failures, not reconnect loops. Pull timeouts and dependency loss
are retried by the runtime after `dependency_retry_millis`. Nothing logs
payloads, signed envelopes, credentials or certificates; dead-letter events
carry only coordinates and hash-token subjects.

## Proofs

- Unit: `transport/command_bus.rs` (contract vector, ack-subject parsing,
  decode bounds, dead-letter shape), `transport/nats_jetstream.rs` (URL, TLS
  material pairing, symlinked material reads, drift refusal),
  `execution/command_delivery_tests.rs` (fake transport: permits before pull,
  duplicate suppression, `+WPI` through drain, disposition table, failed
  dead-letter write, reconnect-spanning heartbeat, drain timeout without
  ack/nak, cancellation, bounds), `tests/command_retirement_contract.rs` and
  `tests/agent_delivery_route_contract.rs` (retirement binding and routing).
- Live: `execution/nats_live_tests.rs` runs the real transport and runtime as
  `elitea-worker` against natstest-serve (the chart's rendered permission
  table and the real bootstrap) — double ack removes the message, nak-delay
  redelivers, poison is dead-lettered and stays pending, `+WPI` keeps a
  66-second job past the 60-second `AckWait` without redelivery, and no worker
  permission violation is logged. `ELITEA_REQUIRE_NATS_SECURE_TEST=1` (CI)
  turns a missing environment into a failure.

## Not proved here

Server restart / leader change mid-delivery, KEDA scaling on consumer lag and
a multi-replica fleet are deployment proofs (remaining gates), not unit or
single-server live tests.
