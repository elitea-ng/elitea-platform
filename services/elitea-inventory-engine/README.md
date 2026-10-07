# elitea-inventory-engine

The Rust-native Inventory engine (ADR-0027) behind the sub-application host's
engine sidecar socket, built on the shared engine crates in `libs/rust`.

**State: P2, the skeleton.** It serves the Inventory tool table on the same
socket protocol the Python sidecar spoke, with two runners:

| `ELITEA_INVENTORY_RUNNER` | What answers |
|---|---|
| `unavailable` (default) | every tool is refused (`FileNotFoundError`): an engine that is not wired must look broken |
| `fixture` | the canned graph every Inventory fixture runner replays |

The native knowledge-graph engine (ingestion, extraction, the PostgreSQL graph,
retrieval, `investigate`) lands in P3–P4.

## Settings

| Variable | Default | |
|---|---|---|
| `ELITEA_INVENTORY_ENGINE_SOCKET` | `/run/inventory/engine.sock` | the Unix socket the host dials |
| `ELITEA_INVENTORY_RUNNER` | `unavailable` | `unavailable` or `fixture` (`legacy` names the Python image and is refused) |
| `ELITEA_INVENTORY_FIXTURE_STEP_SECONDS` | `0` | pause between the fixture's progress lines |
| `ELITEA_INVENTORY_FIXTURES` | packaged | a directory holding `spi/graph.json`, instead of the packaged copy |
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
cargo test --locked --all-targets
cargo build --locked --release
# the host's tests against the binary
cd ../elitea-subapp-host && ELITEA_INVENTORY_NATIVE_ENGINE_BIN=$PWD/../elitea-inventory-engine/target/release/elitea-inventory-engine \
  ELITEA_REQUIRE_NATIVE_ENGINE=1 go test -race -run NativeEngine ./internal/apps/inventory/run
```
