---
title: "The platform-edge Traefik is gone; elitea-main serves the worker over gRPC + mTLS on :9446"
issue: 1366
date: 2026-10-21
order: 10
---

**Affects:** every install that runs the agent worker (`worker.enabled=true`),
sets `main.runtime.currentMainBaseUrl` to an https origin, or sets
`deepwiki.callbackViaPlatformEdge`. Compose: `docker-compose.standalone-full.yml`
(#1359).

**What changed.** The worker's `platform_origin` (its `/api/v2` + `/llm` calls,
the Office host, indexing artifact reads and internal MCP tools) used to go
through `elitea-platform-edge`, a second Traefik with its own certificate.
elitea-main now serves that surface itself on an internal **platform
listener**, `main.runtime.listeners.platformAddress` (default `:9446`), over
TLS 1.3 that **requires a client certificate signed by the runtime CA**. It
reuses the content listener's keypair (`content-server.crt`, SAN
`elitea-main`). The NetworkPolicy admits the worker (and, with the new flag
below, DeepWiki) to 9446.

**:9446 is a gRPC listener, not HTTP.** Every internal hop is gRPC over mTLS:
the port speaks HTTP/2 gRPC only (ALPN `h2`), and each gRPC service on it
admits only the workload identities (certificate SPIFFE URI or DNS SAN)
configured for it. It serves no REST route: `curl`, `openssl s_client` with an
HTTP request line, or an HTTP liveness probe against 9446 no longer gets an
answer. Check it with gRPC health (`grpc.health.v1.Health/Check`, open to any
runtime-CA client with a workload identity), for example with the
`elitea-grpc-health` tool in the elitea-main image:
`/elitea-grpc-health --addr elitea-main:9446 --ca runtime-ca.crt --cert
agent-worker-client.crt --key agent-worker-client.key`. The address,
certificate and chart/env names (`ELITEA_RUNTIME_PLATFORM_*`,
`main.runtime.listeners.platformAddress`) are unchanged.

**What to do.**

1. Delete `worker.platformEdge` from your values. The chart refuses to render
   while `worker.platformEdge.enabled` is true, or while
   `worker.runtime.platformOrigin` still names `elitea-platform-edge`.
2. Set `worker.runtime.platformOrigin: https://elitea-main:9446` (the new
   default) if you override it.
3. Delete the `elitea-platform-edge-tls` Secret; nothing reads it.
4. elitea-main no longer calls itself: the next-input-suggestion policy
   lookup is in process. `main-platform-client.crt` / `.key` and
   `ELITEA_RUNTIME_PLATFORM_CLIENT_CERT_FILE` / `_KEY_FILE` are not read any
   more; remove them from the runtime material Secret and any env overrides.
   `main.runtime.currentMainBaseUrl` stays (it is the internal MCP origin
   handed to the worker); `https://elitea-main:9446` needs no Main client
   certificate. `deploy/scripts/gen-runtime-certs.sh` deletes the retired
   files from an existing tree.
5. `deepwiki.callbackViaPlatformEdge` was removed and now fails the render; it
   is not mapped onto the new key because the new hop needs a client
   certificate you have to mint. Use `deepwiki.callbackViaPlatformListener:
   true` and create the `deepwiki.callbackClient.secretName` Secret
   (default `elitea-deepwiki-callback-client`, keys `tls.crt` / `tls.key`) from
   `subapp-callback-client.crt` / `.key` (runtime CA, clientAuth EKU).
6. If `GATEWAY_SELF_LLM_ORIGINS` lists the worker's model origin, replace
   `https://elitea-platform-edge/llm/v1` with `https://elitea-main:9446/llm/v1`.
7. Compose: re-run `deploy/scripts/gen-runtime-certs.sh` (it removes the
   retired `platform-edge.*` and `main-platform-client.*` files and issues the
   sub-application callback identity), then
   recreate the stack. The `standalone_runtime_edge` volume is unused and can
   be removed.
8. `main.env.LLM_GATEWAY_URL` must name the gateway's gRPC listener,
   `https://elitea-llm-gateway-grpc:8443` (`llmGateway.service.grpcName` /
   `grpcPort`). The chart refuses the HTTP port (`:8083`, health and metrics
   only), which values written for the edge Traefik often still name.
