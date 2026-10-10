# elitea-inventory-engine

The Rust-native Inventory engine (ADR-0027) behind the sub-application host's
engine sidecar socket, built on the shared engine crates in `libs/rust`.

**State: P4, every Inventory tool served natively.** It serves the Inventory tool table on the same
socket protocol the Python sidecar spoke, with two runners:

| `ELITEA_INVENTORY_RUNNER` | What answers |
|---|---|
| `unavailable` (default) | every tool is refused (`FileNotFoundError`): an engine that is not wired must look broken |
| `fixture` | the canned graph every Inventory fixture runner replays |
| `native` | the engine itself over PostgreSQL (`ELITEA_INVENTORY_DATABASE_URL`): every tool of the `inventory` and `inventory_search` families |

The graph, the store-free ingestion steps, the read tools and the type tables
live in `libs/rust/inventory-core` (ADR-0029 decision 7: the desktop's local
workspace index links them too); this crate re-exports them under the paths
below, so `src/graph.rs`, `src/retrieval` and `src/ingest/{files,ids,parse}.rs`
here mean those files there.

The knowledge graph and its PostgreSQL store (P3a), file ingestion (P3b), the
parser stage (P3c) and the model stage (P3d) are in; communities, embeddings and
the native runner (P3e) serve `run_ingestion` on the socket; the read tools
(`src/retrieval`, held to goldens the Python handlers produced) and
`investigate` (`src/investigate.rs`: the toolkit's model with the graph tools
and the source toolkits' read-only tools through elitea-main's `test_tool`
route) complete it (P4).

The model stage (`src/extract`) uses the Python engine's prompts and type tables
as data: `libs/rust/inventory-core/assets/python_inventory.json`, frozen from its source (see
`tests/fixtures/PROVENANCE.md`). The module docs list where it deliberately differs:
absolute citations, kept text facts, and relations that are actually extracted.

## The graph and its store

`src/graph.rs` is the Python engine's `KnowledgeGraph` model: entity merge
(citations only), property filtering, one edge per ordered pair, and the
`graph.json` node-link document both ways. `tests/graph_store.rs` replays
`tests/fixtures/graph_store/ops.json` and compares with
`graph.golden.json`, which the Python graph itself wrote (frozen;
see `PROVENANCE.md`). The module docs list the Python behaviours deliberately not
carried over: stale indices, edge provenance lost on save, type
normalisation inside the store, and relation provenance by last writer: an
edge two sources (or two files) found carries `provenance`, the list of its
`{source_toolkit, discovered_in_file}` contributions, so removing one source
or re-reading one file withdraws only that contribution and the edge goes
when none is left. Python kept the last writer's `source_toolkit` only, and
removing an overlapping source deleted relations the other still said. An
edge with one contribution has no `provenance`, so single-source graphs and
the goldens are unchanged, and a Python `graph.json` loads as is.
`full_rebuild` is scoped to the source `run_ingestion` names: it forgets
what that source said (citations, entities only it cited, its relation
contributions, its document records) and reads every document again; other
sources keep their entities, relations, documents and status. Python deleted
the whole `graph.json`. As before, nothing is committed until the run
completes, so a failed or stopped rebuild leaves the previous graph.

`src/store.rs` keeps each graph as rows in the `inventory_graph` schema
(`migrations/`), addressed by project and toolkit id, instead of one
`graph.json` object per toolkit bucket. `elitea-inventory-engine migrate`
applies the migrations (ledger `inventory_graph.schema_migrations`); the
PostgreSQL tests need `INVENTORY_TEST_DSN` (see the test's header).

## Enterprise content (ADR-0028)

Ingestion reads through `elitea-content-source`: a source lists documents —
each with a version, a mime type and an ACL — and the engine fetches the new
and changed ones. Git (a checked-out tree) is the first connector. A document
that is not text (PDF, Office, spreadsheets, e-mail, HTML) is extracted by
`elitea-doc-extract` (xberg, the `documents` feature, on by default; about
+29 MB of binary) and cited like any file. The `documents` table keeps each
document's version, mime type and ACL; every read tool and `investigate` see
only what the caller may read — the user id the sub-application host
verified (`caller_user_id`) — and a caller without one sees project-wide
documents only.

## Ingestion (`src/ingest`)

One run reads one source into one toolkit's graph: the ingestion lease (one
run per graph), the source's status `in_progress`, a shallow clone of its
branch head (`elitea-repo-ingest`), the SDK loader's file selection, a hash
diff against the last completed run, and one transaction committing the graph,
the new hashes and the `completed` status. Differences from the Python engine,
on purpose:

