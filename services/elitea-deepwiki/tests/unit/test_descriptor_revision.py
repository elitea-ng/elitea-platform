"""The legacy-v1 descriptor is generated, so the generator is the gate.

Without this, `tools/build_descriptor_v1.py` would be a helper someone
remembered to run — and the served copy under services/elitea-subapp-host
could drift from the fixture the host's own byte pin compares against, with
both files looking plausible.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

SERVICE_ROOT = Path(__file__).resolve().parents[2]
REPO_ROOT = SERVICE_ROOT.parents[1]
TOOL = SERVICE_ROOT / "tools" / "build_descriptor_v1.py"
V1 = (
    REPO_ROOT
    / "conformance"
    / "provider"
    / "fixtures"
    / "deepwiki"
    / "descriptor"
    / "legacy-v1"
    / "provider_descriptor.json"
)


def test_the_generator_agrees_with_both_committed_copies() -> None:
    result = subprocess.run(
        [sys.executable, str(TOOL), "--check"], capture_output=True, text=True
    )
    assert result.returncode == 0, result.stdout + result.stderr


def test_ask_and_deep_research_declare_the_attachment_arguments() -> None:
    """The point of the revision, asserted against the document.

    The wrapper refuses a selection whose version pin is missing; the
    descriptor is what tells a caller the pin exists at all.
    """
    document = json.loads(V1.read_text(encoding="utf-8"))
    declaring = 0
    for toolkit in document["provided_toolkits"]:
        for tool in toolkit["provided_tools"]:
            if tool["name"] not in ("ask", "deep_research"):
                continue
            declaring += 1
            schema = tool["args_schema"]
            for name in ("chat_history", "context_paths", "context_wiki_version_id"):
                assert name in schema, f"{tool['name']} does not declare {name}"
    assert declaring == 4, declaring
