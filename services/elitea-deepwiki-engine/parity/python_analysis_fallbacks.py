#!/usr/bin/env python3
"""Write ``tests/fixtures/structure/analysis_fallbacks.json``: the
repository-analysis fallbacks of ``analyze_repository``
(``agents/wiki_graph_optimized.py``) on small repositories, from the
Python engine itself.

* ``doc_samples`` — the file walk finds only ``.go`` files, so the file
  sampler has nothing and the samples come from the graph builder's
  documents (``_extract_representative_code_samples``; its config test is
  a substring test: ``cmd/init.go`` holds ``ini``, ``platform`` holds
  ``tf``);
* ``doc_files`` — every file is under a directory the walk skips
  (``bin/``, ``out/``) but graph discovery keeps, so the file list is
  the documents' ``source`` paths;
* ``no_documents`` — no file yields a document: "No documents found in
  indexer".

Each case records the files (the Rust test writes them to a temporary
directory), the analysis request's messages, the tree, or the error.

    PYTHONHASHSEED=0 PYTHONPATH=services/elitea-deepwiki/src \\
        python parity/python_analysis_fallbacks.py services/elitea-deepwiki-engine/tests/fixtures/structure
"""

from __future__ import annotations

import json
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import python_structure_dump as dump  # noqa: E402
from python_structure_fixture import ScriptedLLM  # noqa: E402

GO_INIT = """package cmd

import "fmt"

// Init prepares the command tree.
func Init() error {
\tfmt.Println("init")
\treturn nil
}

// Version is the build version.
const Version = "1.0"

type Config struct {
\tName string
}
"""

GO_STATE = """package platform

// State is the platform state.
type State struct {
\tReady bool
}

func NewState() *State {
\treturn &State{Ready: true}
}
"""

CASES = {
    "doc_samples": {"cmd/init.go": GO_INIT, "pkg/platform/state.go": GO_STATE},
    "doc_files": {
        "bin/tool.go": GO_INIT,
        "bin/state.go": GO_STATE,
        "out/guide.md": "# Guide\n\nHow to run the tool.\n",
    },
    "no_documents": {"data/blob.xyz": "opaque\n"},
}


def run_case(name: str, files: dict[str, str]):
    from elitea_deepwiki.engine.agents.wiki_graph_optimized import OptimizedWikiGenerationAgent  # noqa: PLC0415
    from elitea_deepwiki.engine.state.wiki_state import TargetAudience, WikiStyle  # noqa: PLC0415

    with tempfile.TemporaryDirectory() as tmp:
        repo = Path(tmp) / "repo"
        for rel, text in files.items():
            (repo / rel).parent.mkdir(parents=True, exist_ok=True)
            (repo / rel).write_text(text, encoding="utf-8")
        repo = repo.resolve()
        _graph, documents = dump.build_graph(repo)
        llm = ScriptedLLM(lambda _i, _m: "analysis")
        agent = OptimizedWikiGenerationAgent(
            indexer=dump.StandInIndexer(documents, str(repo)),
            retriever_stack=object(),
            llm=llm,
            repository_url="acme/notes",
            branch="main",
            wiki_style=WikiStyle.COMPREHENSIVE,
            target_audience=TargetAudience.MIXED,
        )
        state = agent.analyze_repository({}, {"configurable": {}})
        return {
            "name": name,
            "files": files,
            "documents": [{"source": d.metadata.get("source"), "content": d.page_content} for d in documents],
            "messages": llm.calls[0]["messages"] if llm.calls else None,
            "repository_tree": state.get("repository_tree"),
            "errors": state.get("errors"),
        }


def main() -> int:
    out = Path(sys.argv[1]).resolve()
    dump.ref._make_reproducible()
    import os as _os  # noqa: PLC0415

    _os.walk = dump.sorted_walk(_os.walk)
    from elitea_deepwiki.engine.code_graph import graph_builder as gb  # noqa: PLC0415

    original = gb.EnhancedUnifiedGraphBuilder._discover_files_by_language

    def sorted_discover(self, *a, **kw):
        found = original(self, *a, **kw)
        return {language: sorted(paths) for language, paths in sorted(found.items())}

    gb.EnhancedUnifiedGraphBuilder._discover_files_by_language = sorted_discover
    cases = [run_case(name, files) for name, files in CASES.items()]
    (out / "analysis_fallbacks.json").write_text(
        json.dumps({"cases": cases}, indent=1, ensure_ascii=False) + "\n", encoding="utf-8"
    )
    print(json.dumps([(c["name"], c["errors"], len(c["documents"])) for c in cases]))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
