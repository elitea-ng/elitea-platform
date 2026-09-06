"""Report which SDK toolkits THIS worker image can actually build.

The image installs ``elitea-worker-python[agent-current,indexing-current]``, a
measured subset of ``elitea-sdk[all]``. A toolkit whose third-party dependency
that subset omits does not import: ``elitea_sdk.tools`` catches the failure,
records it under ``FAILED_IMPORTS[key]`` and continues. The registry then holds
52 toolkit TYPES while the process can build fewer.

Nothing told Main about that difference. Main serves a toolkit type catalogue
from a build-time SDK snapshot, so a type whose import failed was offered as a
tile, saved, attached to an agent, and failed at the first tool call — the one
place the user cannot act on it.

This module is the answer the worker gives. It reports the import keys that
failed in the running interpreter. ``sync_worker_toolkit_capability_snapshot``
records the result for the admitted image, and Main joins it with the toolkit
catalogue's ``import_key`` to decide whether a type is offered as creatable.

The KEY is not the type. ``elitea_sdk.tools`` registers the Kubernetes toolkit
under ``k8s`` and publishes it as the type ``kubernetes``. Report keys here,
because a key is what a failure is recorded under, and let the catalogue's
type-to-key mapping do the join.
"""

from __future__ import annotations

import json
from typing import Any

SCHEMA_VERSION = "elitea.worker-toolkit-capability.v1"

# ``inventory`` is an elitea-sdk community toolkit that this repository ships as
# a separate service (services/elitea-inventory). It is absent from the built-in
# registry and from the toolkit catalogue, so its import failure is expected and
# says nothing about the image. Every OTHER failure is a real capability gap.
EXPECTED_ABSENT_IMPORT_KEYS = ("inventory",)


def unsupported_import_keys() -> tuple[str, ...]:
    """Return the SDK import keys that failed in this interpreter, sorted.

    The import is performed here rather than at module import time so that a
    caller which only wants the schema version does not pay for the SDK.
    """

    import elitea_sdk.tools as sdk_tools

    failed = getattr(sdk_tools, "FAILED_IMPORTS", None)
    if not isinstance(failed, dict):
        # A registry without the attribute cannot be measured. Report nothing
        # rather than an empty set that would read as "everything works".
        raise RuntimeError("elitea_sdk.tools does not report FAILED_IMPORTS")
    return tuple(
        sorted(key for key in failed if key not in EXPECTED_ABSENT_IMPORT_KEYS)
    )


def toolkit_capability_document(sdk_revision: str) -> dict[str, Any]:
    """Build the document Main embeds for the Python worker profile."""

    if not sdk_revision:
        raise ValueError("sdk_revision is required")
    return {
        "schema_version": SCHEMA_VERSION,
        "implementation": "python",
        "sdk_revision": sdk_revision,
        "unsupported_import_keys": list(unsupported_import_keys()),
    }


def toolkit_capability_json(sdk_revision: str) -> str:
    return json.dumps(
        toolkit_capability_document(sdk_revision),
        ensure_ascii=False,
        separators=(",", ":"),
        sort_keys=True,
    )
