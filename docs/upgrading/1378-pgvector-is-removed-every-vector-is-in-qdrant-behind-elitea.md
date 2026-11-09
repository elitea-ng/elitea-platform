---
title: "pgvector is removed: every vector is in Qdrant behind elitea-vector — BREAKING"
issue: 1378
date: 2026-10-22
order: 19
---

**Affects:** every install. PostgreSQL no longer needs (or gets) the `vector`
extension. DeepWiki and Inventory keep their vectors in elitea-vector (#1359;
ADR-0031 as amended, elitea-docs#58), so a deployment that runs either
engine's `native` runner, Rust indexing or the memory toolkit needs
`vector.enabled` (or an external elitea-vector).

**What changed.**

* **PostgreSQL is plain `postgres:18`** in every compose stack, kind and CI.
  The chart's `dbInit` job, the standalone `db-init` and kind's initdb no
  longer run `CREATE EXTENSION vector`. An existing data volume or server is
  reused as it is.
* **DeepWiki**: dense vectors live in elitea-vector (source `deepwiki`, the
  shared `emb_<slug>_<dim>` collections), one namespace per wiki; a build
  writes them under its own generation and the publish flips
  `wikis.active_generation` with the nodes and edges. The engine's new
  migration `0008_vectors_in_qdrant` drops `wiki_node_embeddings` (and its
  build twin) and adds `wikis.active_generation` and the
  `wiki_stale_generations` table.
  **Every wiki published before the upgrade has no dense vectors**: it
  answers from the lexical branch only until it is regenerated.
* **Inventory**: entity vectors live in elitea-vector (source `inventory`,
  their own float32 collections `inv_<slug>_<dim>`, exact search), one
  namespace per graph. The float8[] `entities.embedding` column stays. The
  engine's `0008_entity_vectors_in_qdrant` drops the generated
  `embedding_vector` column and adds the graph row's vector columns and
  `vector_staged_generations`. **No rebuild is needed**: the first ranking of
  a graph (which carries the invocation's bearer) publishes its stored
  vectors to elitea-vector (the backfill). `orphans --delete` now keeps a
  live project's graph that still holds vectors in elitea-vector; elitea-main's
  toolkit cleanup deletes both.
* **Rewritten released migrations.** Plain PostgreSQL could not run the
  engines' released chains, which created the extension, so these released
  files were rewritten without it:

  | Engine | Migration | Released checksum still accepted |
  |---|---|---|
  | DeepWiki | `0001_wiki_index_storage` | `ea5af872e89869cdf824e92f71586ed58143efa30572c74c73a3e7bfa1bb40c3` |
  | DeepWiki | `0003_build_space` | `e36c440fb1c6d43c2900931f94f4f1c1ea023359c9e9ffbddd93e191efaf2b7e` |
  | DeepWiki | `0005_project_scope` | `fe71faa1fc925a050ad73b5d3b7313e1cc8ca926d1218fe476ae950bd3a7cac7` |
  | Inventory | `0004_entity_vectors` | `a28e5e3845a21781ec21d3d376aadb8f66648b8cd731274caa6210cbc370b44f` |

  The runner (`libs/rust/pg-migrate`, `apply_with_rewrites`) accepts exactly
  the released checksum listed for each rewritten version, besides the new
  one; any other checksum is still refused, as for every other migration.
  A database that applied the released text keeps that checksum in its
  ledger and is not re-run; a fresh install records the new text. Each
  rewritten file names its released checksum in its header.
* **The extension is dropped only when nothing needs it.** Each engine's
  `0008` drops the `vector` extension with `RESTRICT` when it exists. When
  another object still depends on it, or the migrating role does not own
  it, the migration keeps it with a NOTICE and succeeds; drop it yourself
  once nothing uses it (`DROP EXTENSION vector;` as its owner).
* **The `pgvector` configuration type is removed** (and
  `pgvector_configuration` with it). Creating a configuration of type
  `pgvector`, or switching one to it, is refused with 400. **Existing rows
  are kept but inert**: they are hidden from configuration lists and
  selectors, nothing reads them, and `GET` and `DELETE` by id still work, so
  remove them (and their vault secrets) when convenient. Toolkit settings
  lose `pgvector_configuration`: the API strips it on read and write, and the
  tenant migration `0154_strip_toolkit_pgvector_configuration` deletes the
  key from stored `elitea_tools.settings`.
* **elitea-main's per-project PgVector store is gone**: no `project_<id>`
  database or role is created for a new project (`cmd/pgvector-orphans` is
  replaced by `elitea-main legacy-pgvector-orphans`, below).
  * With `ELITEA_RUNTIME_INDEX_INGEST_DISPATCH_ENABLED=false`, project create
    used to provision a `project_<id>` PgVector database. It no longer does,
    and **such a deployment indexes nothing** (it indexed nothing that was
    read since the Python worker went). Index through the Rust worker and
    elitea-vector.
  * The project delete response no longer carries `database`.
  * **A project delete still drops the project's legacy `project_<id>`
    database and `project_<id>_user` role**, as the cleanup-journal step
    `project_pgvector_drop`, through the new
    `ELITEA_LEGACY_PGVECTOR_ADMIN_URL` (the old store's admin connection; a
    secret). The step is on a delete's journal row when that URL is set, or
    when the delete found evidence of a legacy database: the project's
    `elitea-pgvector` configuration row, or the database or role on the
    platform database's own server. **Without the URL, such a delete keeps
    its journal row open** (`last_error` set, retried with backoff, logged at
    Error naming both variables) instead of completing over the data; set
    `ELITEA_LEGACY_PGVECTOR_RETIRED=true` instead when the server is gone
    with its databases, and those steps close without a drop. The two
    variables together are a boot error. A deployment that never ran the
    store sets neither and sees no change.
  * A journal row written before the upgrade decodes as before; one that
    still owes `project_pgvector_drop` (with `had_vector_store`) runs it under
    the rule above, and one that finished it does not run it again.
* **Token introspection** (`TokenIntrospectionService`) is also served on
  elitea-main's platform listener (`:9446`) behind the same
  `ELITEA_VECTOR_INTROSPECTION_CLIENTS`, so a stack without the runtime plane
  can run elitea-vector for DeepWiki and Inventory. Callback tokens of both
  engines now carry a grant to their own source, and their default lifetime
  is 8 h (`ELITEA_{DEEPWIKI,INVENTORY}_CALLBACK_TOKEN_TTL_MINUTES`).
* **The introspection pool is 2 connections** per elitea-main replica (was 8;
  `ELITEA_RUNTIME_DB_VECTOR_INTROSPECTION_MAX_CONNS`). `values-standalone.yaml`
  declares `postgresql.maxConnections: 210` (was 200): four replicas at 44
  connections plus 25 reserved. Turning vector on no longer costs a replica.
* **New engine settings**: `ELITEA_DEEPWIKI_VECTOR_URL` and
  `ELITEA_INVENTORY_VECTOR_URL` (the elitea-vector origin,
  `https://elitea-vector:9470`), with optional `*_VECTOR_SERVER_NAME`. The
  `native` runners refuse to start without them; TLS reuses each engine's
  callback CA, certificate and key. The chart derives them from
  `vector.enabled` (a value set in the app's `env` wins) and **refuses to
  render a `native` runner with neither**.

**What to do.**

1. Turn on elitea-vector before (or with) the upgrade wherever DeepWiki's or
   Inventory's `native` runner runs. With this chart:

   ```yaml
   vector:
     enabled: true            # needs main.runtime.enabled
     qdrant:
       mode: internal         # a 3-node StatefulSet; or external + external.url
       apiKeySecret:
         name: elitea-qdrant-api-key   # create it first; key: api-key
     # recommended in production: every model either engine or the worker
     # embeds with, as "<model-slug>:<dimension>"
     allowedSpaces: []
   ```

   ```bash
   kubectl -n <ns> create secret generic elitea-qdrant-api-key \
     --from-literal=api-key="$(openssl rand -hex 32)"
   ```

   The certificates come from the `elitea-runtime-ca` ClusterIssuer the
   runtime plane already uses (`vector.mtls.issuerRef`). Keep
   `vector.collection.replicationFactor` (2) at most
   `vector.qdrant.internal.replicas` (3).
   **argocd-lab (technicaldomain)** needs exactly this block in its values,
   plus the Secret above in its namespace, before it syncs this release,
   whenever its DeepWiki or Inventory engine runs `native` (the chart's
   default runner), which the chart now refuses without vector. If it also sets `postgresql.maxConnections` from
   `values-standalone.yaml`'s old 200, raise it (and the server) to at least
   201, or keep elitea-main at three replicas.
2. **Regenerate every DeepWiki wiki** after the upgrade (each one answers
   lexically until then). Inventory needs nothing: graphs backfill on their
   first ranking.
3. Roll the engines over completely: a replica still on the previous release
   fails dense searches and publishes once the new `0008` has dropped the
   embedding tables.
4. **Drop the per-project PgVector databases.** If the deployment ever ran
   the per-project PgVector store (the legacy runtime, or elitea-main before this
   release), point elitea-main at that server's admin role, the connection
   the legacy runtime kept in the public project's `elitea-pgvector` configuration (a
   role that may drop the `project_<id>` databases and `project_<id>_user`
   roles it created; `postgresql+psycopg://` is accepted):

   ```bash
   kubectl -n <ns> create secret generic elitea-main-legacy-pgvector \
     --from-literal=admin-url='postgresql://<admin>:<password>@<pgvector-host>:5432/postgres'
   ```

   The chart reads it as `main.secrets.ELITEA_LEGACY_PGVECTOR_ADMIN_URL`
   (Secret `elitea-main-legacy-pgvector`, key `admin-url`, optional); with
   compose, add `ELITEA_LEGACY_PGVECTOR_ADMIN_URL` to the `elitea-main`
   service's environment. From then on every project delete drops its own legacy
   database and role. For projects deleted EARLIER (by the legacy runtime, or by a
   release whose journal skipped the drop), run the one-shot command in the
   same image, with the same environment (`ELITEA_LEGACY_PGVECTOR_ADMIN_URL`
   and `DATABASE_URL`):

   ```bash
   # dry run: prints the platform database, its project count and id range,
   # and every project_<id> database/role whose project no longer exists
   kubectl -n <ns> exec deploy/elitea-main -- /elitea-main legacy-pgvector-orphans
   # drop exactly the N orphans the dry run listed
   kubectl -n <ns> exec deploy/elitea-main -- \
     /elitea-main legacy-pgvector-orphans --drop --confirm-count N
   ```

   An id counts as an orphan only with no `centry.project` row and no open
   `centry.project_deletions` row, and is re-checked right before its drop.
   Check the header before `--drop`: a wrong `DATABASE_URL`, or a PgVector
   server shared with another platform, makes other deployments' databases
   look orphaned. `--drop` is refused when orphans exceed 20% of the
   server's `project_<id>` databases (`--max-orphan-fraction`), unless you
   add `--force-fraction`, and when a platform database has no projects.
   Once the server is decommissioned, replace the URL with
   `main.env.ELITEA_LEGACY_PGVECTOR_RETIRED: "true"` (compose: the same
   variable): projects created before this release keep their
   `elitea-pgvector` configuration row, and deleting one with neither
   setting keeps its journal row open. A deployment that never ran the
   store needs neither. Remove the `pgvector` configuration rows when
   convenient.
5. If an engine's `0008` logged that it kept the extension, drop it once
   nothing else uses it.
6. Compose: nothing beyond the image change for the default stacks;
   `docker-compose.standalone-full.yml` already runs elitea-vector and now
   points both engines at it. For the dev stack's `deepwiki` profile see the
   next entry.
