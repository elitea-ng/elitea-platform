"""The Inventory legacy-v2 descriptor is generated, so the generator is the gate.

The host serves a byte copy of legacy-v2 (services/elitea-subapp-host/
internal/apps/inventory/descriptor.json). Without this check, the generator
is a helper someone remembers to run, and the two copies can drift.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

PACKAGE_ROOT = Path(__file__).resolve().parents[1]
TOOL = PACKAGE_ROOT / "tools" / "build_inventory_descriptor_v2.py"
DESCRIPTORS = PACKAGE_ROOT / "fixtures" / "inventory" / "descriptor"


def _tools(revision: str) -> dict[str, list[dict]]:
    document = json.loads(
        (DESCRIPTORS / revision / "provider_descriptor.json").read_text(encoding="utf-8")
    )
    return {
        toolkit["name"]: toolkit["provided_tools"]
        for toolkit in document["provided_toolkits"]
    }


def test_the_generator_agrees_with_both_committed_copies() -> None:
    result = subprocess.run(
        [sys.executable, str(TOOL), "--check"], capture_output=True, text=True
    )
    assert result.returncode == 0, result.stdout + result.stderr


def test_v2_declares_the_graph_transfer_tools_on_the_inventory_family_only() -> None:
    v1, v2 = _tools("legacy-v1"), _tools("legacy-v2")
    names = [tool["name"] for tool in v2["inventory"]]
    assert names == [tool["name"] for tool in v1["inventory"]][: names.index("import_graph")] + [
        "import_graph",
        "export_graph",
    ] + [tool["name"] for tool in v1["inventory"]][names.index("import_graph") :]
    assert v2["inventory_search"] == v1["inventory_search"]
    schema = {tool["name"]: tool["args_schema"] for tool in v2["inventory"]}
    assert schema["import_graph"]["artifact_name"]["default"] == "graph.json"
    assert schema["import_graph"]["replace_ingestion_state"]["default"] is False


def test_smart_normalize_types_no_longer_claims_to_run_after_ingestion() -> None:
    (tool,) = [t for t in _tools("legacy-v2")["inventory"] if t["name"] == "smart_normalize_types"]
    assert "automatically" not in tool["description"]
