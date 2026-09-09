"""#872: the pinned, checksum-verified `deno` install script.

No network access in these tests — `_download_verified`'s `urlopen` call is
always monkeypatched. What is asserted is exactly the thing this script exists
to guarantee: a digest mismatch or an unexpected archive shape is fatal and
extracts nothing, and a verified archive with the expected single member is
extracted with the right name and permissions.
"""

from __future__ import annotations

import hashlib
import importlib.util
import io
import os
import stat
import zipfile
from pathlib import Path
from typing import Any

import pytest


_SCRIPT = (
    Path(__file__).resolve().parents[2] / "scripts" / "install_pinned_deno.py"
)
_SPEC = importlib.util.spec_from_file_location("install_pinned_deno", _SCRIPT)
assert _SPEC is not None and _SPEC.loader is not None
_MODULE = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(_MODULE)


def _zip_bytes(members: dict[str, bytes]) -> bytes:
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, "w") as bundle:
        for name, content in members.items():
            bundle.writestr(name, content)
    return buffer.getvalue()


class _FakeResponse:
    def __init__(self, data: bytes) -> None:
        self._data = data

    def read(self) -> bytes:
        return self._data

    def __enter__(self) -> "_FakeResponse":
        return self

    def __exit__(self, *exc_info: object) -> None:
        return None


def test_target_arch_must_be_one_of_the_two_pinned_platforms() -> None:
    # docker-bake.hcl's `elitea-worker-python` target builds linux/amd64 and
    # linux/arm64 only — see the constant's own comment. A third value must
    # fail rather than silently fall back to an unverified download.
    with pytest.raises(SystemExit, match="no pinned deno archive digest"):
        _MODULE._download_verified("ppc64le")


def test_a_digest_mismatch_is_fatal_and_extracts_nothing(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    archive = _zip_bytes({"deno": b"not the real binary"})
    monkeypatch.setattr(
        _MODULE.urllib.request, "urlopen", lambda *_a, **_kw: _FakeResponse(archive)
    )

    with pytest.raises(SystemExit, match="digest mismatch"):
        _MODULE._download_verified("amd64")


def test_a_verified_archive_downloads(monkeypatch: pytest.MonkeyPatch) -> None:
    archive = _zip_bytes({"deno": b"pretend deno binary bytes"})
    digest = hashlib.sha256(archive).hexdigest()
    monkeypatch.setitem(
        _MODULE._ARCHIVE_SHA256, "amd64", ("x86_64-unknown-linux-gnu", digest)
    )
    monkeypatch.setattr(
        _MODULE.urllib.request, "urlopen", lambda *_a, **_kw: _FakeResponse(archive)
    )

    assert _MODULE._download_verified("amd64") == archive


def test_extract_binary_writes_the_single_member(tmp_path: Any) -> None:
    archive = _zip_bytes({"deno": b"pretend deno binary bytes"})
    dest = tmp_path / "deno"

    _MODULE._extract_binary(archive, str(dest))

    assert dest.read_bytes() == b"pretend deno binary bytes"


@pytest.mark.parametrize(
    "members",
    [
        {"deno": b"x", "extra-file": b"y"},
        {"not-deno": b"x"},
        {},
    ],
)
def test_extract_binary_refuses_an_unexpected_archive_shape(
    members: dict[str, bytes], tmp_path: Any
) -> None:
    archive = _zip_bytes(members)
    dest = tmp_path / "deno"

    with pytest.raises(SystemExit, match="expected exactly"):
        _MODULE._extract_binary(archive, str(dest))

    assert not dest.exists()


def test_main_downloads_verifies_extracts_and_sets_execute_permissions(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Any
) -> None:
    archive = _zip_bytes({"deno": b"pretend deno binary bytes"})
    digest = hashlib.sha256(archive).hexdigest()
    monkeypatch.setitem(
        _MODULE._ARCHIVE_SHA256, "amd64", ("x86_64-unknown-linux-gnu", digest)
    )
    monkeypatch.setattr(
        _MODULE.urllib.request, "urlopen", lambda *_a, **_kw: _FakeResponse(archive)
    )
    dest = tmp_path / "deno"

    exit_code = _MODULE.main(["--target-arch", "amd64", "--dest", str(dest)])

    assert exit_code == 0
    assert dest.read_bytes() == b"pretend deno binary bytes"
    mode = os.stat(dest).st_mode
    assert mode & stat.S_IXUSR
    assert mode & stat.S_IXGRP
    assert mode & stat.S_IXOTH
