"""Regenerate goldens.json for tests/smart_normalize.rs with the Python engine's
own `_tool_smart_normalize_types`.

Run from the repository root:

    uv run --python 3.13 --with "networkx>=3.5,<4" --with pydantic --with "langchain-core>=0.3,<2" \\
        python services/elitea-inventory-engine/tests/fixtures/smart_normalize/generate.py

What runs is the real Python: the graph is built with `KnowledgeGraph`,
saved with `dump_to_json` (`graph.json`, the input every case reads) and
loaded back with `load_from_json`; the handler's function body is executed
from `tool_operations.py` with `ast` and bound to a stand-in `self` whose
`platform_llm` is a canned model. That model records

* every prompt the handler sends (`prompts`),
* the structured-output tool LangChain derives from the handler's pydantic
  `TypeMappingResponse` (`tool`: `convert_to_openai_tool`, what a
  function-calling `with_structured_output` binds), and

answers each batch with the reply listed in the case (`replies`), validated
through that same pydantic model. The golden keeps the handler's answer and
the node types of the graph it saved (`types_after`, node id -> type).

A node's type is set on the networkx node directly where it must be one that
`add_entity` would have normalised away: a graph imported from an older
engine (or edited) carries such types, which is what the tool is for.
"""

import ast
import importlib
import json
import os
import sys
import tempfile
import types

from langchain_core.utils.function_calling import convert_to_openai_tool

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

kg_module = importlib.import_module("elitea_inventory.engine.inventory.knowledge_graph")

# (id, name, add_entity type, raw type set afterwards or None)
NODES = [
    ("n1", "UserService", "class", None),
    ("n2", "login", "function", None),
    ("n3", "Checkout", "feature", "Feature"),
    ("n4", "Cart", "feature", "Feature"),
    ("n5", "Login screen", "concept", "ui_screen"),
    ("n6", "Order total must be positive", "rule", "business_rule"),
    ("n7", "Retry policy", "rule", "retry_policy"),
    ("n8", "Gizmo", "concept", "widget"),
    ("n9", "README", "documentation", None),
    ("n10", "Payment flow", "process", "payment_flow_step"),
    ("n11", "Café Überblick", "concept", "überblick_seite"),
]
RELATIONS = [("n1", "n2", "contains"), ("n3", "n4", "relates_to"), ("n5", "n2", "calls")]


def build(path):
    graph = kg_module.KnowledgeGraph()
    for node_id, name, kind, _ in NODES:
        citation = kg_module.Citation(file_path=f"src/{node_id}.py", line_start=1, line_end=2,
                                      source_toolkit="repo", doc_id=f"repo:src/{node_id}.py")
        graph.add_entity(node_id, name, kind, citation, None)
    for source, target, kind in RELATIONS:
        graph.add_relation(source, target, kind)
    for node_id, _, _, raw in NODES:
        if raw is not None:
            graph._graph.nodes[node_id]["type"] = raw
    graph.dump_to_json(path)


GRAPH_DIR = tempfile.mkdtemp()
GRAPH_PATH = os.path.join(GRAPH_DIR, "graph.json")
build(GRAPH_PATH)
with open(GRAPH_PATH, encoding="utf-8") as handle:
    graph_document = json.load(handle)
# The stamp is the clock's; the test reads the graph, not the stamp.
graph_document["_metadata"]["last_saved"] = "2026-10-09T00:00:00.000000"
with open(os.path.join(HERE, "graph.json"), "w", encoding="utf-8") as handle:
    json.dump(graph_document, handle, indent=2)
    handle.write("\n")

# ------------------------------------------------------------- the handler

TOOL_OPS = os.path.join(SRC, "tool_operations.py")
with open(TOOL_OPS, encoding="utf-8") as handle:
    tool_tree = ast.parse(handle.read())
tool_class = next(n for n in tool_tree.body if isinstance(n, ast.ClassDef) and any(
    isinstance(f, ast.FunctionDef) and f.name == "_tool_smart_normalize_types" for f in n.body))
constants_path = os.path.join(SRC, "engine/constants.py")
with open(constants_path, encoding="utf-8") as handle:
    constants_tree = ast.parse(handle.read())
namespace = {"__name__": "elitea_inventory.tool_operations", "__package__": "elitea_inventory",
             "log": types.SimpleNamespace(info=print, error=print, warning=lambda *a, **k: None)}
