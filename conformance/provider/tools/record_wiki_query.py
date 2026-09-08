#!/usr/bin/env python3
"""Record the ``wiki_query`` family's answers as parity fixtures.

The four tools of the ``wiki_query`` toolkit — ``list_wikis``,
``resolve_and_ask``, ``resolve_and_deep_research`` and ``delete_wiki`` —
answer TEXT, and their whole value is the shape of that text plus the
``object_type`` it is wrapped in.  ADR-0022 freezes both, so this records them
by running the legacy handlers themselves:

* ``methods/invoke.py::Method._handle_wiki_query_tool`` does the dispatch and
  the toolkit/tool parameter merge;
* ``methods/invoke.py::Method._list_wikis`` / ``_resolve_and_ask`` /
  ``_resolve_and_deep_research`` / ``_delete_wiki`` build the answers;
* ``plugin_implementation/registry_manager.py::WikiRegistryManager`` reads the
  registry out of an in-memory artifact client, exactly as the real one reads
  it out of the bucket.

Only three things are stubbed, and each is stubbed because it is NOT what is
being recorded:

* ``MiniArtifactClient`` — replaced by an in-memory client holding a canned
  ``_registry/wikis.json``.  The HTTP transport is not the subject; the text
  built from what it returns is.
* ``_resolve_wiki_with_llm`` — replaced by a function that returns a fixed
  wiki id.  Which wiki a model picks is not freezable; what the handler does
  with the pick is.
* ``self.ask`` / ``self.deep_research`` — replaced by canned engine results,
  the same technique ``record_generation.py`` uses for ``generate_wiki``.

ONE LEGACY DEFECT IS RECORDED AS SUCH rather than reproduced.
``_delete_wiki`` calls ``WikiRegistryManager.delete_wiki_with_artifacts``,
which calls ``client.list_artifacts`` — a method ``MiniArtifactClient`` does
not define.  Every legacy delete therefore raised ``AttributeError`` into the
manager's own ``except Exception``, deleted nothing, and answered "deletion
completed with errors".  The recording below drives the real handler with a
client that HAS ``list_artifacts`` (so the working path is what is frozen) and
also records the broken-client answer under ``legacy_defect``, so the port's
divergence from it is a documented decision rather than a silent one.

Output (under ``fixtures/deepwiki/wiki_query/``):

    list_wikis.json                the registry, and both rendered formats
    resolve_and_ask.json           the resolved answer and every refusal text
    resolve_and_deep_research.json the report, and the max_tokens widening
    delete_wiki.json               the delete report, both outcomes

Usage:
    python tools/record_wiki_query.py [--check]
"""

from __future__ import annotations

import argparse
import copy
import importlib.util
import json
import sys
import types
from pathlib import Path
from typing import Any, Dict, List

sys.path.insert(0, str(Path(__file__).resolve().parent))

from _legacy import install_pylon_stub, legacy_root, source_pin  # noqa: E402

ROOT = Path(__file__).resolve().parents[1]
FIXTURES = ROOT / "fixtures" / "deepwiki" / "wiki_query"

SOURCE_FILES = [
    "methods/invoke.py",
    "plugin_implementation/registry_manager.py",
]

BUCKET = "wiki-artifacts"

#: Two wikis, so resolution has something to choose between and the compact
#: listing has more than one line.  The entry shape is
#: ``WikiRegistryManager.register_wiki``'s own.
REGISTRY: Dict[str, Any] = {
    "schema_version": 1,
    "updated_at": "2026-01-01T00:00:00Z",
    "wikis": [
        {
            "id": "acme--notes-service--main",
            "repo": "acme/notes-service",
            "branch": "main",
            "provider": "github",
            "host": "github.com",
            "display_name": "notes-service (main)",
            "description": "The notes service: storage, retrieval and the sync worker.",
            "topics": ["python", "storage"],
            "folder_path": "acme--notes-service--main/",
            "commit_hash": "0000000deadbeef0000000deadbeef0000000000",
            "canonical_repo_identifier": "acme/notes-service:main:0000000",
            "analysis_key": "acme--notes-service--main/analysis/repository_analysis.json",
            "created_at": "2026-01-01T00:00:00Z",
            "updated_at": "2026-01-01T00:00:00Z",
        },
        {
            # A LEGACY-SHAPED entry: `repo` carries the branch and commit, and
            # `branch` is absent.  Both quirks are handled by the handler
            # (`repo.split(":")[0]`, `parse_wiki_id`), so both are recorded.
            "id": "acme--billing--release-2",
            "repo": "acme/billing:release-2:abc1234",
            "provider": "gitlab",
            "display_name": "billing (release-2)",
            "description": "",
            "folder_path": "acme--billing--release-2/",
            "created_at": "2026-01-02T00:00:00Z",
        },
    ],
}

