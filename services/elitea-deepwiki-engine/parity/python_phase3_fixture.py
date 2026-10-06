#!/usr/bin/env python3
"""Write the Phase 3 golden fixtures: Python Phase 3 on synthetic graphs.

The Rust replay golden test (``tests/phase3_golden.rs``) runs the Rust
Phase 3 on these graphs with leidenalg's recorded memberships and must
produce the same assignments and stats. The graphs are small and built to
reach every branch of the live path:

``default``
    Twelve directory groups of files with dense intra-group and sparse
    cross-group edges (section consolidation runs), isolated files
    (directory-proximity assignment), a node with only ``file_name`` and
    one with no path (``<unknown>``), self-loops, parallel and opposite
    edges, edges added in shuffled order (predecessor order differs from
    node order), three hubs (one without edges), test files.
``exclude_tests_legacy``
    The same graph with ``exclude_tests`` on and the legacy weight profile
    (section γ = 1.0).
``all_isolated``
    Files without cross-file edges only: no section call, one section per
    file, single-page sections with and without edges.

    PYTHONHASHSEED=0 python parity/python_phase3_fixture.py tests/fixtures/phase3

Same output files as ``python_phase3_dump.py``.
"""

from __future__ import annotations

import json
import random
import sys
from pathlib import Path

import networkx as nx

sys.path.insert(0, str(Path(__file__).resolve().parent))

from python_phase3_dump import assignment_rows, graph_rows, install_recorders, write_dump  # noqa: E402


class StubDB:
    """The calls ``persist_clusters`` and ``_run_phase3_*`` make."""

    class _Conn:
        def execute(self, *args, **kwargs):
            return None

        def commit(self):
            return None

    def __init__(self) -> None:
        self.conn = self._Conn()
        self.meta = {}

    def set_clusters_batch(self, batch):
        return None

    def set_hub(self, node_id, is_hub=True, assignment=None):
        return None

    def set_meta(self, key, value):
        self.meta[key] = value


def node(graph: nx.MultiDiGraph, nid: str, rel_path: str | None, file_name: str | None, doc: dict, is_doc=False):
    attrs = {"symbol_type": "markdown_section" if is_doc else "class"}
    if rel_path is not None:
        attrs["rel_path"] = rel_path
    if file_name is not None:
        attrs["file_name"] = file_name
    graph.add_node(nid, **attrs)
    doc[nid] = is_doc


def build_default(rng: random.Random) -> tuple[nx.MultiDiGraph, dict, set]:
    g = nx.MultiDiGraph()
    doc: dict = {}
    groups: list[list[str]] = []
    for k in range(12):
        members = []
        for j in range(4):
            path = f"g{k}/f{j}.py"
            for i in range(rng.randint(3, 7)):
                nid = f"py::{path}::S{i}"
                node(g, nid, path, f"f{j}.py", doc)
                members.append(nid)
        groups.append(members)
    # Isolated files: intra-file edges only, and one file with no edges.
    isolated = []
    for m in range(4):
        path = f"g{m * 3}/iso{m}.py"
        pair = [f"py::{path}::A", f"py::{path}::B"]
        for nid in pair:
            node(g, nid, path, f"iso{m}.py", doc)
        isolated.append(pair)
    node(g, "py::g3/lonely.py::Alone", "g3/lonely.py", "lonely.py", doc)
    docs = [f"md::README.md::H{i}" for i in range(4)]
    for nid in docs:
        node(g, nid, "README.md", "README.md", doc, is_doc=True)
    node(g, "py::loose::L", None, "loose.py", doc)
    node(g, "py::nowhere::N", None, None, doc)
    tests = []
    for k in range(0, 12, 3):
        path = f"tests/test_g{k}.py"
        for i in range(3):
            nid = f"py::{path}::T{i}"
            node(g, nid, path, f"test_g{k}.py", doc)
            tests.append((nid, k))
    hubs = ["py::core/log.py::Logger", "py::core/cfg.py::Config", "py::core/ghost.py::Ghost"]
    for nid in hubs:
        node(g, nid, nid.split("::")[1], nid.split("::")[1].split("/")[-1], doc)

    edges: list[tuple[str, str, float]] = []
    for members in groups:
        for _ in range(3 * len(members)):
            u, v = rng.choice(members), rng.choice(members)
            edges.append((u, v, round(rng.uniform(0.2, 2.0), 6)))
        u, v = rng.choice(members), rng.choice(members)
        edges += [(u, v, 0.75), (u, v, 0.25), (v, u, 0.5)]  # parallel + opposite
        edges.append((u, u, 1.0))  # self-loop
    for k in range(12):
        for _ in range(2):
            u = rng.choice(groups[k])
            v = rng.choice(groups[(k + 1) % 12])
            edges.append((u, v, round(rng.uniform(0.1, 0.5), 6)))
    for a, b in isolated:
        edges.append((a, b, 1.0))
    for i in range(3):
        edges.append((docs[i], docs[i + 1], 0.9))
    edges.append((docs[0], rng.choice(groups[5]), 0.3))
    edges.append(("py::loose::L", rng.choice(groups[0]), 1.2))
    edges.append(("py::nowhere::N", rng.choice(groups[1]), 1.1))
    for nid, k in tests:
        edges.append((nid, rng.choice(groups[k]), 0.8))
        edges.append((nid, rng.choice(groups[(k + 6) % 12]), 0.6))
    every = [n for members in groups for n in members]
    for _ in range(40):
        edges.append((rng.choice(every), hubs[0], 0.4))
    edges.append((hubs[0], rng.choice(groups[2]), 0.4))
    edges.append((hubs[0], rng.choice(groups[7]), 0.4))
    for _ in range(25):
        edges.append((rng.choice(every), hubs[1], 0.4))
    rng.shuffle(edges)
    for u, v, w in edges:
        g.add_edge(u, v, weight=w)
    return g, doc, set(hubs)


