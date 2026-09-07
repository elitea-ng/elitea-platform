# Inventory conformance fixtures

`descriptor/` and `invoke/` predate this note (see `descriptor/README.md`).
`spi/`, `ingestion/` and `retrieval/` are the canned knowledge graph BOTH
fixture runners replay — the two-runner-drift trap
`support-inventory-wiki-survey.md` (INV-2) warns about, made mechanical.

## The two runners

| Runner | Where | Stack that runs it |
|---|---|---|
| Go | `services/elitea-subapp-host/internal/apps/inventory/run/fixture.go` | the E2E stack (`ELITEA_INVENTORY_RUNNER=fixture` on the Go host — no engine sidecar at all) |
| Python | `services/elitea-inventory/src/elitea_inventory/fixture_graph.py` (used by `fixture_runner.py`) | the standalone-full stack's engine sidecar over the Unix socket |

Both answer from the SAME graph: six entities (two source toolkits — `code`,
`docs` — three types, two layers) and five relations. `code:payment-client` is
the only entity with more than one inbound edge, on purpose: it is what
`impact_analysis` and `get_related_entities` have something to say about.

## The files

- `spi/graph.json` — the graph itself: `entities`, `relations`, `presets`.
  The Go side's copy is the `FixtureEntities`/`FixtureRelations`/
  `FixturePresets` constants in `fixture.go`; a Go test in that package
  (`fixture_parity_test.go`) asserts they equal this file byte-for-shape.
  The Python side's copy is packaged at
  `services/elitea-inventory/src/elitea_inventory/fixtures/inventory/spi/graph.json`
  (wheels do not read outside the package at runtime); a Python test
  (`tests/unit/test_fixture_graph.py`) asserts the packaged copy equals this
  one. **Edit this file, then re-copy it to the packaged path and re-run both
  tests** — editing only one side's copy is exactly the drift this exists to
  catch.
- `ingestion/*.json` and `retrieval/*.json` — one file per representative tool
  call: `{"tool", "params", "expected"}`. `expected` is the tool's JSON
  document (the `output_format=json` answer), not its markdown rendering —
  the shape is what a browser journey and the parity tests assert on, not the
  prose. Both `fixture_parity_test.go` (Go, calling the unexported fixture
  handlers directly) and `tests/unit/test_fixture_parity.py` (Python, calling
  `elitea_inventory.fixture_graph`) load every file here and assert their
  runner's answer equals `expected`.

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