WIKI_OBJECTS = [
    {"name": "acme--notes-service--main/wiki_manifest_20260101T000000Z.json"},
    {"name": "acme--notes-service--main/wiki_pages/overview/getting-started.md"},
    {"name": "acme--notes-service--main/analysis/repository_analysis.json"},
    {"name": "acme--billing--release-2/wiki_manifest_20260102T000000Z.json"},
    {"name": "_registry/wikis.json"},
]


class InMemoryArtifacts:
    """The registry manager's client interface, over a dict.

    ``list_artifacts`` and ``delete_artifact`` are the two methods the real
    ``MiniArtifactClient`` never had; :class:`LegacyMiniArtifactClient` below
    is the surface it actually offered.
    """

    def __init__(self, registry: Dict[str, Any], *, undeletable: tuple = ()) -> None:
        self.objects = {"_registry/wikis.json": json.dumps(registry, indent=2)}
        self.undeletable = undeletable
        self.deleted: List[str] = []

    def download_artifact(self, bucket_name: str, artifact_name: str) -> bytes:
        if artifact_name not in self.objects:
            raise RuntimeError(f"Artifact not found: {bucket_name}/{artifact_name}")
        return self.objects[artifact_name].encode("utf-8")

    def create_artifact(self, bucket_name: str, artifact_name: str, data: Any) -> Dict:
        self.objects[artifact_name] = data if isinstance(data, str) else data.decode("utf-8")
        return {"status": "ok"}

    def list_artifacts(self, bucket_name: str) -> List[Dict[str, str]]:
        return [dict(entry) for entry in WIKI_OBJECTS]

    def delete_artifact(self, bucket_name: str, artifact_name: str) -> None:
        if artifact_name in self.undeletable:
            raise RuntimeError("Not authorized to delete artifact")
        self.deleted.append(artifact_name)
        self.objects.pop(artifact_name, None)


class LegacyMiniArtifactClient:
    """The legacy client's ACTUAL surface: download and create, nothing more."""

    def __init__(self, backing: InMemoryArtifacts) -> None:
        self._backing = backing

    def download_artifact(self, bucket_name: str, artifact_name: str) -> bytes:
        return self._backing.download_artifact(bucket_name, artifact_name)

    def create_artifact(self, bucket_name: str, artifact_name: str, data: Any) -> Dict:
        return self._backing.create_artifact(bucket_name, artifact_name, data)


LLM_SETTINGS = {
    "model_name": "gpt-4o",
    "api_base": "http://elitea/llm/v1",
    "api_key": "<redacted>",
    "organization": "90200",
}


#: The synthetic package the legacy tree is imported under.  The wiki_query
#: handlers do `from ..plugin_implementation.registry_manager import …` INSIDE
#: the function body, so — unlike the generation recorder, which never reaches
#: such a line — invoke.py has to be a submodule of a package whose sibling
#: `plugin_implementation` is importable.  Nothing is copied: the packages are
#: empty modules whose __path__ points into the read-only legacy checkout.
PACKAGE = "deepwiki_legacy_wq"


def load_invoke():
    """Import ``methods/invoke.py`` with Pylon and tasknode stubbed."""
    install_pylon_stub()
    tasknode = types.ModuleType("tasknode_task")
    tasknode.id = "00000000-0000-4000-8000-000000000000"
    sys.modules["tasknode_task"] = tasknode

    root = legacy_root()
    for name, path in (
        (PACKAGE, root),
        (f"{PACKAGE}.methods", root / "methods"),
        (f"{PACKAGE}.plugin_implementation", root / "plugin_implementation"),
    ):
        package = types.ModuleType(name)
        package.__path__ = [str(path)]
        sys.modules[name] = package

    name = f"{PACKAGE}.methods.invoke"
    spec = importlib.util.spec_from_file_location(name, root / "methods" / "invoke.py")
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    return mod


