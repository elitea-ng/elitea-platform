# Inventory provider descriptor revisions

Three revisions live here. All are kept, and they answer different questions.
The host serves `legacy-v2`.

## `legacy-v0` — what the legacy plugin declared

Recorded from `legacy/plugins/inventory_plugin/methods/descriptor.py`. It is the
record of what the Pylon plugin actually advertised, and it is what a **parity**
question is asked against: "did the port drop anything the legacy product
declared?" is only answerable against this file.

It is not what the host serves. Nothing should be added to it.

## `legacy-v1` — four undeclared tools declared (ADR-0023 H4c stage I3)

`legacy-v0` **plus four tools**, and nothing else. The host served it until
`legacy-v2`.

| tool | who calls it |
|---|---|
| `get_entity_neighbors` | the graph view's "expand connections" context menu (1–3 hops) |
| `get_entities_by_ids` | the chat view's highlighting of the entities a response drew on |
| `get_ingestion_status` | the sources view's run-in-flight indicator |
| `smart_normalize_types` | the LLM type normaliser, re-run on an existing graph |

### Why they were added

All four are **implemented** in the legacy plugin (`methods/invoke.py` has a
`_tool_*` handler for each), **routed** by it (`_handle_inventory_tool`'s
dispatch dict names all four), and **called** by the legacy UI — and none was
ever declared in its descriptor.

That worked on the legacy platform because the UI called the plugin's own HTTP
routes directly, bypassing the descriptor entirely. Under ADR-0022/0023 the
facade admits a tool only if the descriptor advertises it, so an undeclared tool
is an unreachable one. Porting the provider without declaring them would have
been a port that silently dropped three features of the product — and dropped
them in a way no test could see, because nothing that exists today calls them
through the descriptor.

`legacy-v0` records the omission; `legacy-v1` corrects it.

### What did NOT change

Everything else: both toolkit configs byte for byte, the `inventory_search`
family untouched, every existing tool's `args_schema`, description and
`sync_invocation_supported` unchanged, and no tool removed. A conformance case
(`legacy-v1 adds four tools to legacy-v0 and changes nothing else`) diffs the
two revisions and fails on any other difference — a 37 KB document edited by a
generator cannot be reviewed for what it did *not* change.

The five tools `legacy-v0` declares and the legacy router **never** carried —
`get_type_stats`, `link_toolkits_to_tools`, `connect_orphan_nodes`,
`validate_relationships`, and `query_graph` on the `inventory` family — are
still declared in `legacy-v1` and `legacy-v2`, and the runner refuses them by name
(`internal/apps/inventory/run`.`DeferredTools`). Removing them from the
descriptor would tell a caller the tool does not exist; what is true is that it
is declared and has never been implemented on any platform.

### Regenerating

`legacy-v1` was generated from `legacy-v0` by
`services/elitea-inventory/tools/build_descriptor_v1.py`, deleted with the
Python service (`services/elitea-inventory-engine/tests/fixtures/PROVENANCE.md`
names the commit). It is frozen now.

`legacy-v2` is generated from `legacy-v1`, and the generator writes both the
fixture and the host's embedded copy:

```
python conformance/provider/tools/build_inventory_descriptor_v2.py          # rewrite both
python conformance/provider/tools/build_inventory_descriptor_v2.py --check  # verify both
```

`conformance/provider/tests/test_inventory_descriptor_revision.py` runs the
check.

> **Note when checking a fixture change locally:** `go test` caches a package
> result across edits to these JSON files, so a fixture-only change can re-run
> against a cached PASS. Use `go test -count=1` when the only thing you changed
> is a fixture.

## `legacy-v2` — graph transfer tools (what the host serves)

`legacy-v1` plus two tools on the `inventory` family, and one corrected
description. The Go host's embedded `internal/apps/inventory/descriptor.json`
is a byte-for-byte copy, pinned by `internal/spi/conformance_inventory_test.go`,
which also diffs v1 against v2 and fails on any other change.

| tool | what it does |
|---|---|
| `import_graph` | reads `artifact_name` (default `graph.json`) from the toolkit's bucket — the Python engine's graph — and imports it into the native graph store; `replace_ingestion_state` mirrors the CLI flag |
| `export_graph` | writes the stored graph to the toolkit's bucket as `graph.json` |

They are the native engine's `import-graph` / `export-graph` commands as tools,
so a toolkit owner migrates a graph without cluster access. The host reads the
import document (it holds the bucket transport) and writes it into the
host-owned `graph_document` parameter; a caller's value is replaced. The
document is bounded at 32 MiB; a larger graph uses the CLI.

`smart_normalize_types` claimed it "runs automatically after a successful
ingestion". No engine did. Its v2 description says so.
