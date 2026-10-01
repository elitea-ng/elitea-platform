from __future__ import annotations

import hashlib
import importlib.util
import json
from pathlib import Path
import sys

import pytest


_SCRIPT = (
    Path(__file__).resolve().parents[2]
    / "scripts"
    / "verify_locked_artifacts.py"
)
_SPEC = importlib.util.spec_from_file_location("verify_locked_artifacts", _SCRIPT)
assert _SPEC is not None and _SPEC.loader is not None
_MODULE = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(_MODULE)


def test_artifact_closure_requires_exact_requirement_record_parity() -> None:
    profile = {
        "artifact_verified_requirements": [
            "cryptography==48.0.1",
            "FigmaPy==2018.1.0",
        ],
        "verified_wheels": {"cryptography": {}},
        "verified_source_archives": {"FigmaPy": {}},
    }

    _MODULE._validate_artifact_closure(profile)

    profile["artifact_verified_requirements"].append("azure-core==1.38.0")
    with pytest.raises(
        SystemExit,
        match="artifact records do not match",
    ):
        _MODULE._validate_artifact_closure(profile)


@pytest.mark.parametrize(
    "requirements",
    [
        ["cryptography>=48.0.1"],
        ["cryptography==48.0.1", "cryptography==48.0.1"],
        ["cryptography==48.0.1", "Cryptography==48.0.1"],
        ["cryptography==48.0.1\n--extra-index-url https://example.invalid"],
        ["cryptography==48.0.1 --hash=sha256:unverified"],
    ],
)
def test_artifact_closure_rejects_non_exact_or_duplicate_pins(
    requirements: list[str],
) -> None:
    profile = {
        "artifact_verified_requirements": requirements,
        "verified_wheels": {"cryptography": {}},
        "verified_source_archives": {"FigmaPy": {}},
    }

    with pytest.raises(SystemExit):
        _MODULE._validate_artifact_closure(profile)


def test_constraints_export_uses_exact_artifact_lock_pins(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    lock_path = _SCRIPT.parents[1] / "elitea-sdk.lock.json"
    before = lock_path.read_bytes()
    requirements = json.loads(before)["indexing_capability_profile"][
        "artifact_verified_requirements"
    ]
    constraints = tmp_path / "constraints.txt"
    monkeypatch.setattr(
        sys, "argv", [str(_SCRIPT), str(lock_path), str(constraints), "constraints"]
    )

    assert _MODULE.main() == 0

    assert constraints.read_text().splitlines() == requirements
    assert "cryptography==50.0.1" in constraints.read_text().splitlines()
    assert lock_path.read_bytes() == before


def test_constraints_export_rejects_incomplete_artifact_closure(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    profile = {
        "artifact_verified_requirements": ["cryptography==50.0.1"],
        "verified_wheels": {"cryptography": {}},
        "verified_source_archives": {"FigmaPy": {}},
    }
    lock_path = tmp_path / "lock.json"
    lock_path.write_text(json.dumps({"indexing_capability_profile": profile}))
    constraints = tmp_path / "constraints.txt"
    monkeypatch.setattr(
        sys, "argv", [str(_SCRIPT), str(lock_path), str(constraints), "constraints"]
    )

    with pytest.raises(SystemExit, match="artifact records do not match"):
        _MODULE.main()

    assert not constraints.exists()


@pytest.mark.parametrize("contents", [None, b"changed artifact", b"locked artifact"])
def test_wheel_verification_retains_missing_and_digest_checks(
    contents: bytes | None,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    profile = {
        "artifact_verified_requirements": [
            "cryptography==50.0.1",
            "FigmaPy==2018.1.0",
        ],
        "verified_wheels": {
            "cryptography": {
                "filename": "cryptography.whl",
                "sha256": hashlib.sha256(b"locked artifact").hexdigest(),
            }
        },
        "verified_source_archives": {"FigmaPy": {}},
    }
    lock_path = tmp_path / "lock.json"
    lock_path.write_text(json.dumps({"indexing_capability_profile": profile}))
    if contents is not None:
        (tmp_path / "cryptography.whl").write_bytes(contents)
    monkeypatch.setattr(
        sys,
        "argv",
        [str(_SCRIPT), str(lock_path), str(tmp_path), "verified_wheels"],
    )

    if contents == b"locked artifact":
        assert _MODULE.main() == 0
    else:
        reason = "is missing" if contents is None else "digest mismatch"
        with pytest.raises(SystemExit, match=f"locked artifact {reason}"):
            _MODULE.main()
