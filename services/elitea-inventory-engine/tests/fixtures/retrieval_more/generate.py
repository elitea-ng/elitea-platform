"""Regenerate graph.json and goldens.json for tests/retrieval_more.rs with the
Python engine's own code.

Run from the repository root:

    uv run --python 3.13 --with "networkx>=3.5,<4" --with numpy --with pydantic \\
        python services/elitea-inventory-engine/tests/fixtures/retrieval_more/generate.py

What runs is the real Python: `KnowledgeGraph` (query_pattern,
get_pattern_vocabulary, semantic_search, search, the community accessors),
the `InventoryRetrievalApiWrapper` methods the chat tools call (bound to a
stand-in `self` holding the loaded graph), and the `tool_operations` handlers
for presets, cache and maintenance (their function bodies executed from the
source with `ast`, bound to a stand-in `self`). The graph every case reads is
the one `dump_to_json` wrote and `load_from_json` read back, as production
read it.

Two things are arranged, and only these:

* ORDER. Python's pattern engine collects start nodes in `set`s, whose order
  follows the per-process string hash, so the same query lists its paths in a
  different order from run to run (and, at the result cap, keeps different
  paths). The Rust engine walks them in node order (seeds: chosen in edge
  order, walked in node order). Here `_resolve_pattern_nodes` and
  `_seed_from_edges` are wrapped to return their very sets as lists in node
  order, so the goldens are the one permutation the Rust engine produces.
* THE CHAT CLOSURES. `query_pattern` / `get_pattern_vocabulary` are closures
  inside `_build_chat_tools`, which needs the whole chat stack. Their text is
  the wrapper method's, transcribed differences being: the closure strips the
  pattern and answers PATTERN_SYNTAX_HELP for an empty one, and its
  vocabulary has no "Syntax Reference" section. Both are applied here around
  the real wrapper methods.
"""

import ast
import importlib
import json
import os
import sys
import tempfile
import types

import pydantic

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = os.path.join(HERE, "../../../../elitea-inventory/src/elitea_inventory")

for name, path in [
    ("elitea_inventory", SRC),
    ("elitea_inventory.engine", SRC + "/engine"),
    ("elitea_inventory.engine.inventory", SRC + "/engine/inventory"),
]:
    module = types.ModuleType(name)
    module.__path__ = [path]
    sys.modules[name] = module

# retrieval.py's base class pulls langchain; the methods called here use none
# of it.
base = types.ModuleType("elitea_inventory.engine.inventory.elitea_base")


class BaseToolApiWrapper(pydantic.BaseModel):
    pass


base.BaseToolApiWrapper = BaseToolApiWrapper
sys.modules[base.__name__] = base

kg_module = importlib.import_module("elitea_inventory.engine.inventory.knowledge_graph")
retrieval = importlib.import_module("elitea_inventory.engine.inventory.retrieval")
presets = importlib.import_module("elitea_inventory.engine.inventory.presets")
importlib.import_module("elitea_inventory.engine.inventory.communities")
Wrapper = retrieval.InventoryRetrievalApiWrapper
KG = kg_module.KnowledgeGraph

# `from .engine import list_presets, PRESETS` inside the preset handlers.
engine_pkg = sys.modules["elitea_inventory.engine"]
engine_pkg.list_presets = presets.list_presets
engine_pkg.get_preset = presets.get_preset
engine_pkg.PRESETS = presets.PRESETS

# ---------------------------------------------------------------- the graph

