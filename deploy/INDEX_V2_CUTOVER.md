# Index capability v1 to v2 release cutover

This runbook is mandatory for the first release that admits index capability
version `2`. It is a two-stage replacement, not a mixed-version rolling
deployment. The candidate `elitea-main` image ships `/index-v2-preflight` as a
one-shot operator command; the normal image entrypoint remains
`/elitea-main`.

The preflight is observational. It never cancels, retires, deletes, repairs or
reconciles work. A non-zero result means indexing admission stays closed and
operators return to the current durable recovery path.

## Stage A: freeze and drain version 1

1. Close indexing admission at ingress.
2. Keep one `ac96452`-compatible version-`1` Main initializer/outbox publisher
   on the old route and the exact pinned version-`1` worker running. Verify the
   standard migrator's recorded migration head includes
   `0051_index_meta_initialization_recovery.sql`; do not execute its SQL
   manually. That migration marks interrupted pre-authority admissions
   recoverable, but the live Main initializer and outbox publisher must
   materialize and publish them.
3. Recover retryable work or terminally reconcile failed/cancelled work through
   the normal fenced durable state machine. For this rollout, explicitly prove
   execution `4ceb724db45501c2cb9b142422f368db` and every other version-`1`
   execution terminally settle. Never update or delete execution, outbox,
   claim, command-bus message or spool state by hand.
4. Keep the version-`1` worker alive until its command-bus deliveries and durable
   output spool are acknowledged, settled and drained. Confirm every
   version-`1` claim is released. A pre-authority outbox must be retired by the
   normal state machine. A post-authority outbox is retained by schema and must
   instead have an exact committed terminal settlement for the same execution
   and generation, with `SUCCEEDED`, `FAILED` or `CANCELLED` matching the
   terminal job state and a non-null job `settled_at`. Do not backfill or
   manually set `retired_at` on a post-authority outbox. Confirm the command
   stream, its durable and its delivery subjects are empty.
5. Only after step 4, scale every version-`1` Main producer to zero and stop
   every version-`1` worker. Preserve and mount every stopped replica's durable
   output-spool root.
6. Run the candidate image's `/index-v2-preflight` against the **old
   version-1** database state and the index stream and its durable.
7. Continue only when the command exits `0` and every reported count is zero.
   Exit `1`, exit `2`, a timeout, a missing consumer group, a missing spool
   mount or a dependency error blocks the release.

The check must be rerun after any additional durable reconciliation.

### The reusable drain order

A recreation of `elitea-main` inside one release does not need a versioned
cutover. It needs the generic drain order. Stop the worker fleet. Recreate
the main replica. Restore the worker fleet.

