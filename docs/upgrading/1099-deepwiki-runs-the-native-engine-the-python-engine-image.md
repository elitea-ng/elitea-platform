---
title: "DeepWiki runs the native engine; the Python engine image is gone — BREAKING"
issue: 1099
date: 2026-10-07
order: 6
---

**Affects:** every Helm install with `deepwiki.enabled: true`, every values
file or Argo CD Application that sets `deepwiki.engine.*` or
`deepwiki.env.ELITEA_DEEPWIKI_RUNNER`, and compose stacks started with
`deploy/docker-compose.deepwiki-native.yml`.

**What changed.**

- The DeepWiki engine sidecar is always the Rust-native engine,
  `ghcr.io/elitea-ng/elitea-deepwiki-engine-native` (ADR-0026). The chart no
  longer runs the Python `ghcr.io/elitea-ng/elitea-deepwiki:<tag>-engine`
  image, as the sidecar or as the migrate Job.
- The default changes from `legacy` to `native`:
  `deepwiki.engine.runner: native` and
  `deepwiki.env.ELITEA_DEEPWIKI_RUNNER: "native"`.
- `deepwiki.engine.runner` accepts `native` (the engine) and `fixture`
  (canned results, for a stack that proves the hop). Both run the native
  image; `fixture` reads no database.
- `legacy` is **refused**, not aliased, on both settings, with a message
  that names this entry. It named the Python engine, and a silent alias would
  hide that the engine, its image and its resource needs all changed.
- `deepwiki.engine.image` and `deepwiki.engine.resources` (the Python
  sidecar's) are gone, and the chart **refuses to render** a values file
  that still sets either. The native sidecar's keys are unchanged:
  `deepwiki.engine.native.image`, `.resources`, `.workerCpuSeconds`.
- The `native` runner needs an `ELITEA_DEEPWIKI_DATABASE_URL` secret:
  `deepwiki.secrets.ELITEA_DEEPWIKI_DATABASE_URL`, else
  `postgresql.existingSecret`. The chart refuses to render without one (a
  plain `deepwiki.env` value does not reach the migrate Job). The engine
  keeps every index in that database.
- The migrate Job always runs the native image's `migrate`. It writes the
  same `schema_migrations` ledger with the same checksums as the Python
  runner, so a database the Python runner migrated needs no extra step.
- Compose: `deploy/docker-compose.standalone-full.yml` runs the native
  sidecar and its `elitea-deepwiki-migrate` service itself, with the
  `fixture` runner by default (`DEEPWIKI_NATIVE_RUNNER=native` for the
  engine). `deploy/docker-compose.deepwiki-native.yml` is deleted. The dev
  `deploy/docker-compose.yml` `deepwiki` profile runs the Go host, the
  native sidecar and the native migration.
- Outside Helm too: the Go host (`elitea-subapp-host`) no longer has a
  `legacy` runner. `ELITEA_DEEPWIKI_RUNNER=legacy` stops it at start with
  a pointer to this entry, as the native engine binary already did.
- The Python service (`services/elitea-deepwiki`) is deleted, and
  `ghcr.io/elitea-ng/elitea-deepwiki` is no longer built or published.
  Tags already published stay in the registry; nothing in the chart pulls
  them.

**What to do.**

1. Remove `deepwiki.engine.image` and `deepwiki.engine.resources` from every
   values file. Move any image or resource override to
   `deepwiki.engine.native.image` / `deepwiki.engine.native.resources`
   (`limits.memory` in Gi or Mi, at least 1205Mi; it sets the generation
   worker's address-space cap).
2. Replace `runner: legacy` and `ELITEA_DEEPWIKI_RUNNER: "legacy"` with
   `native`, or delete them to take the default.
3. Make sure a database URL secret reaches the component (step above). Its
   role needs `CREATE` on the database for migration 0003 (see
   `services/elitea-deepwiki-engine/README.md`).
4. Mirror `ghcr.io/elitea-ng/elitea-deepwiki-engine-native` instead of
   `elitea-deepwiki` if you pull through a registry mirror.
5. Compose: drop `deploy/docker-compose.deepwiki-native.yml` from
   `STANDALONE_OVERLAY`; the base file now does what it did.
