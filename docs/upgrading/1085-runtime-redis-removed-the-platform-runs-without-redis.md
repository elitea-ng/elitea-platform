---
title: "runtime-redis removed: the platform runs without Redis — BREAKING"
issue: 1085
date: 2026-10-06
order: 5
---

**Affects:** every Helm install whose values still carry a `runtimeRedis`
block (including `enabled: false`), every Argo CD Application built on those
values, every compose stack started from `deploy/docker-compose.standalone-full.yml`,
and anyone using the mixed `deploy/centry-hybrid` stack.

**What changed.**

- Nothing reads Redis any more. The runtime command bus is NATS JetStream
  (`docs/runtime-command-bus.md`), the Form sign-in state is in PostgreSQL
  (entry below), and live updates and canvas presence are on NATS. The plain
  `redis`/`elitea-valkey` was already gone.
- The chart no longer has `templates/runtimeRedis/` or a `runtimeRedis` values
  block, and it **refuses to render** a values file that still sets
  `runtimeRedis` at all (`runtimeRedis.enabled`, `.materialSecretName`,
  `.persistence`, `.bootstrap`, `.service`, … — any key under it), with a
  message naming this entry. The worker keys `worker.runtime.redisUrl`,
  `redisStream`, `redisGroup`, the `redis_*` limits, `main.runtime.redis` and
  an auth document `redis:` block were already refused.
- Compose: the `runtime-redis` service, its `standalone_runtime_redis` material
  volume, its `standalone_runtime_redis_data` AOF volume and
  `deploy/runtime/redis.conf` are gone.
- `deploy/scripts/gen-runtime-certs.sh` no longer mints `redis-server.crt`/`.key`,
  `redis-users.acl` or the `redis-{producer,worker,bootstrap}-password` files,
  and deletes them (and a leftover `redis-auth-password`) from an existing
  `deploy/certs/runtime/` tree in place. Nothing else in the tree is rotated.
  The runtime CA stays: it signs elitea-main's control/output/content listeners
  and the worker's workload identity.
- `deploy/centry-hybrid` is **retired** (owner decision: retire, don't port).
  It depended on the private legacy centry repository, and its runtime still
  wrote the Redis command-bus fields both workers refuse. The `hybrid:*` Task
  targets, the cutover-rehearsal suite and the Centry-profile reliability
  harnesses (`TestExistingComposeIndexReliability`, the issue #5681
  production-scale gate) went with it. `deploy/scripts/migrate-artifacts.sh`
  (the libcloud → object-store copy, formerly under the hybrid's `scripts/`)
  is kept; `deploy/ARTIFACT_CUTOVER.md` describes its use.

**What to do.**

1. Delete the `runtimeRedis` block from every values file (and from Argo CD
   `valuesObject`/`helm.values`). The render fails until you do.
2. After the upgrade, delete what the old release or your own tooling left
   behind. A `helm upgrade` removes the chart-owned Deployment, Service and
   ConfigMap; the data PVC always survives (the chart annotated it
   `helm.sh/resource-policy: keep`), as does anything an Argo CD
   `Prune=false` or a storage-class retain policy kept, and the material
   Secret was always yours:

   ```bash
   kubectl -n <ns> delete deployment,service elitea-runtime-redis --ignore-not-found
   kubectl -n <ns> delete configmap elitea-runtime-redis-config --ignore-not-found
   kubectl -n <ns> delete pvc elitea-runtime-redis-data --ignore-not-found
   kubectl -n <ns> delete secret elitea-runtime-redis-material --ignore-not-found
   ```

   (Names are the chart defaults; use your `runtimeRedis.service.name` /
   `fullnameOverride` if you changed them.) Then release the PersistentVolume
   if its reclaim policy is `Retain`.
3. Drop any `redis-*` keys from the runtime material Secret
   (`main.runtime.material.secretName`) and the auth material Secret if they
   are still there. Nothing reads them, and they are live credentials to a
   server that no longer exists.
4. Compose: `podman compose -f deploy/docker-compose.standalone-full.yml down`
   with `--remove-orphans`, then `podman volume rm <project>_standalone_runtime_redis
   <project>_standalone_runtime_redis_data` (the second exists only on stacks
   that ran the AOF-persistent runtime-redis).
   Re-run `deploy/scripts/gen-runtime-certs.sh` (or `task standalone:up`,
   which runs it) to prune the retired files from `deploy/certs/runtime/`.
5. Mixed-deployment users: there is no in-repo replacement for
   `deploy/centry-hybrid`. Move to the Helm chart or the standalone stack.
