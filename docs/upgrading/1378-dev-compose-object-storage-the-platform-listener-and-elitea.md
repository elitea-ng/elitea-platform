---
title: "Dev compose: object storage, the platform listener and elitea-vector for the `deepwiki` profile"
issue: 1378
date: 2026-10-22
order: 18
---

**Affects:** the local dev stack `deploy/docker-compose.yml` only. The chart
and the other compose files are unchanged.

**What changed.**

* elitea-main now gets `STORAGE_BACKEND=s3` with the stack's own rustfs store
  (bucket `elitea-artifacts`), as the e2e and standalone stacks do, and waits
  for `rustfs-bucket-init`. Without it, elitea-main refused to start on this
  stack: `STORAGE_BACKEND must be one of "s3", "azure", "gcs"`.
* The `deepwiki` profile gains `qdrant` and `elitea-vector` (the engine keeps
  its dense vectors there; see "pgvector is removed" above). elitea-vector's
  server and client certificates come off the provider CA, issued by
  `gen-platform-listener-certs.sh`, and it verifies the engine's tokens on
  elitea-main's platform listener. `postgres` stays plain `postgres:18`.
* The `deepwiki` profile can now reach elitea-main's platform gRPC listener
  (`:9446`, #1359). The KNOWN LIMIT about a missing listener is gone. The
  host uploads its artifacts over `ArtifactObjectService`, and the engine
  makes its model calls over `ModelService`. Both present the
  sub-application callback identity (`DNS:elitea-subapp-callback`). The
  engine's `native` runner used to refuse to start on this stack, because
  `ELITEA_DEEPWIKI_MODEL_SERVICE_URL` was unset. It now starts.
* A new unprofiled init service, `platform-material`, installs the listener's
  key into the named volume `platform_main`, owned by 65532 with mode 0600,
  because elitea-main refuses a key that a bind mount leaves 0644. It
  installs nothing and exits 0 while `ELITEA_RUNTIME_PLATFORM_ADDRESS` is
  unset, so the default stack needs no new certificates.
* This stack still has no LLM gateway. ModelService runs elitea-main's `/llm`
  surface, which exists only with `LLM_GATEWAY_URL`, so a model call fails
  there (`Unavailable`, HTTP 503). The refusal names the cause:
  `llm_gateway_not_configured`, "LLM_GATEWAY_URL is empty". Generating a wiki
  with a model needs `docker-compose.standalone-full.yml`.
* This stack still does not migrate the platform database (`SKIP_MIGRATIONS=1`;
  the schema comes from a loaded dump, `docker-compose.staging.yml`).

**What to do.** To run DeepWiki, run
`deploy/scripts/gen-platform-listener-certs.sh` after `gen-deepwiki-certs.sh`.
Then add `ELITEA_RUNTIME_PLATFORM_ADDRESS=:9446` to the `ELITEA_DEEPWIKI_*`
variables on the `--profile deepwiki up` command. The comment on the
elitea-main service and the root README give the full command. Nothing
changes for the default stack, apart from the storage fix above.
