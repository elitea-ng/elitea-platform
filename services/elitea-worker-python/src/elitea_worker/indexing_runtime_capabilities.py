"""Fail-fast checks for the admitted current-baseline indexing runtime."""

from __future__ import annotations

import hashlib
import importlib
import importlib.metadata
import json
import shutil
import sys
from collections.abc import Callable
from contextlib import redirect_stdout
from ctypes.util import find_library
from pathlib import Path
from typing import Any

from elitea_worker.agents.sdk_adapter import verify_sdk_markdown_runtime
from elitea_worker.execution.errors import DependencyUnavailable


_PROFILE_SCHEMA = "elitea.indexing-runtime-capability-profile.v1"
_OCR_PROBE_TEXT = "ELITEA OCR 5681"
_MARKDOWN_PROBE_TEXT = "ELITEA MARKDOWN INDEXING RUNTIME"


def require_indexing_runtime_capabilities(
    *,
    lock_path: Path | None = None,
    distribution_version: Callable[[str], str] = importlib.metadata.version,
    import_module: Callable[[str], Any] = importlib.import_module,
    find_executable: Callable[[str], str | None] = shutil.which,
    find_shared_library: Callable[[str], str | None] = find_library,
    package_tree_digest: Callable[[Path], str] | None = None,
    ocr_probe: Callable[[], None] | None = None,
    markdown_probe: Callable[[], None] | None = None,
) -> str:
    """Verify the complete image-local indexing profile before opening Redis.

    The public failure is deliberately stable and secret-free. The chained
    in-process cause contains only admitted dependency identifiers and exception
    class names so an operator can diagnose an image build without exposing
    runtime configuration, credentials, endpoints or filesystem paths.
    """

    try:
        lock = _load_lock(lock_path or _default_lock_path())
        profile = lock["indexing_capability_profile"]
        if profile.get("schema_revision") != _PROFILE_SCHEMA:
            raise ValueError("profile-schema")
        expected_digest = profile.get("profile_sha256")
        if not isinstance(expected_digest, str) or (
            _profile_digest(profile) != expected_digest
        ):
            raise ValueError("profile-digest")

        failures: list[str] = []
        for distribution, expected in profile["verified_distributions"].items():
            try:
                actual = distribution_version(distribution)
            except Exception as exc:  # pragma: no branch - one stable failure path
                failures.append(
                    f"distribution:{distribution}:{type(exc).__name__}"
                )
                continue
            if actual != expected:
                failures.append(
                    f"distribution:{distribution}:version-mismatch"
                )

        sdk_module: Any | None = None
        sdk_tool_registry: Any | None = None
        for module_name in profile["required_imports"]:
            try:
                # The SDK's optional-tool discovery prints diagnostics. Keep
                # stdout reserved for worker protocol/CLI output.
                with redirect_stdout(sys.stderr):
                    module = import_module(module_name)
                if module_name == "elitea_sdk.runtime.clients.client":
                    sdk_module = import_module("elitea_sdk")
                if module_name == "elitea_sdk.tools":
                    sdk_tool_registry = module
            except Exception as exc:
                failures.append(f"import:{module_name}:{type(exc).__name__}")

        required_families = profile.get("required_indexing_families")
        family_imports = profile.get("required_indexing_family_imports")
        if (
            not isinstance(required_families, list)
            or not required_families
            or not all(isinstance(key, str) and key for key in required_families)
            or not isinstance(family_imports, dict)
            or set(required_families) != set(family_imports)
        ):
            failures.append("sdk-indexing-family-contract:invalid")
        else:
            for family in required_families:
                module_name = family_imports.get(family)
                if not isinstance(module_name, str) or not module_name:
                    failures.append(
                        f"sdk-indexing-family:{family}:invalid-import"
                    )
                    continue
                try:
                    with redirect_stdout(sys.stderr):
                        import_module(module_name)
                except Exception as exc:
                    failures.append(
                        f"sdk-indexing-family:{family}:{type(exc).__name__}"
                    )

        required_sdk_keys = profile.get("required_sdk_tool_import_keys")
        if (
            not isinstance(required_sdk_keys, list)
            or not required_sdk_keys
            or not all(isinstance(key, str) and key for key in required_sdk_keys)
        ):
            failures.append("sdk-tool-import-key-contract:invalid")
        elif sdk_tool_registry is None:
            failures.append("sdk-indexing-toolkit-registry:missing")
        else:
            failed_imports = getattr(sdk_tool_registry, "FAILED_IMPORTS", None)
            if not isinstance(failed_imports, dict):
                failures.append("sdk-indexing-toolkit-failures:missing")
            else:
                for key in sorted(
                    set(required_sdk_keys).intersection(failed_imports)
                ):
                    # Never include the SDK's exception text: it may contain
                    # endpoints or other deployment-specific values.
                    failures.append(f"sdk-indexing-toolkit:{key}:failed-import")

        if sdk_module is not None:
            package_file = getattr(sdk_module, "__file__", None)
            if package_file is None:
                failures.append("sdk-package-tree:missing")
            else:
                digest_package_tree = package_tree_digest or _package_tree_digest
                actual_tree = digest_package_tree(Path(package_file).resolve().parent)
                expected_tree = lock["installed_package_tree"]["sha256"]
                if actual_tree != expected_tree:
                    failures.append("sdk-package-tree:digest-mismatch")

        for executable in profile["required_executables"]:
            if find_executable(executable) is None:
                failures.append(f"executable:{executable}:missing")
        for library in profile["required_shared_libraries"]:
            if find_shared_library(library) is None:
                failures.append(f"shared-library:{library}:missing")
        try:
            (ocr_probe or _verify_ocr_runtime)()
        except Exception as exc:
            failures.append(f"ocr-runtime:{type(exc).__name__}:{exc}")
        try:
            (markdown_probe or _verify_markdown_runtime)()
        except Exception as exc:
            failures.append(f"markdown-runtime:{type(exc).__name__}")

        if failures:
            raise RuntimeError(",".join(failures))
        return expected_digest
    except DependencyUnavailable:
        raise
    except Exception as exc:
        raise DependencyUnavailable(
            "The indexing runtime capability profile is incomplete."
        ) from exc


