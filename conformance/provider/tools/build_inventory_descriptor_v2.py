#!/usr/bin/env python3
"""Derive Inventory descriptor revision legacy-v2 from legacy-v1.

TWO TOOLS AND ONE DESCRIPTION. The `inventory` family gains:

    import_graph   move a toolkit's graph.json (the Python engine's graph,
                   kept in the toolkit's artifact bucket) into the native
                   engine's graph store
    export_graph   write the stored graph back to that bucket as graph.json

Both were operator CLI commands of the Rust engine (`import-graph`,
`export-graph`, issue #1129). The owner decision is that a toolkit owner
reaches them without kubectl, and the host admits a tool only when the
descriptor advertises it, so they are declared here.

`smart_normalize_types` keeps its arguments; its description claimed that
it "runs automatically after a successful ingestion", which no engine has
ever done. The new text says what is true.

Generated rather than hand-edited, so the diff against legacy-v1 is exactly
these changes, plus one optional toolkit parameter, `reasoning_effort`. The host's conformance test diffs the two revisions and fails
on any other difference.

    python tools/build_inventory_descriptor_v2.py            rewrite v2 and the served copy
    python tools/build_inventory_descriptor_v2.py --check    verify both are current
"""

from __future__ import annotations

import argparse
import collections
import json
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]
FIXTURES = REPO_ROOT / "conformance" / "provider" / "fixtures" / "inventory" / "descriptor"
V1 = FIXTURES / "legacy-v1" / "provider_descriptor.json"
V2 = FIXTURES / "legacy-v2" / "provider_descriptor.json"

#: The host serves this document: a byte copy, pinned by the host's
#: conformance test (internal/spi/conformance_inventory_test.go).
SERVED = (
    REPO_ROOT
    / "services"
    / "elitea-subapp-host"
    / "internal"
    / "apps"
    / "inventory"
    / "descriptor.json"
)

SMART_NORMALIZE_DESCRIPTION = (
    "Use the configured LLM to map uncommon entity types onto the canonical set, "
    "on the graph as it is stored now. No engine runs it after an ingestion; "
    "run it when the graph has stray types."
)


def _od(pairs):
    return collections.OrderedDict(pairs)


def _arg(type_, required, description, default=...):
    pairs = [("type", type_), ("required", required), ("description", description)]
    if default is not ...:
        pairs.append(("default", default))
    return _od(pairs)


def _tool(name, description, args):
    return _od(
        [
            ("name", name),
            ("description", description),
            ("args_schema", _od(args)),
            ("tool_result_type", "String"),
            ("sync_invocation_supported", True),
            ("async_invocation_supported", True),
        ]
    )


NEW_TOOLS = [
    _tool(
        "import_graph",
        "Import a graph.json document (the node-link graph the Python engine kept in "
        "this toolkit's artifact bucket) into the graph store, replacing the stored "
        "graph. Re-running it replaces the graph again. Refused while an ingestion "
        "runs, and refused over native ingestion state unless replace_ingestion_state "
        "is set.",
        [
            (
                "artifact_name",
                _arg(
                    "String",
                    False,
                    "Name of the graph document in this toolkit's artifact bucket",
                    "graph.json",
                ),
            ),
            (
                "replace_ingestion_state",
                _arg(
                    "Boolean",
                    False,
                    "Delete the toolkit's native ingestion state (source status, "
                    "document versions) with the old graph",
                    False,
                ),
            ),
            ("output_format", _arg("String", False, "'json' or 'text'", "text")),
        ],
    ),
    _tool(
        "export_graph",
        "Write the stored graph to this toolkit's artifact bucket as graph.json "
        "(the Python engine's node-link layout), replacing the object there.",
        [("output_format", _arg("String", False, "'json' or 'text'", "text"))],
    ),
]

REASONING_EFFORT_DESCRIPTION = (
    "Reasoning effort for model calls: none, low, medium or high (empty keeps the "
    "model's default). Use none for reasoning models such as Qwen3 to avoid long "
    "thinking. A caller-supplied llm_settings.reasoning_effort overrides it."
)

#: The new tools go after the last maintenance tool, in this order.
INSERT_AFTER = "smart_normalize_types"


def build() -> str:
    document = json.loads(V1.read_text(encoding="utf-8"), object_pairs_hook=collections.OrderedDict)
    inserted = 0
    for toolkit in document["provided_toolkits"]:
        if toolkit["name"] != "inventory":
            continue
        expanded = []
        for tool in toolkit["provided_tools"]:
            if tool["name"] == INSERT_AFTER:
                tool["description"] = SMART_NORMALIZE_DESCRIPTION
                expanded.append(tool)
                expanded.extend(NEW_TOOLS)
                inserted += 1
                continue
            expanded.append(tool)
        toolkit["provided_tools"] = expanded
        config = toolkit["toolkit_config"]
        order = config["fields_order"]
        order.insert(order.index("embedding_model") + 1, "reasoning_effort")
        config["parameters"]["reasoning_effort"] = _arg(
            "String", False, REASONING_EFFORT_DESCRIPTION, ""
        )
    if inserted != 1:
        raise SystemExit(f"{INSERT_AFTER} found {inserted} times in legacy-v1; expected once")
    return json.dumps(document, indent=2, ensure_ascii=False) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    built = build()
    if args.check:
        failed = False
        for path in (V2, SERVED):
            if not path.exists() or path.read_text(encoding="utf-8") != built:
                print(f"{path} is not what this tool builds; re-run it", file=sys.stderr)
                failed = True
        if failed:
            return 1
        print("ok")
        return 0
    for path in (V2, SERVED):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(built, encoding="utf-8")
        print(f"wrote {path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
