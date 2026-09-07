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
   claim, Redis stream, delivery-index or spool state by hand.
4. Keep the version-`1` worker alive until its Redis deliveries and durable
   output spool are acknowledged, settled and drained. Confirm every
   version-`1` claim is released. A pre-authority outbox must be retired by the
   normal state machine. A post-authority outbox is retained by schema and must
   instead have an exact committed terminal settlement for the same execution
   and generation, with `SUCCEEDED`, `FAILED` or `CANCELLED` matching the
   terminal job state and a non-null job `settled_at`. Do not backfill or
   manually set `retired_at` on a post-authority outbox. Confirm the old Redis
   stream/PEL/delivery index are empty.
5. Only after step 4, scale every version-`1` Main producer to zero and stop
   every version-`1` worker. Preserve and mount every stopped replica's durable
   output-spool root.
6. Run the candidate image's `/index-v2-preflight` against the **old
   version-1** database state, Redis stream and consumer group.
7. Continue only when the command exits `0` and every reported count is zero.
   Exit `1`, exit `2`, a timeout, a missing consumer group, a missing spool
   mount or a dependency error blocks the release.

The check must be rerun after any additional durable reconciliation.

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
| `ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM` | Old version-`1` dedicated index command stream. |
| `ELITEA_RUNTIME_INDEX_INGEST_CONSUMER_GROUP` | Old version-`1` worker consumer group. |
| `ELITEA_RUNTIME_REDIS_URL` | Canonical `rediss://<acl-user>@<host>:<port>/0` URL without a password. |
| `ELITEA_RUNTIME_REDIS_PASSWORD_FILE` | Absolute in-container path to the old route's Redis ACL password file. |
| `ELITEA_RUNTIME_REDIS_CA_FILE` | Absolute in-container path to the Redis trust anchor. |

The Redis ACL identity needs only the go-redis connection/authentication
handshake (`HELLO`/`AUTH`) plus `PING`, `XLEN`, `XPENDING` and `HLEN` for the
old stream, consumer group and derived delivery-index key. Best-effort client
metadata may be attempted by the library and ignored when denied. The identity
does not need mutation commands.

Required read-only mounts:

- the Redis password file, private and owned by the container identity;
- the Redis CA file;
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
      ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM: <old-v1-stream>
      ELITEA_RUNTIME_INDEX_INGEST_CONSUMER_GROUP: <old-v1-group>
      ELITEA_RUNTIME_REDIS_URL: <old-v1-rediss-url>
      ELITEA_RUNTIME_REDIS_PASSWORD_FILE: /run/secrets/runtime-redis-password
      ELITEA_RUNTIME_REDIS_CA_FILE: /run/secrets/runtime-redis-ca.pem
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

### What is already done in this repository

- `deploy/centry-hybrid/pov-compose.yml` puts `pylon_indexer` in the `index-v1`
  profile. `compose.sh` renders and starts with `--profile runtime` only, so the
  service is absent from both. Compose has no directive that deletes a service
  a merged model declares, and Centry's model — which is not in this repository
  — still declares it, so a profile is the mechanism.
- `deploy/centry-hybrid/compose.sh config` asserts that absence and refuses a
  model that starts it.
- `services/pylon-indexer/` is deleted, with its bake target, its publish
  matrix entries, its scan matrix entry and its image-scan exemption. #509
  recorded 604 findings in the base image and said the exemption ends when the
  service goes away rather than by repairing an image we delete. This is that
  end.

### What a deployer still does live

1. **Drain first.** Run Stage A in full. Its preflight is the oracle:
   `deploy/centry-hybrid/compose.sh preflight` runs the candidate image's
   `/index-v2-preflight` against the version-1 database state, Redis stream and
   consumer group. Continue only on exit `0` with every count zero.
2. **Stop the service.** `podman compose ... stop pylon_indexer`. Preserve and
   mount every stopped replica's durable output-spool root; the preflight needs
   it and Stage A says so.
3. **Re-render and bring the stack up** with the tree as it is now. The service
   is not selected, so it is not recreated.
4. **Prove no index request reaches pylon.** Read `pylon_main`'s access log for
   the window of one index run. Finding nothing is the pass; read the window,
   not the whole file.

`deploy/centry-hybrid/scripts/cutover-rehearsal.sh` walks these steps in order
and stops at the first one that does not hold. Run it with `--dry-run` first: it
prints the plan and touches nothing.

### Rollback

`deploy/centry-hybrid/rollback/index-v1.yml` restores the service with the exact
mounts, health check and dependency it had. Apply it as a further overlay with
`--profile index-v1` in addition to `--profile runtime`; the file's header
carries the whole command.

**It is valid only before version-2 admission reopens.** That is Stage B's rule,
not a new one: after any version-2 command has been admitted, binary rollback to
version 1 is prohibited. Freeze admission, drain or terminally reconcile version
2, then roll forward.

A stack running that overlay fails `compose.sh config`, on purpose. That failure
is the signal that a rollback is in force. It is not a defect to be silenced.

### The four plugins, and what covers each

`pylon_indexer` loaded four. None is dropped by accident:

| plugin | what now serves it |
| --- | --- |
| `indexer_worker` | `services/elitea-worker-python` — the agent worker serves index ingest on the same command stream and consumer group as agent execution |
| `worker_core` | the worker's own runtime; replaced with it |
| `provider_worker` | ADR-0012 P3. The Go successor owns provider descriptors, and no hybrid route reaches this plugin |
| `runtime_engine_litellm` | **dropped**, and already dropped before this change (#323). It ran a LiteLLM proxy inside the container — a second LLM data plane with no budget and no billing. The LLM data plane is `elitea-llm-gateway`, reached through `elitea-main` at `/llm/v1`. Its configuration file is kept beside the rollback overlay, which is the only thing that would need it |
