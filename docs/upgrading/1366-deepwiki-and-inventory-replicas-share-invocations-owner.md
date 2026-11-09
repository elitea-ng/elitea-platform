---
title: "DeepWiki and Inventory replicas share invocations; owner rows are marked"
issue: 1366
date: 2026-10-21
order: 17
---

**Affects:** DeepWiki and Inventory installs with a database
(`<PREFIX>DATABASE_URL`), above all a rolling upgrade from an image before
#1359. No migration and no setting.

**What changed.**

* Each sub-app host process holds a PostgreSQL advisory lock for as long as
  it lives, on a dedicated connection, keyed by a 64-bit hash of its owner
  id. A sibling checks the locks of the owners with invocations in flight at
  startup and every minute after, and terminates an owner's invocations as
  orphans only when its lock is still free 30 seconds (three claim checks)
  after the sibling first saw it free, with no held sighting between: an
  owner whose session dropped re-takes its lock well within that, and keeps
  its runs.
* The host checks its own lock every 10 seconds. When its session dropped
  (a PostgreSQL restart, a failover, an idle-session kill) it refuses new
  invocations (gRPC `UNAVAILABLE`) and fails readiness until it has re-taken
  the lock: `owner_claim: false` in its gRPC health, and `503` from
  `/readyz` on its admin port, which the chart's readiness probe and the
  compose healthcheck read, so the replica leaves the Service. The host
  sends GOAWAY to every client connection after a minute, so elitea-main's
  connection moves to a ready replica. A run a sibling terminated in that
  gap keeps the terminated answer.
* The host has a second, plain-HTTP port for its probes only:
  `ELITEA_<APP>_ADMIN_ADDR` (default `:9090`; chart
  `deepwiki.admin.port` / `inventory.admin.port`) serves `/healthz` and
  `/readyz`. The chart's probes moved from `tcpSocket` on `platform-grpc`
  to `httpGet` on `admin`; the NetworkPolicy admits
  `networkPolicies.probeFrom` (if any) to it and nothing else. The chart
  renders the variable itself and refuses it in `<app>.env`.
* Rows written by this release carry an owner prefixed `claim1:`. Rows from
  an older image carry no prefix, and that image takes no lock, so a free
  lock means nothing for them. They keep the rule the older image applied
  itself: a process that **starts** terminates every such row in flight
  (its startup reconcile), and the periodic pass never touches them. So
  during a rolling deploy from an older image, a new replica starting ends
  what the old replicas are running at that moment — exactly what an
  old replica starting did before.

**What to do.**

* To keep in-flight DeepWiki/Inventory runs across the upgrade from an
  older image, let them finish before rolling; otherwise they end with "The service restarted while this
  invocation was running", as on any restart before.
* A run an old replica accepted after the last new replica started is not
  ended until some host process starts again. If one still reports in
  progress once the rollout is done, restart the hosts once
  (`kubectl rollout restart deployment/elitea-deepwiki` /
  `deployment/elitea-inventory`).
* A pooler in front of the database must keep a client's session
  (session pooling, not transaction pooling) for the lock to mean anything;
  point `<PREFIX>DATABASE_URL` at PostgreSQL or at a session-mode pool.
