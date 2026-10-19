from __future__ import annotations

import json
import re
from pathlib import Path

from .sdk_current import BASELINE_LOCK


def test_current_pylon_sdk_baseline_is_immutable_and_target_independent() -> None:
    baseline = json.loads(BASELINE_LOCK.read_text())
    assert baseline == {
        "schema_version": "elitea.current-pylon-sdk-baseline.v1",
        "distribution": "elitea-sdk",
        "distribution_version": "0.8.30",
        "source": {
            "repository": "https://github.com/EliteaAI/elitea-sdk.git",
            "revision": "48c51a16634a9924f6c5d5313c3bacedb0b5b56b",
            "git_archive_sha256": (
                "85e8b2396dc86ea8e7d2098a41e12b228fa1995a0be8ff0143af6d3df49b6d61"
            ),
        },
    }

    platform_root = Path(__file__).resolve().parents[5]
    target = json.loads(
        (platform_root / "services/elitea-llm-gateway/internal/sdkpin/elitea-sdk.lock.json").read_text()
    )
    assert target["source"]["revision"] != baseline["source"]["revision"]
    assert re.fullmatch(r"[0-9a-f]{40}", target["source"]["revision"])