`deploy/scripts/drain-workers.sh` runs those kubectl steps and prints each
one. `deploy/README.md` states the order and the commands under "Scaling
order with live workers (#968)". Stage A above is the versioned form of the
same order.

That order does not replace this cutover. A version change still needs this
runbook's preflight and the versioned stream switch.

## Dedicated preflight service contract

Use the exact candidate `elitea-main` image and override its entrypoint to
`/index-v2-preflight`. The one-shot service needs no published port and must
not start `/elitea-main`.

Required environment:

| Name | Requirement |
| --- | --- |
| `DATABASE_URL` | Authoritative PostgreSQL URL. Its role needs `USAGE` on schema `elitea_runtime` and `SELECT` on `elitea_runtime.execution_jobs`, `elitea_runtime.command_outbox`, `elitea_runtime.execution_claims` and `elitea_runtime.execution_settlements`. Include the production TLS policy. |
| `ELITEA_RUNTIME_ENABLED` | Exact value `true`. |
| `ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED` | Exact value `true`. |
| `ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM` | The index route's command-bus stream (`ELITEA_RT_V1_INDEX`, or `ELITEA_RT_V1_AGENT` where index shares the agent stream). Its durable is fixed by the stream. |
| `ELITEA_RUNTIME_NATS_URL` | `tls://<nats-host>:4222`, no credential. |
| `ELITEA_RUNTIME_NATS_TLS_{CA,CERT,KEY}_FILE` | The `elitea-main-runtime` NATS identity (all three, or none for a plaintext compose NATS). |

The `elitea-main-runtime` identity's grants cover what the preflight reads —
`STREAM.INFO` and `CONSUMER.INFO` — and nothing it could change. "Drained" is
`State.Msgs`, `NumAckPending + NumRedelivered + NumPending` and
`State.NumSubjects` all zero (docs/runtime-command-bus.md).

Required read-only mounts:

- the `elitea-main-runtime` NATS client certificate, key and CA;
- PostgreSQL CA/client identity files referenced by `DATABASE_URL`, when its
  TLS mode uses files;
- every old worker replica's durable output-spool root, each passed once as an
  absolute canonical `--spool-root` argument.

The spool directory itself must not be a symlink and must not grant group or
other permissions. Mounting only a shared parent, only currently running
replicas or a newly empty directory is not valid coverage. No runtime command
signing key, worker verification keyring, gRPC listener certificate or server
private key is required by this service and none should be mounted.

Example shape, with deployment-specific secret and volume names:

```yaml
services:
  index-v2-preflight:
    image: <candidate-elitea-main-image>
    entrypoint: ["/index-v2-preflight"]
    command:
      - --spool-root
      - /mnt/index-worker-0/output-spool
      - --spool-root
      - /mnt/index-worker-1/output-spool
    environment:
      DATABASE_URL: <old-release-database-url>
      ELITEA_RUNTIME_ENABLED: "true"
      ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED: "true"
      ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM: ELITEA_RT_V1_INDEX
      ELITEA_RUNTIME_NATS_URL: tls://elitea-nats:4222
      ELITEA_RUNTIME_NATS_TLS_CA_FILE: /etc/runtime-nats-client/ca.crt
      ELITEA_RUNTIME_NATS_TLS_CERT_FILE: /etc/runtime-nats-client/tls.crt
      ELITEA_RUNTIME_NATS_TLS_KEY_FILE: /etc/runtime-nats-client/tls.key
    read_only: true
    volumes:
      - <worker-0-spool>:/mnt/index-worker-0/output-spool:ro
      - <worker-1-spool>:/mnt/index-worker-1/output-spool:ro
```

Centry owns the concrete service composition, secret objects, network policy
and complete replica-specific volume list.

## Stage B: activate version 2

1. Keep indexing admission closed.
2. Deploy all version-`2` workers on a **new** versioned stream and consumer
   group. Verify health and advertised capability version `2`.
3. Deploy version-`2` Main on the same new route and verify health.
4. Reopen indexing admission only after Main and every worker agree on version
   `2`.

Before version-`2` admission reopens, rollback may restore the stopped
version-`1` release on its unchanged old route. After any version-`2` command
is admitted, binary rollback to version `1` is prohibited. Freeze admission,
drain or terminally reconcile version `2`, then roll forward or run a separately
reviewed symmetric cutover. Never attach a version-`1` process to a
version-`2` stream or consumer group. Final version-`2` Main and workers must
reject version-`1` commands; they are not a recovery path for old work.

## Stage C: retire the version-1 index plane

Issue #339. Stages A and B move the traffic. Stage C removes the service that
used to carry it. Do it as a separate act, after version 2 has served real work,
and never as part of the same change.

Machines here use **podman**: `podman compose`, not `docker compose`.

### Status: the mixed (centry-hybrid) deployment is retired

Stage C was written for the mixed deployment in `deploy/centry-hybrid`, which
put `pylon_indexer` in an `index-v1` Compose profile, shipped a
`cutover-rehearsal.sh` walk-through and a `rollback/index-v1.yml` overlay. That
directory is gone (see `docs/UPGRADING.md`, "runtime-redis removed"): it
depended on the private legacy centry repository, and its runtime still wrote
the Redis command-bus fields that both workers now refuse. The command bus is
NATS JetStream (`docs/runtime-command-bus.md`), so there is no version-1 Redis
stream or consumer group left to drain, and no version-1 rollback overlay.

`services/pylon-indexer/` is deleted too, with its bake target, its publish
matrix entries, its scan matrix entry and its image-scan exemption. #509
recorded 604 findings in the base image and said the exemption ends when the
service goes away rather than by repairing an image we delete.

A deployment still running the legacy index plane migrates by Stages A and B
against its own stack, then stops `pylon_indexer` there. Version-2 admission
has no binary rollback to version 1, which was Stage B's rule all along.

### The four plugins, and what covers each

`pylon_indexer` loaded four. None is dropped by accident:

| plugin | what now serves it |
| --- | --- |
| `indexer_worker` | `services/elitea-worker-python` — the agent worker serves index ingest on the same command stream and consumer group as agent execution |
| `worker_core` | the worker's own runtime; replaced with it |
| `provider_worker` | ADR-0012 P3. The Go successor owns provider descriptors, and no hybrid route reaches this plugin |
| `runtime_engine_litellm` | **dropped**, and already dropped before this change (#323). It ran a LiteLLM proxy inside the container — a second LLM data plane with no budget and no billing. The LLM data plane is `elitea-llm-gateway`, reached through `elitea-main` at `/llm/v1`. Nothing in the repository configures it any more |

### Retired with the service: the vault-parity gate

`services/elitea-main/tests/vaultparity` asserted that pylon-indexer and
elitea-main read one `SECRETS_MASTER_KEY` and could read each other's Fernet
vaults (issue 418). With `services/pylon-indexer` and its rollback
profile deleted, elitea-main is the only compose service that carries the
key, so the package's subject no longer exists and it
was removed in the same change rather than left to fail on a path that is
gone. The single-key rule for elitea-main itself is still enforced by the
Helm render suite.
