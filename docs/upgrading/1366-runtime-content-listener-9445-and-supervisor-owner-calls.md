---
title: "Runtime content listener (:9445) and supervisor owner calls are gRPC"
issue: 1366
date: 2026-10-21
order: 13
---

**Affects:** every install with the runtime plane enabled
(`main.runtime.enabled`, `ELITEA_RUNTIME_CONTENT_ADDRESS`): elitea-main, the
agent worker, and the sandbox supervisor; also installs that set
`ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG` (#1359).

**What changed.**

* elitea-main's content listener (`:9445`, same address, TLS files and client
  CA) now serves **`elitea.runtime.v1.RuntimeContentService` over gRPC** and
  no HTTP at all: agent inputs, runtime context and tokens, artifacts, Code
  workspace / debug / platform / recovery documents, toolkit discovery
  results, sandbox bundles and compiled snapshots. Inputs and bundle files
  stream in 64 KiB chunks after a header carrying length and SHA-256. The
  execution claim and fence travel as `elitea-claim-id` /
  `elitea-claim-fence-bin` call metadata (they replaced `X-Elitea-Claim-Id` /
  `X-Elitea-Fence`); a sandbox grant travels in the request (it replaced
  `X-Elitea-Sandbox-Bundle-Grant` / `-Compiled-Grant`). The JSON documents
  themselves are unchanged. The listener also answers `grpc.health.v1` to
  any runtime-CA workload identity, so a client certificate must carry
  exactly one DNS or SPIFFE SAN (the certificates `gen-runtime-certs.sh` and
  the chart issue already do).
* Main's owner calls to the sandbox supervisor (node Code recovery read /
  seal-no-effect, Code platform retained runtime / pending call / reply) are
  RPCs on `elitea.runtime.v1.SandboxSupervisorService`; the JSON-over-HTTP/2
  routes `/elitea.runtime.node-code-recovery.v1/...` and
  `/elitea.runtime.code-platform.v1/...` are gone.
* Health and metrics are unchanged: elitea-main's probes stay plain HTTP on
  `:8080`, the supervisor's readiness stays a TCP check, and neither needs a
  client certificate.

**What to do.**

1. **Roll elitea-main, the agent worker and the sandbox supervisor
   together.** An old worker or supervisor cannot read content from a new
   elitea-main, nor the other way round. With the chart, one `helm upgrade`
   does it.
2. Keep `content_origin` (worker) and the supervisor's content `origin` in
   their `https://host:port` form; the port is now required and is dialled
   as gRPC. `ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG.supervisors[].https_origin`
   keeps its name and form but must now include the port of the
   supervisor's gRPC listener (the one `SandboxSupervisorService` already
   used); an origin without a port is refused at startup.
3. Replace any HTTP probe or `curl` check against `:9445` with
   `/elitea-grpc-health --addr elitea-main:9445 ...` from the elitea-main
   image (the standalone stack's `check` does this). A plain TLS handshake
   check still works.
