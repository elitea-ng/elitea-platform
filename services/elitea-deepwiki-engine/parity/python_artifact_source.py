#!/usr/bin/env python3
"""Write the artifact-folder source cases with the Python engine's answers.

    PYTHONPATH=services/elitea-deepwiki/src python parity/python_artifact_source.py \
        tests/fixtures/ingest/artifact_source.json

Everything comes from the Python code itself, not from a transcription:

* ``parse``: ``artifact_source.parse_artifact_source`` (the bucket, the
  prefix, the listing prefix, the cache-directory slug), or the error;
* ``listings``: raw listing pages as elitea-main's object route returns
  them, read through ``PlatformArtifactClient.list_artifacts`` (with
  ``requests.get`` answered from the pages), then ``collect_objects``,
  ``check_caps``, ``listing_digest`` and ``materialise_artifact_source``
  with a fake client (the directory name it chooses and the marker it
  writes beside it); and the generation's wiki id
  (``normalize_wiki_id(build_repo_identifier(...))``, as
  ``wiki_subprocess_worker`` derives it). That is the ONE id of an artifact
  folder: the Rust engine's direct ``ask`` and context paths derive it too.
  Python's own ``ask`` and ``wiki_context`` derived two others
  (``artifact--docs-handbook``, ``docs--handbook--main``); they were dropped
  on purpose and are not recorded;
* ``settings``: ``extract_artifact_settings`` over ``llm_settings`` blocks.

The Rust unit tests ``ingest::artifact::tests`` replay every case.
"""

from __future__ import annotations

import json
import os
import sys
import tempfile
from pathlib import Path
from unittest import mock

from elitea_deepwiki import artifact_source as source_mod
from elitea_deepwiki.engine import artifacts_platform_client as client_mod
from elitea_deepwiki.engine.registry_manager import normalize_wiki_id
from elitea_deepwiki.engine.repository_identity import (
    build_repo_identifier,
    canonical_repository_path,
)

PARSE = [
    "artifact://docs",
    "artifact://docs/handbook",
    "  ARTIFACT://Docs/handbook/v2/  ",
    "artifact://docs//",
    "artifact://my-bucket-1/a b/c.d_e",
    "artifact://x",
    "artifact://1docs",
    "artifact://docs/a/../b",
    "artifact://docs/a\\b",
    "artifact://docs/a//b",
    "artifact://docs/./b",
    "github.com/acme/notes",
    "artifact://Doc_s",
]


class _Response:
    def __init__(self, status: int, body):
        self.status_code = status
        self._body = body
        self.text = json.dumps(body)

    def json(self):
        return self._body


def _list_through_client(pages: list, bucket: str, prefix: str) -> tuple:
    """``list_artifacts`` with ``requests.get`` answered from ``pages``."""
    answers = iter(pages)
    calls = []

    def fake_get(url, headers=None, params=None, verify=None, timeout=None):
        calls.append(dict(params or {}))
        return _Response(200, next(answers))

    client = client_mod.PlatformArtifactClient(
        {"base_url": "https://platform.example", "api_key": "k", "project_id": "42"}
    )
    with mock.patch("requests.get", fake_get):
        return client.list_artifacts(bucket, prefix), calls


class _FakeClient:
    def __init__(self, listing):
        self.listing = listing

    def list_artifacts(self, bucket, prefix):
        return self.listing

    def download_artifact(self, bucket, key):
        return f"content of {key}\n".encode()


LISTINGS = [
    {
        "name": "a folder with a placeholder, whitespace and mixed field types",
        "repository": "artifact://docs/handbook",
        "branch": "main",
        "pages": [
            {
                "objects": [
                    {"key": "handbook/", "size_bytes": 0, "modified_at": "2026-10-01T10:00:00Z"},
                    {"key": "handbook/z.md", "size_bytes": 12, "modified_at": "2026-10-01T10:00:00.123456Z"},
                    {"key": " handbook/a.py ", "size_bytes": "40", "modified_at": "2026-10-02T09:00:00Z"},
                ],
                "common_prefixes": [],
                "next_cursor": "page-2",
            },
            {
                "objects": [
                    {"key": "handbook/sub/dir/b.txt", "size_bytes": 7.9, "modified_at": None},
                    {"key": "handbook/sub/c.json", "size_bytes": None},
                    {"key": "handbook/sub/ü.md", "size_bytes": 3, "modified_at": "2026-10-03T00:00:00Z"},
                    "not an object",
                    {"key": None, "size_bytes": 5},
                ],
            },
        ],
    },
    {
        "name": "the whole bucket on a branch label with odd characters",
        "repository": "artifact://docs",
        "branch": "release/v1 (rc)",
        "pages": [
            {
                "objects": [
                    {"key": "README.md", "size_bytes": 100, "modified_at": "2026-09-30T08:00:00Z"},
                    {"key": "B.md", "size_bytes": 1, "modified_at": "2026-09-30T08:00:00Z"},
                    {"key": "a.md", "size_bytes": 1, "modified_at": "2026-09-30T08:00:00Z"},
                ]
            }
        ],
    },
    {
        "name": "an empty folder",
        "repository": "artifact://docs/empty",
        "branch": "main",
        "pages": [{"objects": [{"key": "empty/", "size_bytes": 0}]}],
    },
    {
        "name": "a key outside the folder",
        "repository": "artifact://docs/handbook",
        "branch": "main",
        "pages": [{"objects": [{"key": "handbook-archive/x.md", "size_bytes": 1}]}],
    },
    {
        "name": "a traversal key",
        "repository": "artifact://docs/handbook",
        "branch": "main",
        "pages": [{"objects": [{"key": "handbook/../etc/passwd", "size_bytes": 1}]}],
    },
    {
        "name": "a backslash key",
        "repository": "artifact://docs/handbook",
        "branch": "main",
        "pages": [{"objects": [{"key": "handbook/a\\b.md", "size_bytes": 1}]}],
    },
    {
        "name": "an absolute key",
        "repository": "artifact://docs",
        "branch": "main",
        "pages": [{"objects": [{"key": "/etc/passwd", "size_bytes": 1}]}],
    },
    {
        "name": "a NUL key",
        "repository": "artifact://docs",
        "branch": "main",
        "pages": [{"objects": [{"key": "a\u0000b", "size_bytes": 1}]}],
    },
]

