# Provenance of the frozen goldens

Every file below was produced by the real Python Inventory engine
(`services/elitea-inventory`) at commit `a1d38fb4d` (origin/main immediately
before the Python service was removed). The Python service and the generator
scripts were then deleted; the JSON is now frozen reference data that the Rust
engine's tests compare against. Do not edit it by hand.

Five of them moved since, with the code that reads them, to
`libs/rust/inventory-core` (ADR-0029 decision 7); the table gives where each
file is now (prefixed `inventory-core:`) and where its generator was.

| Golden file(s) | Generator (path inside `services/elitea-inventory-engine/` at `a1d38fb4d`) |
|---|---|
| `inventory-core:assets/python_inventory.json` | `assets/generate.py` (prompts, taxonomies, type tables) |
| `inventory-core:assets/python_retrieval.json` | `assets/generate_retrieval.py` (retrieval-tool tables) |
| `assets/source_tools.json` | `assets/source_tools.py` (still present; reads `inventory-core:assets/python_inventory.json` and elitea-main's toolkit schema snapshot, not Python source) |
| `tests/fixtures/communities/{two_clusters,centrality}.expected.json` | `tests/fixtures/communities/generate.py` (real igraph + networkx over `two_clusters.json` / `centrality.json`) |
| `tests/fixtures/graph_store/graph.golden.json` | `tests/fixtures/graph_store/generate.py` (replays `ops.json` through the Python `KnowledgeGraph`) |
| `tests/fixtures/ingest/entity_ids.json` | `tests/fixtures/ingest/generate.py` (Python `_generate_entity_id`) |
| `inventory-core:tests/fixtures/retrieval/{graph,goldens}.json` | `tests/fixtures/retrieval/generate.py` |
| `inventory-core:tests/fixtures/retrieval_more/{graph,bare,goldens}.json` | `tests/fixtures/retrieval_more/generate.py` |
| `tests/fixtures/smart_normalize/{graph,goldens}.json` | `tests/fixtures/smart_normalize/generate.py` |
| `tests/fixtures/code_like/goldens.json` | `tests/fixtures/code_like/generate.py` |

## Regenerating (only if ever needed)

```sh
git worktree add ../inventory-a1d38fb4d a1d38fb4d
cd ../inventory-a1d38fb4d
python3 services/elitea-inventory-engine/<generator>   # path from the table
```

(`git show a1d38fb4d:services/elitea-inventory-engine/<generator>` prints a
generator without a worktree, but it locates the Python source relative to its
own path, so run it inside the worktree.)

The generators need the Python service's dependencies (`services/elitea-inventory`
requirements at that commit, including igraph/networkx for the communities
goldens). Copy the regenerated JSON back over the frozen files and review the
diff: a changed golden means the Rust port must change or the difference must be
documented as a deliberate deviation.

Deliberate deviation without a golden change: `graph.golden.json` has no
relation two sources contributed, so the `provenance` edge attribute (the
Rust graph's list of an overlapping relation's `{source_toolkit,
discovered_in_file}` contributions; see `src/graph.rs`) never appears in it.
Python had no such attribute: its edge kept the last writer's
`source_toolkit` only.

Also without a golden: `full_rebuild` rebuilds only the source it names
(Python deleted the whole graph, every other source's data with it).