- **Sources.** It reads the source the facade actually sends: settings and
  credentials at the top level, patterns as `file_patterns`/`exclude_patterns`.
  The nested `settings` form is still read.
- **A clone, not per-file API reads.** It needs `ELITEA_INVENTORY_GIT_ALLOWLIST`
  in the engine's own environment, set to the same value as elitea-main's. It
  is fail-closed: unset, no host is admitted.
- **Changed and deleted files really lose their old entities.** Python's
  removal step read a citation key the graph no longer had.
- **Entity ids match Python's.** They are held to ids computed by Python's own
  `_generate_entity_id` (frozen in `tests/fixtures/ingest/entity_ids.json`).

## Maintenance writes

`smart_normalize_types` is the one read-family tool that writes: the
toolkit's model (`llm_model`, through the gateway in `llm_settings`) maps
each type below `threshold` entities that is not canonical onto the
canonical set, `batch_size` types per call, with Python's prompt and a
forced call of the structured-output tool its handler bound. Unless
`dry_run`, the mapping is applied under the ingestion lease and the graph
saved in one transaction (its revision bumps, so cached views reload). It
differs from Python where Python damaged graphs: a batch the model cannot
answer refuses the run with nothing written (Python mapped the batch to
`fact` and saved), and a mapping applies only to a type the batch asked
about. `tests/fixtures/smart_normalize/goldens.json` records the Python
handler's prompts and saved graph.

A code file without a parser (`.sh`, `.rb`, `.lua`, C, …) gets its file
node and the model stage with the code fact prompt, as Python's `run()` did.
Python's `TextParser` "hybrid fallback" is not ported: only the never-served
`delta_update` reached it, and it raised on the first reference it found
(`tests/fixtures/code_like/goldens.json` records both).

## Operator runbook: moving Python graphs in, and out (issue #1129)

The Python engine kept each toolkit's graph as `graph.json` in its artifact
bucket; this engine keeps it in PostgreSQL. Nothing moves the graphs
automatically. When a deployment switches to the native engine, an operator
imports them once:

1. Point `ELITEA_INVENTORY_DATABASE_URL` at the engine's database (the
   commands run `migrate` first, so the schema is created if needed).
2. For every Inventory toolkit, download `graph.json` from its bucket
   (`bucket` / `toolkit_configuration_bucket`, default `graphs`) and note the
   project id and the toolkit (application) id.
3. Import it — idempotent, a re-run replaces the graph and bumps its revision:

   ```bash
   elitea-inventory-engine import-graph --project-id 3 --application-id 42 --file graph.json
   # or from standard input
   elitea-inventory-engine import-graph --project-id 3 --application-id 42 - < graph.json
   ```

   The report (standard error) gives the entity and relation counts and the
   embedding model the graph was embedded with.
4. Spot-check: `SELECT count(*) FROM inventory_graph.entities WHERE
   project_id = 3 AND application_id = 42` equals the file's node count.

What is refused, with nothing written (exit 1): a file that is not JSON, an
undirected graph, a multigraph, a node or link without a string id, and text
holding a NUL character (PostgreSQL `jsonb` cannot store it; the message
names where). Report such graphs; do not skip them silently. A graph that
already has native ingestion state (source status rows, document versions)
is refused too, since importing over it would leave versions and ACLs that
describe another graph; `--replace-ingestion-state` deletes that state with
the old graph. An import while an ingestion of that toolkit runs is refused.