def build_all_isolated(rng: random.Random) -> tuple[nx.MultiDiGraph, dict, set]:
    g = nx.MultiDiGraph()
    doc: dict = {}
    for m in range(5):
        path = f"pkg{m % 2}/m{m}.py"
        ids = [f"py::{path}::X{i}" for i in range(m % 3 + 1)]
        for nid in ids:
            node(g, nid, path, f"m{m}.py", doc)
        if m % 2 == 0:
            for i in range(len(ids) - 1):
                g.add_edge(ids[i], ids[i + 1], weight=1.0)
    node(g, "py::pkg9/two.py::P", "pkg9/two.py", "two.py", doc)
    node(g, "py::pkg9/two.py::Q", "pkg9/two.py", "two.py", doc)
    node(g, "py::hub.py::H", "hub.py", "hub.py", doc)
    g.add_edge("py::pkg0/m0.py::X0", "py::hub.py::H", weight=0.5)
    return g, doc, {"py::hub.py::H"}


def run(name: str, graph: nx.MultiDiGraph, doc: dict, hubs: set, flags, out_root: Path) -> None:
    from elitea_deepwiki.engine import graph_clustering as gc  # noqa: PLC0415

    calls, captured = install_recorders(gc)
    nodes, edges = graph_rows(graph, doc)
    stats = gc.run_phase3(StubDB(), graph, hubs=set(hubs), feature_flags=flags)
    write_dump(
        out_root / name,
        nodes=nodes,
        edges=edges,
        calls=calls,
        assignments=assignment_rows(graph, captured),
        hubs={"phase3_hubs": sorted(hubs), "phase2_hub_count": len(hubs)},
        summary={
            "repo": f"synthetic:{name}",
            "nodes": len(nodes),
            "edges": len(edges),
            "leiden_calls": len(calls),
            "phase3_stats": stats,
            "flags": {
                "exclude_tests": flags.exclude_tests,
                "weight_calibration_profile": flags.weight_calibration_profile,
            },
        },
    )
    print(name, json.dumps({"nodes": len(nodes), "edges": len(edges), "calls": len(calls),
                            "sections": stats["macro"], "pages": stats["micro"]}))


def main() -> int:
    if len(sys.argv) != 2:
        print(__doc__, file=sys.stderr)
        return 2
    out_root = Path(sys.argv[1]).resolve()
    from elitea_deepwiki.engine import graph_clustering as gc  # noqa: PLC0415
    from elitea_deepwiki.engine.feature_flags import FeatureFlags  # noqa: PLC0415

    originals = (gc._nx_to_igraph, gc._contract_to_file_graph, gc._to_weighted_undirected,
                 gc.leidenalg.find_partition, gc.persist_clusters)

    def restore() -> None:
        (gc._nx_to_igraph, gc._contract_to_file_graph, gc._to_weighted_undirected,
         gc.leidenalg.find_partition, gc.persist_clusters) = originals

    graph, doc, hubs = build_default(random.Random(7))
    run("default", graph, doc, hubs, FeatureFlags(), out_root)
    restore()
    graph, doc, hubs = build_default(random.Random(7))
    run("exclude_tests_legacy", graph, doc, hubs,
        FeatureFlags(exclude_tests=True, weight_calibration_profile="legacy"), out_root)
    restore()
    graph, doc, hubs = build_all_isolated(random.Random(3))
    run("all_isolated", graph, doc, hubs, FeatureFlags(), out_root)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