def module_under_test(mod, artifacts, *, resolved="acme--notes-service--main",
                      ask=None, deep_research=None):
    """A stand-in for the Pylon module carrying only what the handlers touch."""
    calls: Dict[str, Any] = {}

    class _Module:
        _handle_wiki_query_tool = mod.Method._handle_wiki_query_tool
        _list_wikis = mod.Method._list_wikis
        _resolve_and_ask = mod.Method._resolve_and_ask
        _resolve_and_deep_research = mod.Method._resolve_and_deep_research
        _delete_wiki = mod.Method._delete_wiki
        _create_error_response = mod.Method._create_error_response

        def _resolve_wiki_with_llm(self, question, wikis, llm_settings):
            calls["resolve"] = {"question": question,
                                "wiki_ids": [w["wiki_id"] for w in wikis],
                                "model_name": llm_settings.get("model_name")}
            return resolved

        def ask(self, **kwargs):
            calls["ask"] = kwargs
            return ask if ask is not None else {"success": True, "answer": "The notes service stores notes in Postgres."}

        def deep_research(self, **kwargs):
            calls["deep_research"] = kwargs
            return deep_research if deep_research is not None else {
                "success": True, "report": "# Architecture\n\nThe notes service is three processes."}

    mod.MiniArtifactClient = lambda settings: artifacts  # noqa: ARG005
    return _Module(), calls


def objects_of(response: Dict[str, Any]) -> List[Dict[str, Any]]:
    return json.loads(response["result"])


