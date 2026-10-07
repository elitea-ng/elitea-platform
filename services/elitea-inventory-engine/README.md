# elitea-inventory-engine

The Rust-native Inventory engine (ADR-0027) behind the sub-application host's
engine sidecar socket, built on the shared engine crates in `libs/rust`.

**State: P3d, ingestion with parsers and a model.** It serves the Inventory tool table on the same
socket protocol the Python sidecar spoke, with two runners:

| `ELITEA_INVENTORY_RUNNER` | What answers |
|---|---|
| `unavailable` (default) | every tool is refused (`FileNotFoundError`): an engine that is not wired must look broken |
| `fixture` | the canned graph every Inventory fixture runner replays |

The knowledge graph and its PostgreSQL store (P3a), file ingestion (P3b), the
parser stage (P3c) and the model stage (P3d) are in, as a library: no socket
tool runs them yet. Communities, embeddings, the `run_ingestion` wiring,
retrieval and `investigate` land in P3e–P4.

The model stage (`src/extract`) uses the Python engine's prompts and type tables
as data: `assets/python_inventory.json`, generated from its source by
`assets/generate.py`. The module docs list where it deliberately differs:
absolute citations, kept text facts, and relations that are actually extracted.

## The graph and its store

`src/graph.rs` is the Python engine's `KnowledgeGraph` model: entity merge
(citations only), property filtering, one edge per ordered pair, and the
`graph.json` node-link document both ways. `tests/graph_store.rs` replays
`tests/fixtures/graph_store/ops.json` and compares with
`graph.golden.json`, which the Python graph itself wrote (`generate.py`
regenerates it). The module docs list the Python behaviours deliberately not
carried over: stale indices, edge provenance lost on save, type
normalisation inside the store.

`src/store.rs` keeps each graph as rows in the `inventory_graph` schema
(`migrations/`), addressed by project and toolkit id, instead of one
`graph.json` object per toolkit bucket. `elitea-inventory-engine migrate`
applies the migrations (ledger `inventory_graph.schema_migrations`); the
PostgreSQL tests need `INVENTORY_TEST_DSN` (see the test's header).

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
  `_generate_entity_id` (`tests/fixtures/ingest/generate.py`).

## Settings

| Variable | Default | |
|---|---|---|
| `ELITEA_INVENTORY_ENGINE_SOCKET` | `/run/inventory/engine.sock` | the Unix socket the host dials |
| `ELITEA_INVENTORY_RUNNER` | `unavailable` | `unavailable` or `fixture` (`legacy` names the Python image and is refused) |
| `ELITEA_INVENTORY_FIXTURE_STEP_SECONDS` | `0` | pause between the fixture's progress lines |
| `ELITEA_INVENTORY_FIXTURES` | packaged | a directory holding `spi/graph.json`, instead of the packaged copy |
| `ELITEA_INVENTORY_SOURCE_TYPES` | `github,ado_repos` | the source types ingestion reads |
| `ELITEA_INVENTORY_GIT_ALLOWLIST` | unset (no host) | the git hosts a clone may reach |
| `ELITEA_INVENTORY_MAX_CLONE_BYTES` / `_MAX_FILE_COUNT` / `_MAX_FILE_BYTES` / `_MAX_PARSED_BYTES` / `_CLONE_TIMEOUT_SECONDS` | `elitea-repo-ingest` defaults | clone limits |
| `ELITEA_INVENTORY_SCRATCH_PATH` | `/var/scratch/inventory` | where a run clones (removed after) |
| `ELITEA_INVENTORY_DATABASE_URL` | unset | the graph store, read by `migrate` (`postgresql://` URL form) |
| `OTEL_EXPORTER_OTLP_(TRACES_)ENDPOINT` | unset | span export (`elitea-engine-sidecar::telemetry`) |

## The three fixture runners

The Go host's (`internal/apps/inventory/run/fixture.go`), the Python engine's
(`elitea_inventory.fixture_graph`) and this one all answer from
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
