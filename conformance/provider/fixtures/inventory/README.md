# Inventory conformance fixtures

`descriptor/` and `invoke/` predate this note (see `descriptor/README.md`).
`spi/`, `ingestion/` and `retrieval/` are the canned knowledge graph BOTH
fixture runners replay — the two-runner-drift trap
`support-inventory-wiki-survey.md` (INV-2) warns about, made mechanical.

## The runners

| Runner | Where | Stack that runs it |
|---|---|---|
| Go | `services/elitea-subapp-host/internal/apps/inventory/run/fixture.go` | the E2E stack (`ELITEA_INVENTORY_RUNNER=fixture` on the Go host — no engine sidecar at all) |
| Rust | `services/elitea-inventory-engine/src/fixture.rs` (ADR-0027; packaged copy `src/fixtures/graph.json`) | the native engine sidecar, `ELITEA_INVENTORY_RUNNER=fixture` — `tests/conformance.rs` holds it to these files, and the host's `native_engine_test.go` to the Go runner over the socket |

The Python engine's runner (`elitea_inventory.fixture_graph`) was a third
until the Python service was deleted; parity is now Go against Rust
(provenance: `services/elitea-inventory-engine/tests/fixtures/PROVENANCE.md`).

Both answer from the SAME graph: six entities (two source toolkits — `code`,
`docs` — three types, two layers) and five relations. `code:payment-client` is
the only entity with more than one inbound edge, on purpose: it is what
`impact_analysis` and `get_related_entities` have something to say about.

## The files

- `spi/graph.json` — the graph itself: `entities`, `relations`, `presets`.
  The Go side's copy is the `FixtureEntities`/`FixtureRelations`/
  `FixturePresets` constants in `fixture.go`; a Go test in that package
  (`fixture_parity_test.go`) asserts they equal this file byte-for-shape.
  The Rust side's copy is packaged at
  `services/elitea-inventory-engine/src/fixtures/graph.json` (compiled in);
  `tests/conformance.rs` there asserts it holds the same data as this file.
  **Edit this file, then the packaged copy, and re-run both tests** —
  editing only one side's copy is exactly the drift this exists to catch.
- `ingestion/*.json` and `retrieval/*.json` — one file per representative tool
  call: `{"tool", "params", "expected"}`. `expected` is the tool's JSON
  document (the `output_format=json` answer), not its markdown rendering —
  the shape is what a browser journey and the parity tests assert on, not the
  prose. Both `fixture_parity_test.go` (Go, calling the unexported fixture
  handlers directly) and the engine's `tests/conformance.rs` (Rust, calling
  `fixture::handler`) load every file here and assert their runner's answer
  equals `expected`.
- `transfer/*.json` — `import_graph` and `export_graph` (descriptor revision
  `legacy-v2`), in the same shape, read by the same two tests. The
  fixture `import_graph` checks the document and stores nothing, and says so;
  `graph_document` stands for what the host read from the toolkit's bucket.

These are a curated subset — the four categories the standalone-stack package
asked for (sources, graph, stats, one representative slice of retrieval) — not
every one of the ~28 tools either runner serves. Both runners implement the
rest from the same `spi/graph.json` data; only these calls are pinned as
golden files.

## Regenerating

There is nothing to regenerate mechanically here (no generator, unlike
`descriptor/`): the graph is small enough that `spi/graph.json` and the
`ingestion/`/`retrieval` goldens were computed by hand from the relations
above and checked into both languages' tests. Extending the graph means
updating `spi/graph.json`, the packaged Python copy, `fixture.go`'s Go
constants, and recomputing any golden answer whose input set changed.