def _default_lock_path() -> Path:
    source_lock = Path(__file__).resolve().parents[2] / "elitea-sdk.lock.json"
    if source_lock.is_file():
        return source_lock
    return Path(__file__).resolve().with_name("elitea-sdk.lock.json")


def _load_lock(path: Path) -> dict[str, Any]:
    document = json.loads(path.read_bytes())
    if not isinstance(document, dict):
        raise ValueError("lock-shape")
    return document


def _profile_digest(profile: dict[str, Any]) -> str:
    admitted = dict(profile)
    admitted.pop("profile_sha256", None)
    encoded = json.dumps(
        admitted,
        allow_nan=False,
        ensure_ascii=False,
        separators=(",", ":"),
        sort_keys=True,
    ).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def _package_tree_digest(root: Path) -> str:
    digest = hashlib.sha256()
    for path in sorted(root.rglob("*.py")):
        relative = path.relative_to(root).as_posix().encode("utf-8")
        content = path.read_bytes()
        digest.update(len(relative).to_bytes(4, "big"))
        digest.update(relative)
        digest.update(len(content).to_bytes(8, "big"))
        digest.update(content)
    return digest.hexdigest()


def _verify_ocr_runtime() -> None:
    """Exercise the exact Python wrapper and image-local Tesseract executable."""

    import pytesseract
    from PIL import Image, ImageDraw, ImageFont

    image = Image.new("L", (1_000, 150), 255)
    font = ImageFont.truetype(
        "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
        48,
    )
    ImageDraw.Draw(image).text((30, 35), _OCR_PROBE_TEXT, font=font, fill=0)
    observed = " ".join(
        pytesseract.image_to_string(
            image,
            config="--psm 7",
            # pytesseract raises RuntimeError("Tesseract process timeout") when
            # the executable does not answer in time. A CI builder running the
            # image matrix in parallel took longer than 10 s once (run
            # 34137688006), and the failure read as "ocr-runtime:RuntimeError"
            # — the same label a wrong reading gets. The bound is generous now,
            # and the two failures are named apart below.
            timeout=60,
        ).split()
    )
    if observed != _OCR_PROBE_TEXT:
        raise RuntimeError(f"ocr-output-mismatch:{observed!r}")


def _verify_markdown_runtime() -> None:
    """Exercise the shared SDK Markdown parser used by indexing families."""

    verify_sdk_markdown_runtime(_MARKDOWN_PROBE_TEXT)


__all__ = ["require_indexing_runtime_capabilities"]
