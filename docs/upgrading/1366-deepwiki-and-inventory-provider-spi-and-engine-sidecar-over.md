---
title: "DeepWiki and Inventory: provider SPI and engine sidecar over gRPC; cancel fan-out over NATS"
issue: 1366
date: 2026-10-21
order: 11
---

**Affects:** every install with `deepwiki.enabled` or `inventory.enabled`
(or `ELITEA_DEEPWIKI_ENABLED` / `ELITEA_INVENTORY_ENABLED` on elitea-main),
and compose stacks that run `elitea-deepwiki` / `elitea-inventory` (#1359).

**What changed.**

* elitea-main reaches each sub-application host over **gRPC on mutual TLS**,
  `elitea.subapp.v1.ProviderService` (describe, health, slots, invoke as a
  server stream, get-invocation, cancel), on the host's platform port
  (`9443`, `platform-grpc`). The host's HTTPS SPI listener (`8443`) and
  elitea-main's reverse proxy to it are gone. The browser-facing
  `/api/v2/deepwiki` and `/api/v2/inventory` routes are unchanged.
* The host and its engine sidecar speak **gRPC over the shared Unix socket**,
  `elitea.engine.v1.EngineService` (no TLS: the socket directory is the only
  way in). The NDJSON-over-HTTP protocol (`/engine/invoke`, `/engine/health`)
  is gone. **Roll the host image and the engine image together**: an old
  host cannot talk to a new engine, nor the other way round. Both run in one
  Pod, so one `helm upgrade` does it.
* A cancel that lands on one host replica now stops the run on whichever
  replica holds it: the hosts publish it on core NATS,
  `elitea.subapp.<app>.cancel.<invocation>`, in a new **`SUBAPP`** account
  (identities `elitea-deepwiki`, `elitea-inventory`; no JetStream).

**What to do.**

1. Remove `main.env.ELITEA_DEEPWIKI_BASE_URL` / `ELITEA_INVENTORY_BASE_URL`.
   The chart and elitea-main refuse them. The chart derives
   `ELITEA_<APP>_PLATFORM_GRPC_TARGET` (`elitea-<app>-svc:9443`); set it
   yourself (host:port, no scheme) only for a provider outside this release.
2. Remove `deepwiki.service.port` / `inventory.service.port` (refused): the
   Service exposes `platformGrpc.port` only. `platformGrpc.mtls` is
   mandatory and `platformGrpc.clients` must name elitea-main (the default).
3. Unset `ELITEA_<APP>_LISTEN_ADDR` on the host (refused at start); it
   listens on `ELITEA_<APP>_PLATFORM_GRPC_ADDR` (default `:9443`) and
   requires `ELITEA_<APP>_PLATFORM_CLIENTS`.
4. Upgrade the NATS chart (`deploy/helm/nats`) **before** the platform: it
   adds the `SUBAPP` account, and its guards refuse a values file that
   declares accounts without it. With NATS TLS on, the platform chart issues
   the `elitea-deepwiki` / `elitea-inventory` client certificates and mounts
   them. Without `ELITEA_<APP>_NATS_URL` the host starts and logs that the
   fan-out is off; that is fine for one replica only.
5. Compose: the hosts serve gRPC on 9443 only; elitea-main reads
   `ELITEA_<APP>_PLATFORM_GRPC_TARGET=elitea-<app>:9443`
   (`deploy/scripts/gen-nats-certs.sh` now also mints the two SUBAPP
   identities for the secured NATS overlay).