Caveats: `sources_status.json` and the `.ingestion-checkpoint-*.json`
objects are not imported, so the first native ingestion of each source
re-reads every file (use `full_rebuild` where a changed file's old citation
must not linger). A graph embedded with a model other than the one now
configured needs re-embedding before semantic search. Edge provenance (an
edge's own `source`: `parser`, `llm`) was already lost in Python's file:
networkx overwrote it with the source node id on save.

`export-graph` writes the stored graph as Python-compatible `graph.json`
(`json.dump(indent=2)` layout), the on-demand export ADR-0027 §3 promises:

```bash
elitea-inventory-engine export-graph --project-id 3 --application-id 42 --file graph.json   # or to stdout
```

An import followed by an export gives the imported document back, except
`_metadata.last_saved` (the export's time) and the order of ids inside
`_indices` (node order; Python's was string-hash order). `tests/transfer.rs`
holds both commands to two Python-written graphs.

Both are also socket tools (descriptor revision `legacy-v2`), so a toolkit
owner runs them without cluster access:

* `import_graph` — the Go host reads `artifact_name` (default `graph.json`)
  from the toolkit's bucket (`bucket` / `toolkit_configuration_bucket`,
  default `graphs`) and hands the text to the engine in `graph_document`;
  `replace_ingestion_state` is the CLI flag. The host refuses a document over
  32 MiB: import such a graph with the command. The sidecar reads an invoke
  body up to `elitea_inventory_engine::MAX_INVOKE_BYTES` (96 MiB; other engines keep the sidecar default of 2 MB).
* `export_graph` — the engine returns `graph.json` as an artifact, and the
  host uploads it to the toolkit's bucket (a `knowledge_graph` object).

`tests/transfer_tools.rs` drives both over the socket.

## Settings

| Variable | Default | |
|---|---|---|
| `ELITEA_INVENTORY_ENGINE_SOCKET` | `/run/inventory/engine.sock` | the Unix socket the host dials |
| `ELITEA_INVENTORY_RUNNER` | `unavailable` | `unavailable`, `fixture` or `native` (`legacy` named the retired Python engine and is refused) |
| `ELITEA_INVENTORY_FIXTURE_STEP_SECONDS` | `0` | pause between the fixture's progress lines |
| `ELITEA_INVENTORY_FIXTURES` | packaged | a directory holding `spi/graph.json`, instead of the packaged copy |
| `ELITEA_INVENTORY_SOURCE_TYPES` | `github,ado_repos` | the source types ingestion reads |
| `ELITEA_INVENTORY_GIT_ALLOWLIST` | unset (no host) | the git hosts a clone may reach |
| `ELITEA_INVENTORY_MAX_CLONE_BYTES` / `_MAX_FILE_COUNT` / `_MAX_FILE_BYTES` / `_MAX_PARSED_BYTES` / `_CLONE_TIMEOUT_SECONDS` | `elitea-repo-ingest` defaults | clone limits |
| `ELITEA_INVENTORY_SCRATCH_PATH` | `/var/scratch/inventory` | where a run clones (removed after) |
| `ELITEA_INVENTORY_DATABASE_URL` | unset | the graph store (`migrate`, `import-graph`, `export-graph`, and required by `native`; `postgresql://` URL form). PostgreSQL with the pgvector extension available: migration 0004 creates it, and semantic search ranks there |
| `ELITEA_INVENTORY_CALLBACK_CA_FILE` | unset | a PEM bundle the model transport trusts besides the platform roots |
| `ELITEA_INVENTORY_MODEL_CONCURRENCY` | `8` | ingestion model calls (extraction, relations, community labels) in flight at once across the engine process; set it to the chat model server's concurrent slots (past them a request only queues there, and keeps the server busy after a stop) |
| `OTEL_EXPORTER_OTLP_(TRACES_)ENDPOINT` | unset | span export (`elitea-engine-sidecar::telemetry`) |

## The three fixture runners

The Go host's (`internal/apps/inventory/run/fixture.go`), the Python engine's
(retired Python engine's `elitea_inventory.fixture_graph`, deleted) and this one all answer from
`conformance/provider/fixtures/inventory/spi/graph.json` and are held to the
goldens beside it:

- `tests/conformance.rs` — every golden, and the packaged graph equals the
  conformance file;
- `tests/descriptor.rs` — the tool table against the descriptor the host serves;
- the host's `native_engine_test.go` — this binary over a real socket answers
  what the host's own fixture runner answers.

## Checks

```bash
cargo fmt --all -- --check
cargo clippy --locked --all-targets --all-features -- -D warnings
RUSTDOCFLAGS="-D warnings" cargo doc --locked --no-deps
INVENTORY_TEST_DSN=postgresql://… INVENTORY_REQUIRE_POSTGRES=1 cargo test --locked --all-targets
cargo build --locked --release
# the host's tests against the binary
cd ../elitea-subapp-host && ELITEA_INVENTORY_NATIVE_ENGINE_BIN=$PWD/../elitea-inventory-engine/target/release/elitea-inventory-engine \
  ELITEA_REQUIRE_NATIVE_ENGINE=1 go test -race -run NativeEngine ./internal/apps/inventory/run
```