ENTITIES = [
    ("n1", "UserService", "class", ("src/user_service.py", 10, 80, "code"),
     {"description": "Manages users and their sessions", "signature": "class UserService(BaseService)"}),
    ("n2", "BaseService", "class", ("src/base.py", 1, 40, "code"), {"description": "Base class for services"}),
    ("n3", "AuthService", "class", ("src/auth.py", 1, 90, "code"), {"description": "Authentication logic"}),
    ("n4", "login", "function", ("src/auth.py", 5, 20, "code"), {"description": "Logs a user in"}),
    ("n5", "hash_password", "function", ("src/crypto.py", 3, None, "code"), None),
    ("n6", "get_user", "method", ("src/user_service.py", 30, 45, "code"), None),
    ("n7", "UserRepository", "class", ("src/repo.py", 1, 200, "code"),
     {"description": "Stores users in the database and keeps a cache of the most recently read rows so repeated reads are cheap and consistent."}),
    ("n8", "db_url", "variable", ("src/config.py", 2, 2, "code"), None),
    ("n9", "os", "import", ("src/auth.py", 1, 1, "code"), None),
    ("n10", "User Login", "feature", ("docs/features.md", 1, 30, "docs"), {"description": "Users sign in with a password"}),
    ("n11", "Secure Passwords", "requirement", ("docs/req.md", 4, 9, "docs"), None),
    ("n12", "Login Story", "user_story", ("docs/stories.md", None, None, "docs"), None),
    ("n13", "Auth Guide", "documentation", ("docs/guide.md", 1, 99, "docs"), {"description": "How sign-in works"}),
    ("n14", "Passwords are hashed", "fact", ("docs/req.md", 10, 10, "docs"), None),
    ("n15", "login", "method", ("src/web/views.py", 12, 30, "code"), {"description": "View that logs the user in"}),
    ("n16", "utils", "module", ("src/utils.py", 1, 5, "code"), None),
    ("n17", "SessionStore", "class", ("src/session.py", 1, 50, "code"), None),
]

RELATIONS = [
    ("n1", "n2", "extends"), ("n3", "n2", "extends"), ("n17", "n2", "extends"),
    ("n1", "n3", "calls"), ("n3", "n4", "calls"), ("n4", "n5", "calls"),
    ("n5", "n4", "calls"), ("n1", "n6", "contains"), ("n6", "n7", "calls"),
    ("n7", "n8", "imports"), ("n3", "n9", "imports"), ("n4", "n9", "imports"),
    ("n10", "n11", "implements"), ("n11", "n3", "related_to"), ("n12", "n10", "related_to"),
    ("n13", "n4", "documents"), ("n13", "n10", "mentions"), ("n15", "n4", "calls"),
    ("n6", "n4", "calls"), ("n14", "n11", "related_to"), ("n11", "n17", "related_to"),
    ("n1", "n17", "uses"),
]

EMBEDDINGS = {
    "n1": [0.1, 0.9, 0.0, 0.1], "n2": [0.2, 0.5, 0.1, 0.0], "n3": [0.9, 0.1, 0.0, 0.2],
    "n4": [0.8, 0.3, 0.1, 0.0], "n5": [0.7, 0.0, 0.3, 0.1], "n6": [0.0, 0.8, 0.2, 0.0],
    "n7": [0.05, 0.7, 0.6, 0.0], "n8": [0.0, 0.0, 1.0, 0.0], "n10": [0.6, 0.6, 0.0, 0.3],
    "n11": [0.5, 0.0, 0.0, 0.8], "n13": [0.75, 0.25, 0.0, 0.25], "n14": [0.0, 0.0, 0.0, 0.0],
    "n15": [0.8, 0.3, 0.1, 0.0], "n17": [0.1, 0.6, 0.2, 0.123456],
}

COMMUNITIES = {
    "algorithm": "leiden",
    "resolution": 1.0,
    "modularity": 0.41235,
    "num_communities": 4,
    "communities": {
        "community_0": {
            "members": ["n1", "n2", "n3", "n4", "n5", "n6", "n7", "n8", "n9", "n15", "n17"],
            "centroids": [
                {"id": "n1", "name": "UserService", "type": "class", "score": 1.0},
                {"id": "n3", "name": "AuthService", "type": "class", "score": 0.81235},
                {"id": "n4", "name": "login", "type": "function", "score": 0.5},
                {"id": "n2", "name": "BaseService", "type": "class", "score": 0.4999},
            ],
            "stats": {"size": 11, "density": 0.18181, "cohesion": 0.75, "internal_edges": 14},
            "label": "User services",
            "summary": "Code that manages users and signs them in.",
            "dominant_types": ["class", "function", "method", "variable"],
            "dominant_layers": ["code"],
            "micro_clusters": None,
        },
        "community_1": {
            "members": ["n10", "n11", "n12", "n13", "n14"],
            "centroids": [{"id": "n10", "name": "User Login", "type": "feature", "score": 1.0}],
            "stats": {"size": 5, "density": 0.25, "cohesion": 0.5, "internal_edges": 4},
            "label": "Login docs",
            "summary": None,
            "dominant_types": ["feature"],
            "dominant_layers": ["product", "documentation"],
            "micro_clusters": {"micro_0": {"label": "Passwords", "members": ["n11", "n14"]}, "micro_1": {"members": ["n10"]}},
        },
        "community_2": {"members": ["n16"], "centroids": []},
        "community_3": {"members": [], "stats": {"size": 5}, "label": "Empty", "centroids": []},
    },
}

