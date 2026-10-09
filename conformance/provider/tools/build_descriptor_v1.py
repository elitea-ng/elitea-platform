#!/usr/bin/env python3
"""Derive DeepWiki descriptor revision legacy-v1 from legacy-v0.

TWO ARGUMENTS, ON TWO TOOLS. `ask` and `deep_research` gain:

    chat_history    already sent on every wiki-chat turn and never declared
    context_paths   NEW: the wiki pages a reader attached to the question
    context_wiki_version_id
                    NEW: the wiki version that selection was made in

`chat_history` is the same class of gap the Inventory legacy-v1 revision
closed: the legacy plugin READ it (methods/invoke.py passes it straight into
the engine's keyword set) and never declared it, which worked only because
the legacy UI called the provider's own routes. Under ADR-0022/0023 the
admission plane reads the descriptor to know a tool's argument shape, so an
undeclared argument is an undocumented one — and adding `context_paths`
beside a still-undeclared `chat_history` would ship the new gap next to the
old one.

The invoke `parameters` object is an `x-elitea-passthrough`
additionalProperties passthrough, so NO api/v2.yaml edit is needed and none
is made: this is about what the descriptor ADVERTISES, not about what the
HTTP schema permits.

Generated rather than hand-edited so the diff against legacy-v0 is exactly
these insertions and nothing else — a hand edit of a 17 KB document cannot
be reviewed for what it did NOT change.

    python tools/build_descriptor_v1.py            rewrite the v1 fixture
    python tools/build_descriptor_v1.py --check    verify it is current
"""

from __future__ import annotations

import argparse
import collections
import json
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]
FIXTURES = REPO_ROOT / "conformance" / "provider" / "fixtures" / "deepwiki" / "descriptor"
V0 = FIXTURES / "legacy-v0" / "provider_descriptor.json"
V1 = FIXTURES / "legacy-v1" / "provider_descriptor.json"

#: The host serves this document; it is a byte copy with the service
#: location substituted, pinned by a conformance test.
SERVED = (
    REPO_ROOT
    / "services"
    / "elitea-subapp-host"
    / "internal"
    / "apps"
    / "deepwiki"
    / "descriptor.json"
)

#: The tools that take a question, and so can take context in front of it.
#: generate_wiki has no question; the wiki_query family resolves WHICH wiki
#: only after the model has answered, so a selection made before that would
#: be a selection against an unknown wiki.
TOOLS = ("ask", "deep_research")


def _od(pairs):
    return collections.OrderedDict(pairs)


def _arg(type_, required, description, default=...):
    pairs = [("type", type_), ("required", required), ("description", description)]
    if default is not ...:
        pairs.append(("default", default))
    return _od(pairs)


#: Appended to each tool's args_schema, in this order.
NEW_ARGS = _od(
    [
        (
            "chat_history",
            _arg(
                "JSON",
                False,
                "Prior turns as [{role: 'user'|'assistant', content: str}]; "
                "the engine folds the last four into the retrieval question",
                [],
            ),
        ),
        (
            "context_paths",
            _arg(
                "JSON",
                False,
                "Wiki page ids the reader attached to this question, as "
                "published in the pinned version's manifest. Identifiers "
                "only: page content is resolved server-side from this wiki's "
                "own artifacts, and an id that is not published in that "
                "version is refused. Repository paths and URLs are not "
                "selectable.",
                [],
            ),
        ),
        (
            "context_wiki_version_id",
            _arg(
                "String",
                False,
                "The wiki version context_paths was selected in. Required "
                "whenever context_paths is given, so a question asked "
                "against the version on screen cannot silently resolve "
                "against a newer one.",
            ),
        ),
    ]
)


def build() -> str:
    document = json.loads(V0.read_text(encoding="utf-8"), object_pairs_hook=collections.OrderedDict)
    for toolkit in document["provided_toolkits"]:
        for tool in toolkit.get("provided_tools", []):
            if tool["name"] not in TOOLS:
                continue
            schema = tool.setdefault("args_schema", collections.OrderedDict())
            for name, spec in NEW_ARGS.items():
                schema[name] = spec
    return json.dumps(document, indent=2, ensure_ascii=False) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    built = build()
    if args.check:
        if V1.read_text(encoding="utf-8") != built:
            print("legacy-v1 is not what this tool builds; re-run it", file=sys.stderr)
            return 1
        if SERVED.read_text(encoding="utf-8") != built:
            print(
                f"{SERVED} has drifted from the legacy-v1 fixture; re-run this tool",
                file=sys.stderr,
            )
            return 1
        print("ok")
        return 0
    V1.parent.mkdir(parents=True, exist_ok=True)
    V1.write_text(built, encoding="utf-8")
    SERVED.write_text(built, encoding="utf-8")
    print(f"wrote {V1} and {SERVED}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