def record() -> Dict[str, Dict[str, Any]]:
    mod = load_invoke()
    pin = source_pin(SOURCE_FILES)
    request = {"configuration": {"parameters": {"llm_settings": LLM_SETTINGS}}}

    def invoke(tool, parameters, *, artifacts=None, **kwargs):
        store = artifacts if artifacts is not None else InMemoryArtifacts(copy.deepcopy(REGISTRY))
        module, calls = module_under_test(mod, store, **kwargs)
        body = copy.deepcopy(request)
        body["parameters"] = parameters
        response = mod.Method._handle_wiki_query_tool(module, "x", tool, body)
        return response, calls, store

    fixtures: Dict[str, Dict[str, Any]] = {}

    # ---- list_wikis -------------------------------------------------------
    compact, _, _ = invoke("list_wikis", {})
    metadata, _, _ = invoke("list_wikis", {"include_metadata": True})
    empty, _, _ = invoke("list_wikis", {}, artifacts=InMemoryArtifacts(
        {"schema_version": 1, "wikis": [], "updated_at": "2026-01-01T00:00:00Z"}))
    fixtures["list_wikis"] = {
        "_source": pin,
        "producer": "methods/invoke.py::Method._list_wikis",
        "how_recorded": "MiniArtifactClient replaced by an in-memory client over the registry below;"
                        " every line that builds the text is legacy code",
        "registry": REGISTRY,
        "cases": {
            "compact": {"parameters": {}, "result_objects": objects_of(compact)},
            "include_metadata": {"parameters": {"include_metadata": True},
                                 "result_objects": objects_of(metadata)},
            "empty_registry": {"parameters": {}, "result_objects": objects_of(empty)},
        },
    }

    # ---- resolve_and_ask --------------------------------------------------
    answered, ask_calls, _ = invoke("resolve_and_ask", {"question": "How does notes-service store notes?"})
    hinted, hint_calls, _ = invoke("resolve_and_ask",
                                   {"question": "What is release-2?",
                                    "wiki_id_hint": "acme--billing--release-2"})
    unresolved, _, _ = invoke("resolve_and_ask", {"question": "How does anything work?"}, resolved=None)
    missing, _, _ = invoke("resolve_and_ask", {"question": "?"}, resolved="acme--nope--main")
    none_available, _, _ = invoke("resolve_and_ask", {"question": "?"}, artifacts=InMemoryArtifacts(
        {"schema_version": 1, "wikis": [], "updated_at": "2026-01-01T00:00:00Z"}))
    failed, _, _ = invoke("resolve_and_ask", {"question": "?"},
                          ask={"success": False, "error": "index unavailable"})
    fixtures["resolve_and_ask"] = {
        "_source": pin,
        "producer": "methods/invoke.py::Method._resolve_and_ask",
        "how_recorded": "the artifact client, _resolve_wiki_with_llm and self.ask are stubbed;"
                        " resolution handling, repo_config construction and composition are legacy code",
        "cases": {
            "resolved": {"parameters": {"question": "How does notes-service store notes?"},
                         "ask_arguments": ask_calls["ask"],
                         "result_objects": objects_of(answered)},
            "wiki_id_hint": {"parameters": {"question": "What is release-2?",
                                            "wiki_id_hint": "acme--billing--release-2"},
                             "ask_arguments": hint_calls["ask"],
                             "result_objects": objects_of(hinted)},
            "unresolved": {"result_objects": objects_of(unresolved)},
            "wiki_not_in_registry": {"result_objects": objects_of(missing)},
            "no_wikis": {"result_objects": objects_of(none_available)},
            "retrieval_failed": {"result_objects": objects_of(failed)},
        },
    }

    # ---- resolve_and_deep_research ---------------------------------------
    researched, research_calls, _ = invoke(
        "resolve_and_deep_research",
        {"question": "How is notes-service put together?", "research_type": "architecture"})
    unresolved_research, _, _ = invoke("resolve_and_deep_research", {"question": "?"}, resolved=None)
    fixtures["resolve_and_deep_research"] = {
        "_source": pin,
        "producer": "methods/invoke.py::Method._resolve_and_deep_research",
        "how_recorded": "as resolve_and_ask, with self.deep_research stubbed",
        "cases": {
            "resolved": {
                "parameters": {"question": "How is notes-service put together?",
                               "research_type": "architecture"},
                "deep_research_arguments": research_calls["deep_research"],
                "result_objects": objects_of(researched),
            },
            "unresolved": {"result_objects": objects_of(unresolved_research)},
        },
    }

    # ---- delete_wiki ------------------------------------------------------
    working = InMemoryArtifacts(copy.deepcopy(REGISTRY))
    deleted, _, _ = invoke("delete_wiki", {"wiki_id": "acme--notes-service--main"}, artifacts=working)
    partial_store = InMemoryArtifacts(
        copy.deepcopy(REGISTRY),
        undeletable=("acme--notes-service--main/analysis/repository_analysis.json",))
    partial, _, _ = invoke("delete_wiki", {"wiki_id": "acme--notes-service--main"}, artifacts=partial_store)
    unknown, _, _ = invoke("delete_wiki", {"wiki_id": "acme--nope--main"})
    broken_backing = InMemoryArtifacts(copy.deepcopy(REGISTRY))
    broken, _, _ = invoke("delete_wiki", {"wiki_id": "acme--notes-service--main"},
                          artifacts=LegacyMiniArtifactClient(broken_backing))
    fixtures["delete_wiki"] = {
        "_source": pin,
        "producer": "methods/invoke.py::Method._delete_wiki",
        "how_recorded": "the artifact client is in-memory AND has list_artifacts/delete_artifact,"
                        " which the real MiniArtifactClient did not — so the two cases below record"
                        " what the legacy code WOULD have answered, not what it ever answered in"
                        " production. See legacy_defect. The deletion loop, the registry unregister"
                        " and the messages are legacy code.",
        "cases": {
            "deleted": {"parameters": {"wiki_id": "acme--notes-service--main"},
                        "deleted_keys": working.deleted,
                        "result_objects": objects_of(deleted)},
            "partial_failure": {"deleted_keys": partial_store.deleted,
                                "result_objects": objects_of(partial)},
            "unknown_wiki": {"result_objects": objects_of(unknown)},
        },
        "legacy_defect": {
            "what": "MiniArtifactClient defines only download_artifact and create_artifact,"
                    " so delete_wiki_with_artifacts' client.list_artifacts raised AttributeError"
                    " into its own except Exception. Every legacy delete deleted nothing and"
                    " reported errors.",
            "result_objects": objects_of(broken),
            "port_decision": "NOT reproduced. The Go host deletes for real, in ONE batch through"
                             " the platform's :batchDelete route, and names the keys that survived"
                             " — DeleteWikiButton's semantics.",
        },
    }
    return fixtures


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true",
                        help="verify the committed fixtures instead of writing them")
    args = parser.parse_args()

    recorded = record()
    FIXTURES.mkdir(parents=True, exist_ok=True)
    failures = []
    for name, payload in recorded.items():
        path = FIXTURES / f"{name}.json"
        text = json.dumps(payload, indent=2, ensure_ascii=False) + "\n"
        if args.check:
            if not path.is_file() or path.read_text(encoding="utf-8") != text:
                failures.append(str(path.relative_to(ROOT)))
        else:
            path.write_text(text, encoding="utf-8")
            print(f"wrote {path.relative_to(ROOT)}")
    if failures:
        print("stale fixtures: " + ", ".join(failures), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