for node in constants_tree.body:
    if isinstance(node, ast.Assign) and any(isinstance(t, ast.Name) and t.id == "CANONICAL_TYPES" for t in node.targets):
        exec(compile(ast.Module([node], []), constants_path, "exec"), namespace)
for node in tool_class.body:
    if isinstance(node, ast.FunctionDef) and node.name == "_tool_smart_normalize_types":
        exec(compile(ast.Module([node], []), TOOL_OPS, "exec"), namespace)
handler = namespace["_tool_smart_normalize_types"]


class CannedModel:
    def __init__(self, replies):
        self.replies = list(replies)
        self.prompts = []
        self.tool = None
        self.model_config = None

    def with_structured_output(self, schema):
        self.tool = convert_to_openai_tool(schema)
        model = self

        class Structured:
            def invoke(self, prompt):
                model.prompts.append(prompt)
                reply = model.replies.pop(0)
                if reply is None:
                    raise ValueError("the model answered something that is not a TypeMappingResponse")
                return schema.model_validate(reply)

        return Structured()


def run(params, replies, model_name="mapper-model"):
    graph = kg_module.KnowledgeGraph()
    graph.load_from_json(GRAPH_PATH)
    model = CannedModel(replies)

    def platform_llm(model_name, model_config):
        model.model_config = {"model_name": model_name, **model_config}
        return model

    me = types.SimpleNamespace(
        _get_or_create_wrapper=lambda graph_path, request_data=None: types.SimpleNamespace(_knowledge_graph=graph),
        invocation_thinking=lambda message: None,
        invocation_stop_checkpoint=lambda: None,
        _get_elitea_client=lambda project_id: object(),
        platform_llm=platform_llm,
    )
    scratch = os.path.join(tempfile.mkdtemp(), "graph.json")
    request_data = {"configuration": {"project_id": 1, "settings": {"llm_model": model_name}}}
    result = handler(me, params, scratch, request_data)
    saved = None
    if os.path.exists(scratch):
        with open(scratch, encoding="utf-8") as handle:
            saved = {node["id"]: node.get("type") for node in json.load(handle)["nodes"]}
    return {
        "params": params,
        "replies": replies,
        "prompts": model.prompts,
        "model_config": model.model_config,
        "result": result,
        "types_after": saved,
        "tool": model.tool,
    }


def reply(*pairs):
    return {"mappings": [{"original": o, "canonical": c, "confidence": 0.9} for o, c in pairs]}


BATCH_1 = reply(("Feature", "feature"), ("ui_screen", "component"), ("business_rule", "rule"))
BATCH_2 = reply(("retry_policy", "rule"), ("widget", "gadget"), ("payment_flow_step", "step"))
BATCH_3 = reply(("überblick_seite", "documentation"))
ONE_BATCH = {"mappings": BATCH_1["mappings"] + BATCH_2["mappings"] + BATCH_3["mappings"]}

cases = [
    run({}, [ONE_BATCH]),
    run({"batch_size": 3}, [BATCH_1, BATCH_2, BATCH_3]),
    run({"batch_size": "3", "dry_run": "true"}, [BATCH_1, BATCH_2, BATCH_3]),
    run({"output_format": "text", "dry_run": True}, [ONE_BATCH]),
    run({"output_format": "text"}, [ONE_BATCH]),
    # `Feature` (2 entities) is above the threshold; the rest qualify.
    run({"threshold": 2}, [reply(("ui_screen", "component"), ("business_rule", "rule"), ("retry_policy", "rule"),
                                ("widget", "fact"), ("payment_flow_step", "step"), ("überblick_seite", "fact"))]),
    # A batch the model could not answer: Python maps every type of it to
    # `fact` and saves. The port refuses instead (tests/smart_normalize.rs).
    run({"batch_size": 3}, [BATCH_1, None, BATCH_3]),
]

goldens = {
    "_comment": "Generated by generate.py with the Python Inventory engine. Do not edit by hand.",
    "canonical_types": sorted(namespace["CANONICAL_TYPES"]),
    "tool": cases[0].pop("tool"),
    "cases": cases,
}
for case in cases[1:]:
    assert case.pop("tool") in (goldens["tool"], None)
with open(os.path.join(HERE, "goldens.json"), "w", encoding="utf-8") as handle:
    json.dump(goldens, handle, indent=1, ensure_ascii=False)
    handle.write("\n")
print(f"{len(cases)} cases")
