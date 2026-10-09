"""Regenerate the community fixtures from the Python engine.

Run from the elitea-platform root:

    uv run --python 3.13 --with "networkx>=3.5,<4" --with "igraph>=0.11" --with numpy --with scipy \
        python services/elitea-inventory-engine/tests/fixtures/communities/generate.py

It loads `communities.py` of the Python engine on its own (no package
import), runs `CommunityAnalyzer` with real igraph over `two_clusters.json`
and writes what it produced, plus igraph's and networkx's centralities over
`centrality.json`.

One change is made to the Python code before it runs: the label and summary
prompts walk `centroid_ids`, a `set`, whose order follows the string hash
(PYTHONHASHSEED); here it is an insertion-ordered dict, the order the Rust
port uses (centroid order).
"""

import importlib.util
import inspect
import json
import pathlib
import textwrap

import igraph as ig
import networkx as nx

HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parents[4]
SOURCE = ROOT / "services/elitea-inventory/src/elitea_inventory/engine/inventory/communities.py"

spec = importlib.util.spec_from_file_location("communities", SOURCE)
communities = importlib.util.module_from_spec(spec)
spec.loader.exec_module(communities)
Analyzer = communities.CommunityAnalyzer

for name in ("_build_label_prompt", "_build_summary_prompt"):
    code = textwrap.dedent(inspect.getsource(getattr(Analyzer, name)))
    ordered = code.replace(
        'centroid_ids = {c["id"] for c in centroids}',
        'centroid_ids = dict.fromkeys(c["id"] for c in centroids)',
    )
    assert ordered != code, name
    scope = dict(vars(communities))
    exec(ordered, scope)
    setattr(Analyzer, name, scope[name])


def load(path):
    document = json.loads(path.read_text())
    graph = nx.DiGraph()
    for node in document["nodes"]:
        graph.add_node(node["id"], **node["attributes"])
    for edge in document["edges"]:
        graph.add_edge(edge["source"], edge["target"], **edge["attributes"])
    return graph


def two_clusters():
    graph = load(HERE / "two_clusters.json")
    analyzer = Analyzer()
    data = analyzer.detect_communities(graph)

    # networkx's modularity of the same partition over the same undirected
    # multigraph, as an independent check of igraph's.
    multi = nx.MultiGraph()
    multi.add_nodes_from(graph.nodes)
    for u, v, attrs in graph.edges(data=True):
        multi.add_edge(u, v, weight=communities._get_edge_weight(attrs.get("relation_type", "")))
    parts = [set(c["members"]) for c in data["communities"].values()]
    nx_modularity = nx.community.modularity(multi, parts, weight="weight")

    detected = json.loads(json.dumps(data))
    label_prompts, summary_prompts = {}, {}

    def label_model(prompt):
        cid = next(c for c, info in data["communities"].items() if info["centroids"][0]["name"] in prompt.split("## Key Relationships")[0])
        label_prompts[cid] = prompt
        if "- AuthService (class)" in prompt:
            return '  "Authentication  &\n Token Handling."  '
        raise RuntimeError("model down")

    def summary_model(prompt):
        cid = next(c for c, info in data["communities"].items() if f"labeled '{info['label']}'" in prompt)
        summary_prompts[cid] = prompt
        return f"\n  Summary of {data['communities'][cid]['label']}.  \n"

    labels = analyzer.generate_labels(graph, data, label_model, max_workers=1)
    summaries = analyzer.generate_summaries(graph, data, summary_model, max_workers=1)
    return {
        "detected": detected,
        "networkx_modularity": nx_modularity,
        "label_prompts": label_prompts,
        "summary_prompts": summary_prompts,
        "labelled": data,
        "labels": labels,
        "summaries": summaries,
    }


def centrality():
    graph = load(HERE / "centrality.json")
    analyzer = Analyzer()
    undirected = analyzer._nx_to_igraph(graph)
    names = undirected.vs["_nx_name"]
    pagerank = undirected.pagerank(weights="weight")
    betweenness = undirected.betweenness(weights=[1.0 / w for w in undirected.es["weight"]])
    strength = undirected.strength(weights="weight")

    multi = nx.MultiGraph()
    multi.add_nodes_from(graph.nodes)
    for u, v, attrs in graph.edges(data=True):
        w = communities._get_edge_weight(attrs.get("relation_type", ""))
        multi.add_edge(u, v, weight=w, distance=1.0 / w)
    nx_pagerank = nx.pagerank(multi, weight="weight", tol=1e-14, max_iter=10000)
    nx_betweenness = nx.betweenness_centrality(multi, weight="distance", normalized=False)
    return {
        "igraph": {
            "pagerank": dict(zip(names, pagerank)),
            "betweenness": dict(zip(names, betweenness)),
            "strength": dict(zip(names, strength)),
        },
        "networkx": {
            "pagerank": nx_pagerank,
            "betweenness": nx_betweenness,
        },
        "modularity": {
            "membership": {n: i % 2 for i, n in enumerate(names)},
            "igraph": undirected.modularity([i % 2 for i in range(len(names))], weights="weight"),
            "igraph_resolution_0_5": undirected.modularity(
                [i % 2 for i in range(len(names))], weights="weight", resolution=0.5
            ),
        },
    }


(HERE / "two_clusters.expected.json").write_text(json.dumps(two_clusters(), indent=1, ensure_ascii=False) + "\n")
(HERE / "centrality.expected.json").write_text(json.dumps(centrality(), indent=1) + "\n")