kg = KG()
for eid, name, etype, (path, start, end, toolkit), properties in ENTITIES:
    assert kg_module._normalize_entity_type(etype) == etype, etype
    citation = kg_module.Citation(file_path=path, line_start=start, line_end=end, source_toolkit=toolkit, doc_id=f"{toolkit}:{path}")
    kg.add_entity(eid, name, etype, citation, properties)
for source, target, kind in RELATIONS:
    kg.add_relation(source, target, kind)
for eid, vector in EMBEDDINGS.items():
    kg._graph.nodes[eid]["embedding"] = vector
kg._metadata["embeddings_model"] = "text-embed-x"
kg._metadata["embeddings_dimension"] = 4
kg.set_community_data(COMMUNITIES)
GRAPH_PATH = os.path.join(HERE, "graph.json")
kg.dump_to_json(GRAPH_PATH)

# A second graph: no embeddings, no communities.
bare = KG()
bare.add_entity("b1", "Lonely", "class", None, None)
BARE_PATH = os.path.join(HERE, "bare.json")
bare.dump_to_json(BARE_PATH)

# dump_to_json stamps the time and lists the index sets in hash order; pin
# both so a re-run writes the same files (loading reads the indices back
# into sets, so their order is no input).
for path in (GRAPH_PATH, BARE_PATH):
    with open(path, encoding="utf-8") as handle:
        document = json.load(handle)
    document["_metadata"]["last_saved"] = "2026-10-07T00:00:00"
    for index in document.get("_indices", {}).values():
        for key, ids in index.items():
            index[key] = sorted(ids)
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(document, handle, indent=2)


def load(path):
    graph = KG()
    graph.load_from_json(path)
    position = {node: index for index, node in enumerate(graph._graph.nodes())}
    resolve = graph._resolve_pattern_nodes
    seed = graph._seed_from_edges

    def ordered(found):
        return None if found is None else sorted(found, key=position.__getitem__)

    graph._resolve_pattern_nodes = lambda spec: ordered(resolve(spec))
    graph._seed_from_edges = lambda rel_types, neighbor_idx: ordered(seed(rel_types, neighbor_idx))
    return graph


class FakeEmbeddings:
    def __init__(self, vectors):
        self.vectors = vectors

    def embed_query(self, query):
        return self.vectors[query]


def wrapper_self(graph, vectors=None):
    return types.SimpleNamespace(
        _knowledge_graph=graph,
        _log_tool_event=lambda *args, **kwargs: None,
        _get_embedding_model=lambda: FakeEmbeddings(vectors or {}),
    )


graph = load(GRAPH_PATH)
bare_graph = load(BARE_PATH)
me = wrapper_self(graph)
bare_me = wrapper_self(bare_graph)

# --------------------------------------------------------------- patterns


def chat_query_pattern(target, pattern_input):
    pattern = pattern_input.strip()
    if not pattern:
        return target._knowledge_graph.PATTERN_SYNTAX_HELP
    return Wrapper.query_pattern(target, pattern, max_results=50)


SYNTAX_REFERENCE = "\n## Syntax Reference\n"


def chat_vocabulary(target):
    text = Wrapper.get_pattern_vocabulary(target)
    cut = text.index(SYNTAX_REFERENCE)
    return text[:cut]


