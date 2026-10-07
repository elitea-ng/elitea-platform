"""Regenerate graph.golden.json from ops.json with the Python engine's own graph.

Run from the repository root with networkx installed (nothing else of the
engine is imported):

    uv run --python 3.13 --with "networkx>=3.5,<4" \\
        python services/elitea-inventory-engine/tests/fixtures/graph_store/generate.py

(networkx 3.5 added the `edges=` keyword dump_to_json passes.)
"""

import importlib
import json
import os
import sys
import types

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = os.path.join(HERE, "../../../../elitea-inventory/src/elitea_inventory")

# Load knowledge_graph.py without the package __init__ chain (langchain & co).
for name, path in [
    ("elitea_inventory", SRC),
    ("elitea_inventory.engine", SRC + "/engine"),
    ("elitea_inventory.engine.inventory", SRC + "/engine/inventory"),
]:
    module = types.ModuleType(name)
    module.__path__ = [path]
    sys.modules[name] = module
kg_module = importlib.import_module("elitea_inventory.engine.inventory.knowledge_graph")

with open(os.path.join(HERE, "ops.json"), encoding="utf-8") as handle:
    operations = json.load(handle)["operations"]

graph = kg_module.KnowledgeGraph()
for op in operations:
    kind = op["op"]
    if kind == "entity":
        normalized = kg_module._normalize_entity_type(op["type"])
        assert normalized == op["type"], f"{op['type']!r} is not canonical ({normalized!r})"
        properties = op.get("properties")
        if properties and properties.get("long") == "LONG_STRING":
            properties = dict(properties, long="x" * 1000)
        citation = op.get("citation")
        graph.add_entity(
            op["id"],
            op["name"],
            op["type"],
            kg_module.Citation.from_dict(citation) if citation else None,
            properties,
        )
    elif kind == "relation":
        graph.add_relation(op["source"], op["target"], op["type"], op.get("properties"))
    elif kind == "embedding":
        graph._graph.nodes[op["id"]]["embedding"] = op["vector"]
    elif kind == "metadata":
        graph._metadata[op["key"]] = op["value"]
    elif kind == "communities":
        graph.set_community_data(op["value"])
    else:
        raise ValueError(kind)

# The indices of a live graph go stale: a merged citation never reaches
# _file_index, and a `name` property renames the node but not its
# _entity_index entry. The Rust store derives every index from the rows, which
# is what _rebuild_indices computes, so the golden is dumped after it.
graph._rebuild_indices()
graph.dump_to_json(os.path.join(HERE, "graph.golden.json"))