SETTINGS = [
    {"api_base": "https://platform.example/llm/v1", "api_key": "k", "organization": "42"},
    {"api_base": "https://platform.example/llm/api/v2/", "api_key": "k", "organization": 7},
    {"api_base": "https://platform.example/llm", "api_key": "k", "project_id": "9"},
    {"openai_api_base": "https://p.example/base/llm/v1", "openai_api_key": "k", "openai_organization": "3"},
    {"api_base": "https://platform.example/v1", "api_key": "k", "organization": "42"},
    {"api_base": "http://127.0.0.1:8080/llm/v1", "api_key": "k", "organization": ""},
]


def _error(exc: Exception) -> dict:
    return {"error_type": type(exc).__name__, "message": str(exc)}


def parse_cases() -> list:
    out = []
    for repository in PARSE:
        try:
            source = source_mod.parse_artifact_source(repository)
        except Exception as exc:  # noqa: BLE001 - the error IS the answer
            out.append({"repository": repository, "error": _error(exc)})
            continue
        out.append({
            "repository": repository,
            "bucket": source.bucket,
            "prefix": source.prefix,
            "url": source.url,
            "list_prefix": source.list_prefix,
            "slug": source.slug,
        })
    return out


def listing_case(case: dict) -> dict:
    repository, branch = case["repository"], case["branch"]
    source = source_mod.parse_artifact_source(repository)
    listing, calls = _list_through_client(case["pages"], source.bucket, source.list_prefix)
    out = {
        "name": case["name"],
        "repository": repository,
        "branch": branch,
        "pages": case["pages"],
        "list_params": calls,
    }
    try:
        objects = source_mod.collect_objects(source, listing)
        source_mod.check_caps(objects, source)
    except Exception as exc:  # noqa: BLE001
        out["error"] = _error(exc)
        return out
    digest = source_mod.listing_digest(objects)
    out["objects"] = [
        {"key": o.key, "size": o.size, "modified": o.modified, "relative_path": o.relative_path}
        for o in objects
    ]
    out["digest"] = digest
    with tempfile.TemporaryDirectory() as cache:
        path = source_mod.materialise_artifact_source(
            repository, branch, cache, client=_FakeClient(listing)
        )
        out["directory"] = os.path.basename(path)
        out["files"] = sorted(
            str(p.relative_to(path)) for p in Path(path).rglob("*") if p.is_file()
        )
        info = source_mod.artifact_repository_info(path)
    out["marker"] = {k: v for k, v in info.items() if k != "local_path"}
    canonical = canonical_repository_path(repository, None)
    identifier = build_repo_identifier(
        repository=repository, branch=info["branch"], commit_hash=info["commit_hash"]
    )
    out["canonical_repository"] = canonical
    out["repo_identifier"] = identifier
    out["wiki_id"] = normalize_wiki_id(identifier)
    return out


def main() -> None:
    target = Path(sys.argv[1])
    document = {
        "parse": parse_cases(),
        "listings": [listing_case(case) for case in LISTINGS],
        "settings": [
            {
                "llm_settings": block,
                "base_url": client_mod.extract_artifact_settings(block)["base_url"],
                "project_id": client_mod.extract_artifact_settings(block)["project_id"],
            }
            for block in SETTINGS
        ],
        "caps": {
            "max_files_env": source_mod.MAX_FILES_ENV,
            "max_bytes_env": source_mod.MAX_BYTES_ENV,
            "default_max_files": source_mod.DEFAULT_MAX_FILES,
            "default_max_bytes": source_mod.DEFAULT_MAX_BYTES,
        },
    }
    target.write_text(json.dumps(document, indent=1, ensure_ascii=False) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
