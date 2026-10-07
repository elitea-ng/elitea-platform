#!/usr/bin/env python3
"""Write the golden fixtures of ``tests/structure_golden.rs`` from the
Python engine itself.

Two fixtures, both with a SCRIPTED model (no stub, no network): every
request's messages are recorded with the answer the script gave, so the
Rust test replays the answers in order and compares the requests.

``cluster.json``
    A synthetic index (``repo_nodes`` / ``repo_edges`` rows written into a
    real ``UnifiedWikiDB``) shaped to reach every planner branch: a page
    the validator demotes, one it splits by file, one it merges, a docs
    page, a section whose batched answer misses a page (multi-call
    naming), one whose calls all fail (the fallback section), one that
    answers a JSON list, one with a non-string page name, ``PageRank`` over
    parallel, reciprocal and self-loop edges with ties. Run twice: as is,
    and with ``DEEPWIKI_EXCLUDE_TESTS=1``.
``analysis.json``
    ``analyze_repository`` over ``tests/fixtures/structure/repo`` (the
    graph builder's documents for the README), then the classic planner
    (``planner_type=auto``) on five scripted structure answers.

The planner's sets are pinned to insertion order as in
``python_structure_dump.py``.

    PYTHONHASHSEED=0 PYTHONPATH=services/elitea-deepwiki/src \\
        python parity/python_structure_fixture.py services/elitea-deepwiki-engine/tests/fixtures/structure
"""

from __future__ import annotations

import json
import os
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import python_structure_dump as dump  # noqa: E402


class Answer:
    def __init__(self, content):
        self.content = content


class ScriptedLLM:
    """``llm.invoke(messages)`` answered by ``script(index, messages)``: a
    string, or an exception to raise."""

    def __init__(self, script):
        self.script = script
        self.calls = []

    def invoke(self, messages, config=None):
        recorded = [[_role(m), m.content] for m in messages]
        answer = self.script(len(self.calls), recorded)
        self.calls.append({"messages": recorded, "answer": answer if isinstance(answer, str) else None})
        if not isinstance(answer, str):
            raise answer
        return Answer(answer)


def _role(message) -> str:
    return {"system": "system", "human": "user", "ai": "assistant"}[message.type]


def node(node_id, rel_path, name, stype, macro, micro, *, doc="", source="x" * 60, sig="", arch=1, test=0):
    is_doc = 1 if stype in ("markdown_section", "markdown_document", "yaml_document") else 0
    return {
        "node_id": node_id,
        "rel_path": rel_path,
        "symbol_name": name,
        "symbol_type": stype,
        "signature": sig,
        "docstring": doc,
        "source_text": source,
        "is_architectural": arch,
        "is_doc": is_doc,
        "is_test": test,
        "macro_cluster": macro,
        "micro_cluster": micro,
    }