PATTERNS = [
    "(UserService)-[:calls*1..3]->(?)",
    "(?:class)-[:extends]->(BaseService)",
    "(login)<-[:calls*1..2]-(?)",
    "(?)-[:calls]->(?)",
    "(?)<-[:inherits]-(?)",
    "(?:feature)-[:implements]->(?:requirement)-[:related_to]->(?:class)",
    "(?:user_story)-[:related]->(?:feature)-[:implements]->(?:requirement)-[:related_to]->(?:class)",
    "(?:user_story)-[:related]->(?:feature)-[:implements]->(?:requirement)-[:related_to]->(?:class)-[:calls]->(?)",
    "(UserServ)-[:calls]->(?)",
    "(login:method)-[:calls]->(?)",
    "(login:class)-[:calls]->(?)",
    "(?:code)-[:imports]->(?)",
    "(Zzyzx)-[:calls]->(?)",
    "(A)-[:calls*0..2]->(B)",
    "(A)-[:calls*1..9]->(B)",
    "(A)-[:calls*3..2]->(B)",
    "(A)-[:calls*x]->(B)",
    "(A)-[:calls*]->(B)",
    "(A)-[:calls* 1 .. 2 ]->(B)",
    "(A)-[:calls*+2]->(B)",
    "(A)-[:calls*1_0]->(B)",
    "(A)-[:calls]-(B)",
    "garbage",
    "(UserService)-[:calls]->(?) junk",
    "",
    "   ",
    "  (?)-[:*1..2]->(?:function)  ",
    "(?)-[:]->(?)",
    "(UserService)-[:calls,contains*1..2]->(?)",
    "( ?:class )-[: EXTENDS ]->( ? )",
    "(?)-[:*1..5]->(?)",
    "(?)-[:calls*1..5]->(?)",
    "(?:class)-[:extends]->(?)<-[:extends]-(?:class)",
    "(?:class)-[:extends]->(?:nothing)-[:calls]->(?)",
    "(?:)-[:calls]->(:function)",
    "(UserService)-[:uses]->(SessionStore)<-[:related_to]-(?)",
    "(?)<-[:documents]-(?:documentation)",
    "(login)-[:calls*2]->(?)",
    "(?)-[:a]->(?)-[:b]->(?)-[:c]->(?)-[:d]->(?)-[:e]->(?)",
    "(?)-[:*1..5]->(?)-[:*1..5]->(?)",
    "(?:class,function)-[:calls*1..2]->(?)",
    "(UserService)<-[:calls]-(?)",
]

pattern_goldens = [{"pattern": p, "text": chat_query_pattern(me, p)} for p in PATTERNS]

# --------------------------------------------------------------- semantic

VECTORS = {
    "auth": [1.0, 0.0, 0.0, 0.0],
    "users": [0.0, 1.0, 0.0, 0.0],
    "mixed": [0.3, 0.4, 0.5, 0.6],
    "zero": [0.0, 0.0, 0.0, 0.0],
    "short": [1.0, 0.0, 0.0],
}
SEMANTIC = [
    {"query": "auth"},
    {"query": "users", "top_k": 3},
    {"query": "mixed", "min_score": 0.0},
    {"query": "mixed", "entity_type": "CLASS"},
    {"query": "auth", "layer": "code"},
    {"query": "auth", "layer": "product", "min_score": 0.1},
    {"query": "auth", "file_pattern": "*auth*"},
    {"query": "users", "file_pattern": "src/user?service.py", "min_score": 0.0},
    {"query": "auth", "min_score": 0.99},
    {"query": "zero"},
    {"query": "short"},
]
semantic_goldens = []
semantic_me = wrapper_self(graph, VECTORS)
for case in SEMANTIC:
    kwargs = {k: v for k, v in case.items()}
    semantic_goldens.append(
        dict(case, vector=VECTORS[case["query"]], text=Wrapper.semantic_search(semantic_me, **kwargs))
    )
semantic_unavailable = Wrapper.semantic_search(wrapper_self(bare_graph, VECTORS), query="auth")

# ------------------------------------------------------------- communities

community_goldens = {
    "list": [
        {"top_n": None, "text": Wrapper.list_communities(me)},
        {"top_n": 2, "text": Wrapper.list_communities(me, top_n=2)},
        {"top_n": None, "bare": True, "text": Wrapper.list_communities(bare_me)},
    ],
    "detail": [
        {"community_id": cid, "text": Wrapper.get_community_detail(me, cid)}
        for cid in ["community_0", "community_1", "community_2", "community_3", "community_9"]
    ]
    + [{"community_id": "community_0", "bare": True, "text": Wrapper.get_community_detail(bare_me, "community_0")}],
    "find": [
        {"entity_name": name, "text": Wrapper.find_entity_community(me, name)}
        for name in ["login", "UserService", "user", "Zzyzx", "Lonely"]
    ],
    "search": [
        {"community_id": cid, "query": query, "text": Wrapper.search_within_community(me, cid, query)}
        for cid, query in [
            ("community_0", "service"),
            ("community_0", "user"),
            ("community_1", "login"),
            ("community_1", "password"),
            ("community_3", "x"),
            ("community_9", "x"),
            ("community_0", "zzyzx"),
        ]
    ],
}

# --------------------------------------------------------------- admin tools

