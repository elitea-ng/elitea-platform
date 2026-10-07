# elitea-inventory-engine

The Rust-native Inventory engine (ADR-0027) behind the sub-application host's
engine sidecar socket, built on the shared engine crates in `libs/rust`.

**State: P3a, the graph store.** It serves the Inventory tool table on the same
socket protocol the Python sidecar spoke, with two runners:

| `ELITEA_INVENTORY_RUNNER` | What answers |
|---|---|
| `unavailable` (default) | every tool is refused (`FileNotFoundError`): an engine that is not wired must look broken |
| `fixture` | the canned graph every Inventory fixture runner replays |

The knowledge graph and its PostgreSQL store are in (P3a, below); ingestion,
extraction, retrieval and `investigate` land in P3b–P4.

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

## Settings

| Variable | Default | |
|---|---|---|
| `ELITEA_INVENTORY_ENGINE_SOCKET` | `/run/inventory/engine.sock` | the Unix socket the host dials |
| `ELITEA_INVENTORY_RUNNER` | `unavailable` | `unavailable` or `fixture` (`legacy` names the Python image and is refused) |
| `ELITEA_INVENTORY_FIXTURE_STEP_SECONDS` | `0` | pause between the fixture's progress lines |
| `ELITEA_INVENTORY_FIXTURES` | packaged | a directory holding `spi/graph.json`, instead of the packaged copy |
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