def synthetic_rows():
    rows = []
    # Section 0: a 12-node page (PageRank with ties, every tier) and a
    # 3-node page (returned in node order).
    core = [
        ("Engine", "class"), ("run", "function"), ("Loader", "class"), ("MAX_SIZE", "constant"),
        ("parse", "function"), ("Config", "struct"), ("helper_util", "function"), ("Mode", "enum"),
        ("Alias", "type_alias"), ("start", "method"), ("Overview", "markdown_section"), ("misc", "variable"),
    ]
    for i, (name, stype) in enumerate(core):
        rel = "src/core/engine.py" if i % 3 else "src/core/loader.py"
        if stype == "markdown_section":
            rel = "docs/core.md"
        rows.append(node(f"c{i}", rel, name, stype, 0, 0, doc="Documented." if i % 4 == 0 else "",
                         sig=f"def {name}(a, b) -> None" + " " * (i * 30)))
    for i, name in enumerate(["Zeta", "alpha", "Beta"]):
        rows.append(node(f"s{i}", "src/small.py", name, "class", 0, 3))
    # Section 1: page 2 (5 nodes), page 5 (a bridge over 4 files: split),
    # page 7 (one node: merged), page 8 (docs only: promoted).
    for i in range(5):
        rows.append(node(f"p{i}", "src/api/handlers.py", f"Handler{i}", "class", 1, 2))
    for i in range(4):
        rows.append(node(f"b{i}", f"src/bridge/f{i}.py", f"bridge_{i}", "function", 1, 5))
    rows.append(node("lone", "src/api/lone.py", "Lone", "class", 1, 7))
    for i in range(3):
        rows.append(node(f"d{i}", "docs/api.md", f"API section {i}", "markdown_section", 1, 8))
    # Section 2: page 0 constants only (demoted), page 1 four classes in
    # pkg/a and pkg/b (all calls fail: fallback section "Module: pkg").
    for i in range(3):
        rows.append(node(f"k{i}", "pkg/consts.py", f"K{i}", "constant", 2, 0))
    for i in range(4):
        rows.append(node(f"m{i}", f"pkg/{'ab'[i % 2]}/mod.py", f"Model{i}", "class", 2, 1))
    # Section 3: answers a JSON list. Section 4: a non-string page name,
    # and test nodes (out under exclude_tests).
    for i in range(2):
        rows.append(node(f"l{i}", "lib/list.py", f"Lister{i}", "class", 3, 0))
    for i in range(3):
        rows.append(node(f"t{i}", "tests/test_x.py", f"TestX{i}", "class", 4, 0, test=1))
    rows.append(node("t3", "lib/real.py", "Real", "class", 4, 0))
    # Not in the map: no section, not architectural.
    rows.append(node("none", "src/x.py", "Unclustered", "class", None, None))
    rows.append(node("meth", "src/core/engine.py", "inner", "method", 0, 0, arch=0))
    edges = []
    for i in range(1, 9):
        edges.append(["c0", f"c{i}", 1.0])
    edges += [["c1", "c2", 0.5], ["c2", "c1", 2.0], ["c1", "c2", 0.25], ["c3", "c3", 1.0], ["c4", "c5", 0.0],
              ["c5", "c4", None], ["c6", "c7", 1.0], ["c8", "c9", 1.0], ["c10", "c0", 1.0], ["meth", "c0", 1.0]]
    edges += [["p0", "p1", 1.0], ["lone", "p3", 1.0], ["lone", "b0", 1.0], ["b1", "lone", 1.0], ["p2", "b2", 1.0]]
    edges += [["s0", "s1", 1.0], ["m0", "m1", 1.0], ["none", "c0", 1.0]]
    return rows, edges


def write_db(path: str, rows, edges, repo_identifier: str):
    from elitea_deepwiki.engine.unified_db import UnifiedWikiDB  # noqa: PLC0415

    udb = UnifiedWikiDB(path, embedding_dim=16)
    columns = list(rows[0].keys())
    udb.conn.executemany(
        f"INSERT INTO repo_nodes ({', '.join(columns)}) VALUES ({', '.join('?' for _ in columns)})",
        [[row[c] for c in columns] for row in rows],
    )
    udb.conn.executemany(
        "INSERT INTO repo_edges (source_id, target_id, rel_type, weight) VALUES (?, ?, 'calls', ?)",
        edges,
    )
    udb.conn.commit()
    udb.set_meta("repo_identifier", repo_identifier)
    udb.set_meta("phase3_completed", True)
    return udb


def cluster_script(index, messages):
    system, user = messages[0][1], messages[1][1]
    if "SECTION CLUSTER" in user and "PAGES IN THIS SECTION" in user:
        pages = json.loads(user.split("PAGES IN THIS SECTION:\n", 1)[1].split("\n\nOutput ONLY", 1)[0])
        ids = [p["page_id"] for p in pages]
        names = [p["page_symbols"][0]["name"] if p["page_symbols"] else "none" for p in pages]
        if "Model" in user:  # section 2: every call fails
            return RuntimeError("upstream 500")
        if "Lister" in user:  # section 3: a JSON list
            return '["not", "an", "object"]'
        if "Real" in user:  # section 4: a page name that is a number
            return json.dumps({"section_name": "Odd", "pages": [{"page_id": i, "page_name": 7} for i in ids]})
        if "Handler" in user:  # section 1: one page missing
            return json.dumps({"section_name": "Api", "pages": [{"page_id": ids[0], "page_name": "Handlers"}]})
        return "```json\n" + json.dumps({
            "section_name": "Core Engine",
            "section_description": "",
            "pages": [
                {"page_id": str(i), "page_name": f"Page of {n}", "description": None, "retrieval_query": "q " + n}
                for i, n in zip(ids, names)
            ] + [{"page_id": "bad"}, "skip"],
        }, ensure_ascii=False) + "\n```"
    if "PAGE CLUSTER" in user:
        if "Model" in user:
            return RuntimeError("upstream 500")
        if "Handler0" in user:
            return RuntimeError("timeout")
        return 'Here: {"page_name": "Named page", "description": "Described.", "retrieval_query": "rq",}'
    if "SECTION (derived from" in user:
        if "Page 1" in user and "Model" not in user and "Named page" not in user:
            return RuntimeError("upstream 500")
        return '{"section_name": "From Pages", "section_description": "Derived."}'
    raise AssertionError(f"unexpected prompt {index}: {user[:80]}")