TOOL_OPS = os.path.join(SRC, "tool_operations.py")
with open(TOOL_OPS, encoding="utf-8") as handle:
    tool_tree = ast.parse(handle.read())
tool_class = next(n for n in tool_tree.body if isinstance(n, ast.ClassDef) and any(
    isinstance(f, ast.FunctionDef) and f.name == "_tool_list_presets" for f in n.body))
constants_path = os.path.join(SRC, "engine/constants.py")
with open(constants_path, encoding="utf-8") as handle:
    constants_tree = ast.parse(handle.read())
namespace = {"__name__": "elitea_inventory.tool_operations", "__package__": "elitea_inventory",
             "log": types.SimpleNamespace(info=print, error=print, warning=print)}
for node in constants_tree.body:
    if isinstance(node, ast.Assign) and any(isinstance(t, ast.Name) and t.id == "CANONICAL_TYPES" for t in node.targets):
        exec(compile(ast.Module([node], []), constants_path, "exec"), namespace)
for node in tool_class.body:
    if isinstance(node, ast.FunctionDef) and node.name in {
        "_tool_list_presets", "_tool_get_preset_info", "_tool_cleanup_cache",
        "_tool_normalize_types", "_tool_rebuild_indices", "_tool_smart_normalize_types",
    }:
        exec(compile(ast.Module([node], []), TOOL_OPS, "exec"), namespace)


class FakeCache:
    def cleanup_stale_graphs(self):
        return {"removed": 0, "freed_bytes": 0}


def tool_self(path):
    graph_for_tool = load(path)
    return types.SimpleNamespace(
        _get_or_create_wrapper=lambda graph_path, request_data=None: types.SimpleNamespace(_knowledge_graph=graph_for_tool),
        cache_manager=FakeCache(),
        invocation_thinking=lambda message: None,
    )


def run_tool(name, params, path=GRAPH_PATH):
    scratch = os.path.join(tempfile.mkdtemp(), "graph.json")
    try:
        return {"ok": namespace[name](tool_self(path), params, scratch, {})}
    except Exception as error:  # noqa: BLE001 - the error is the golden
        return {"error_type": type(error).__name__, "message": str(error)}


def preset_info_with_patterns(params):
    """get_preset_info as it meant to read: the presets carry their patterns
    as whitelist/blacklist, the handler reads include/exclude_patterns."""
    original = engine_pkg.get_preset

    def with_patterns(name):
        preset = original(name)
        return dict(preset, include_patterns=preset["whitelist"], exclude_patterns=preset["blacklist"])

    engine_pkg.get_preset = with_patterns
    try:
        return run_tool("_tool_get_preset_info", params)
    finally:
        engine_pkg.get_preset = original


admin_goldens = {
    "list_presets": run_tool("_tool_list_presets", {}),
    "get_preset_info": [
        {"params": params, "python": run_tool("_tool_get_preset_info", params), "intended": preset_info_with_patterns(params)}
        for params in [{"preset_name": "python"}, {"preset_name": "ts"}, {"preset_name": "nope"}, {}]
    ],
    "cleanup_cache": [
        {"params": params, "result": run_tool("_tool_cleanup_cache", params)}
        for params in [{}, {"output_format": "json"}, {"output_format": "text"}]
    ],
    "normalize_types": [
        {"params": params, "result": run_tool("_tool_normalize_types", params)}
        for params in [{}, {"output_format": "text"}, {"smart": False}, {"smart_threshold": 100}]
    ],
    "rebuild_indices": [
        {"params": params, "result": run_tool("_tool_rebuild_indices", params)}
        for params in [{}, {"output_format": "text"}]
    ],
    "smart_normalize_types": [
        {"params": params, "result": run_tool("_tool_smart_normalize_types", params)}
        for params in [{"threshold": 1}, {"threshold": "1", "output_format": "text"}, {"threshold": 2}]
    ],
}

goldens = {
    "_comment": "Generated by generate.py with the Python Inventory engine. Do not edit by hand.",
    "patterns": pattern_goldens,
    "vocabulary": chat_vocabulary(me),
    "vocabulary_bare": chat_vocabulary(bare_me),
    "semantic": semantic_goldens,
    "semantic_unavailable": semantic_unavailable,
    "communities": community_goldens,
    "admin": admin_goldens,
}
with open(os.path.join(HERE, "goldens.json"), "w", encoding="utf-8") as handle:
    json.dump(goldens, handle, indent=1, ensure_ascii=False)
    handle.write("\n")