def cluster_case(exclude_tests: bool):
    os.environ["DEEPWIKI_EXCLUDE_TESTS"] = "1" if exclude_tests else "0"
    from elitea_deepwiki.engine.wiki_structure_planner import cluster_planner as cp  # noqa: PLC0415

    rows, edges = synthetic_rows()
    with tempfile.TemporaryDirectory() as scratch:
        udb = write_db(os.path.join(scratch, "fixture.wiki.db"), rows, edges, "acme/notes:main:0123abcd")
        llm = ScriptedLLM(cluster_script)
        spec = cp.ClusterStructurePlanner(db=udb, llm=llm).plan_structure()
        udb.close()
    return {
        "exclude_tests": exclude_tests,
        "calls": llm.calls,
        "structure": spec.model_dump(),
    }


def analysis_fixture(repo: Path):
    from elitea_deepwiki.engine.agents.wiki_graph_optimized import OptimizedWikiGenerationAgent  # noqa: PLC0415
    from elitea_deepwiki.engine.state.wiki_state import TargetAudience, WikiStyle  # noqa: PLC0415

    _graph, documents = dump.build_graph(repo)
    context = "## Analysis\n\nThe notes service — store and API. 😀"
    answers = {
        "valid": json.dumps({
            "wiki_title": "Notes", "overview": "o", "total_pages": "2",
            "sections": [{"section_name": "S", "section_order": 1.0, "description": "d", "rationale": "r",
                          "pages": [{"page_name": "P", "page_order": True, "description": "d", "content_focus": "c",
                                     "rationale": "r", "key_files": ["notes/api.py"], "extra": 1},
                                    {"page_name": "Q", "page_order": "2", "description": "d", "content_focus": "c",
                                     "rationale": "r", "metadata": {"k": [1]}}]}],
        }),
        "fenced": "Sure:\n```JSON\n" + json.dumps({"wiki_title": "T", "overview": "", "sections": [], "total_pages": 0}) + "\n```",
        "invalid": json.dumps({"wiki_title": 5, "overview": "o", "sections": [], "total_pages": 0}),
        "no_json": "I cannot do that.",
        "broken": "```json\n{not json}\n```",
        "list": "[1, 2]",
    }
    cases = []
    analysis_calls = None
    for name, answer in answers.items():
        script_answers = [context, answer]
        llm = ScriptedLLM(lambda i, _m: script_answers[i])
        agent = OptimizedWikiGenerationAgent(
            indexer=dump.StandInIndexer(documents, str(repo)),
            retriever_stack=object(),
            llm=llm,
            repository_url="acme/notes",
            branch="main",
            wiki_style=WikiStyle.COMPREHENSIVE,
            target_audience=TargetAudience.MIXED,
        )
        config = {"configurable": {"planner_type": "auto"}}
        state = agent.analyze_repository({}, config)
        result = agent.generate_wiki_structure(dict(state), config)
        analysis_calls = llm.calls[0]
        spec = result.get("wiki_structure_spec")
        cases.append({
            "name": name,
            "answer": answer,
            "messages": llm.calls[1]["messages"],
            "structure": spec.model_dump() if spec is not None else None,
            "error": bool(result.get("errors")),
        })
    return {
        "repository_name": "acme/notes",
        "branch": "main",
        "analysis": analysis_calls,
        "repository_tree": state["repository_tree"],
        "readme_content": state["readme_content"],
        "classic": cases,
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
    analysis = analysis_fixture(out / "repo")
    dump.install_planner_patches(out)
    cases = [cluster_case(False), cluster_case(True)]
    rows, edges = synthetic_rows()
    (out / "cluster.json").write_text(
        json.dumps({"rows": rows, "edges": edges, "repo_identifier": "acme/notes:main:0123abcd", "cases": cases},
                   indent=1, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    (out / "analysis.json").write_text(json.dumps(analysis, indent=1, ensure_ascii=False) + "\n", encoding="utf-8")
    print(json.dumps({
        "cluster_calls": [len(c["calls"]) for c in cases],
        "sections": [[s["section_name"] for s in c["structure"]["sections"]] for c in cases],
        "classic": [(c["name"], c["error"], c["structure"]["wiki_title"] if c["structure"] else None) for c in analysis["classic"]],
    }, indent=1))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
