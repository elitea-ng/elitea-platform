"""Regenerate graph.json and goldens.json for tests/retrieval_tools.rs.

Run from the repository root (nothing of the engine package __init__ chain is
imported; langchain is stubbed):

    uv run --python 3.13 --with "networkx>=3.5,<4" --with pydantic \\
        python services/elitea-inventory-engine/tests/fixtures/retrieval/generate.py

1. builds a graph with the REAL `KnowledgeGraph` (add_entity / add_relation /
   set_community_data) and dumps it with `dump_to_json` -> graph.json;
2. loads it back through the REAL `InventoryRetrievalApiWrapper` and runs the
   REAL copied handlers of `tool_operations.Method` over CASES -> goldens.json.

What the generator changes in the Python it runs, and why (each one is a
documented decision of the Rust port, `src/retrieval/inventory_tools.rs`):

* ORDER. Python iterates sets of node ids and of types whose order depends on
  PYTHONHASHSEED. The port defines one order (node order; a layer's types in
  `LAYER_TYPE_MAPPING` source order; first-seen order for toolkit sets), and
  here the name/type indices, the layer mapping and the module-level `set`
  are replaced by insertion-ordered sets that follow it. Set LITERALS cannot
  be replaced (`get_entity_neighbors`); every case is therefore run under
  three hash seeds and a case whose answer differs between them is marked
  `unordered` (the test compares it order-insensitively).
* `ids` (deviation D1): a node loaded from graph.json has no `id` attribute
  (networkx drops it), so `dict(data)` rows (`list_entities_by_layer`,
  `list_entities_by_source`, `query_graph`) had none. The port appends it,
  as a live (never reloaded) Python graph had it: here every node gets `id`
  appended after load.
* `edges` (D2): `_get_edges_for_entities` returns `(edges, connected_ids)`,
  and `json.dumps` of that tuple raises (`set` is not serialisable), so the
  JSON form of impact_analysis / list_entities_by_* never answered. The port
  answers the edge list; here the helper is patched to return it.
* `citation` (D3): get_entity_content, impact_analysis (text) and
  list_entities_by_type (text) read the legacy single `citation` key, which no
  v1 graph has. The port falls back to `citations[0]`; for those cases the
  node's first citation is exposed as `citation` while the handler runs.
* `python_raised`: a case where the unpatched handler raised records the
  exception; the test asserts the port's own answer for it.
"""

import contextlib
import hashlib
import importlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import types
from collections.abc import MutableSet

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = os.path.join(HERE, "../../../../elitea-inventory/src/elitea_inventory")


class OrderedSet(MutableSet):
    """A set that iterates in insertion order (set semantics otherwise)."""

    def __init__(self, items=()):
        self._d = dict.fromkeys(items)

    def __contains__(self, item):
        return item in self._d

    def __iter__(self):
        return iter(self._d)

    def __len__(self):
        return len(self._d)

    def add(self, item):
        self._d[item] = None

    def discard(self, item):
        self._d.pop(item, None)

    def update(self, *others):
        for other in others:
            for item in other:
                self._d[item] = None

    def copy(self):
        return OrderedSet(self._d)

    def __repr__(self):
        return f"OrderedSet({list(self._d)!r})"

    __hash__ = None


def load_modules():
    for name, path in [
        ("elitea_inventory", SRC),
        ("elitea_inventory.engine", SRC + "/engine"),
        ("elitea_inventory.engine.inventory", SRC + "/engine/inventory"),
    ]:
        module = types.ModuleType(name)
        module.__path__ = [path]
        sys.modules[name] = module
    # langchain_core stubs: elitea_base imports these names only.
    lc = types.ModuleType("langchain_core")
    lc_tools = types.ModuleType("langchain_core.tools")
    lc_callbacks = types.ModuleType("langchain_core.callbacks")

    class BaseTool:  # noqa: D401 - stub
        pass

    class ToolException(Exception):
        pass

    def dispatch_custom_event(*_args, **_kwargs):
        raise RuntimeError("no callback context")

    lc_tools.BaseTool = BaseTool
    lc_tools.ToolException = ToolException
    lc_callbacks.CallbackManagerForToolRun = object
    lc_callbacks.dispatch_custom_event = dispatch_custom_event
    sys.modules["langchain_core"] = lc
    sys.modules["langchain_core.tools"] = lc_tools
    sys.modules["langchain_core.callbacks"] = lc_callbacks
    kg = importlib.import_module("elitea_inventory.engine.inventory.knowledge_graph")
    retrieval = importlib.import_module("elitea_inventory.engine.inventory.retrieval")
    tools = importlib.import_module("elitea_inventory.tool_operations")
    return kg, retrieval, tools


# The order of LAYER_TYPE_MAPPING as written in knowledge_graph.py (and in
# src/graph.rs LAYERS).
LAYER_ORDER = {
    "code": ["class", "function", "method", "module", "import", "variable", "constant", "attribute",
             "decorator", "exception", "enum", "class_reference", "class_import", "function_import",
             "function_reference", "function_call", "method_call", "test_function", "pydanticmodel"],
    "service": ["api_endpoint", "rpc_method", "route", "service", "handler", "controller",
                "middleware", "event", "sio", "rpc"],
    "data": ["model", "schema", "field", "table", "database", "migration", "entity",
             "pydantic_model", "dictionary", "list", "object"],
    "product": ["feature", "capability", "platform", "product", "application", "menu",
                "ui_element", "ui_component", "interface_element"],
    "domain": ["concept", "process", "action", "use_case", "workflow", "requirement",
               "guideline", "best_practice"],
    "documentation": ["document", "guide", "section", "subsection", "tip", "example",
                      "resource", "reference", "documentation"],
    "configuration": ["configuration", "configuration_option", "configuration_section",
                      "setting", "credential", "secret", "integration"],
    "testing": ["test", "test_case", "test_function", "fixture", "mock"],
    "tooling": ["tool", "toolkit", "command", "node_type", "node"],
    "knowledge": ["fact", "algorithm", "behavior", "validation", "dependency", "error_handling",
                  "decision", "definition", "date", "contact"],
    "structure": ["file", "source_file", "document_file", "config_file", "web_file",
                  "directory", "package"],
}


def eid(source, kind, name):
    return hashlib.md5(f"{source}:{kind}:{name}".encode()).hexdigest()[:16]


REPO = "repo"
WIKI = "wiki"

# (key, source, type, name, citations [(file, start, end)], properties)
ENTITIES = [
    ("user_service", REPO, "class", "UserService",
     [("src/services/user_service.py", 10, 80)],
     {"file_path": "src/services/user_service.py", "description": "Manages users: creation, lookup and deletion.",
      "bases": ["BaseService"], "decorators": [], "is_abstract": False}),
    ("get_user", REPO, "method", "get_user", [("src/services/user_service.py", 20, 30)],
     {"file_path": "src/services/user_service.py", "signature": "def get_user(self, user_id: int) -> User"}),
    ("create_user", REPO, "method", "create_user", [("src/services/user_service.py", 32, 55)],
     {"file_path": "src/services/user_service.py", "signature": "def create_user(self, email: str, password: str) -> User",
      "docstring": "Create a user after validating the e-mail address and hashing the password with the configured algorithm, then persist it through the repository."}),
    ("delete_user", REPO, "method", "delete_user", [("src/services/user_service.py", 57, 70)],
     {"file_path": "src/services/user_service.py"}),
    ("user_repository", REPO, "class", "UserRepository", [("src/data/user_repository.py", 5, 60)],
     {"file_path": "src/data/user_repository.py", "description": "Persists users in PostgreSQL."}),
    ("find_by_id", REPO, "method", "find_by_id", [("src/data/user_repository.py", 12, 20)],
     {"file_path": "src/data/user_repository.py"}),
    ("save", REPO, "method", "save", [("src/data/user_repository.py", 22, 40)],
     {"file_path": "src/data/user_repository.py"}),
    ("user_model", REPO, "entity", "User",
     [("src/data/models.py", 1, 25), ("src/data/schemas.py", 3, 9), ("src/api/serializers.py", 40, 52),
      ("src/data/migrations/0001_users.py", None, None), ("src/admin/views.py", 77, None)],
     {"file_path": "src/data/models.py", "fields": {"id": "int", "email": "str", "password_hash": "str"}}),
    ("account_model", REPO, "table", "Account", [("src/data/models.py", 27, 40)],
     {"file_path": "src/data/models.py"}),
    ("auth_middleware", REPO, "middleware", "AuthMiddleware", [("src/api/middleware.py", 8, 44)],
     {"file_path": "src/api/middleware.py", "description": "Checks the session token on every request."}),
    ("login_endpoint", REPO, "rest_endpoint", "POST /login", [("src/api/routes.py", 10, 25)],
     {"file_path": "src/api/routes.py", "method": "POST", "path": "/login"}),
    ("users_endpoint", REPO, "rest_endpoint", "GET /users", [("src/api/routes.py", 27, 40)],
     {"file_path": "src/api/routes.py", "method": "GET", "path": "/users"}),
    ("config_class", REPO, "class", "Config", [("src/config.py", 1, 30)],
     {"file_path": "src/config.py", "description": "Application settings read from the environment."}),
    ("load_config", REPO, "function", "load_config", [("src/config.py", 32, 50)],
     {"file_path": "src/config.py"}),
    ("database_url", REPO, "constant", "DATABASE_URL", [("src/config.py", 3, 3)],
     {"file_path": "src/config.py", "value": "postgresql://localhost/app"}),
    ("validate_email", REPO, "function", "validate_email", [("src/utils/validation.py", 4, 15)],
     {"file_path": "src/utils/validation.py"}),
    ("hash_password", REPO, "function", "hash_password", [("src/utils/security.py", 6, 18)],
     {"file_path": "src/utils/security.py", "description": "Hashes a password with bcrypt."}),
    ("test_create_user", REPO, "test_function", "test_create_user", [("tests/test_user_service.py", 10, 30)],
     {"file_path": "tests/test_user_service.py"}),
    ("user_service_file", REPO, "source_file", "user_service.py", [("src/services/user_service.py", None, None)],
     {"file_path": "src/services/user_service.py", "language": "python", "line_count": 120}),
    ("chat_handler", REPO, "class", "ChatMessageHandler", [("src/chat/handler.py", 1, 90)],
     {"file_path": "src/chat/handler.py", "description": "Routes incoming chat messages to the right conversation."}),
    ("send_message", REPO, "method", "send_message", [("src/chat/handler.py", 40, 60)],
     {"file_path": "src/chat/handler.py"}),
    ("notification_queue", REPO, "class", "NotificationQueue", [("src/chat/notifications.py", 1, 40)],
     {"file_path": "src/chat/notifications.py"}),
    ("gizmo", REPO, "widget", "Gizmo", [("src/ui/gizmo.ts", 1, 10)],
     {"file_path": "src/ui/gizmo.ts", "score": 0.75, "weight": 1e-07, "enabled": True}),
    ("legacy_helper", REPO, "function", "LegacyHelper", [], {"file_path": "src/legacy.py"}),
    # Documentation (wiki).
    ("getting_started", WIKI, "resource", "Getting Started", [("docs/getting-started.md", 1, 120)],
     {"file_path": "docs/getting-started.md"}),
    ("installation", WIKI, "section", "Installation", [("docs/getting-started.md", 10, 40)], {}),
    ("config_section", WIKI, "section", "Config", [("docs/configuration.md", 1, 80)],
     {"description": "How to configure the service: environment variables and files."}),
    ("user_management", WIKI, "documentation", "User Management", [("docs/users.md", 1, 200)],
     {"description": "Creating, updating and deleting users from the admin console."}),
    ("authentication", WIKI, "concept", "Authentication", [("docs/security.md", 1, 50)], {}),
    ("login_flow", WIKI, "process", "Login Flow", [("docs/security.md", 52, 90)],
     {"description": "A user submits credentials; the service checks them and issues a session token."}),
    ("password_policy", WIKI, "requirement", "Password Policy", [("docs/security.md", 92, 110)], {}),
    ("chat_feature", WIKI, "feature", "Chat", [("docs/chat.md", 1, 60)], {}),
    ("notifications_feature", WIKI, "feature", "Notifications", [("docs/chat.md", 62, 90)], {}),
    ("cafe", WIKI, "resource", "Café Überblick", [("docs/de/überblick.md", 1, 10)],
     {"description": "Ein Überblick — naïve façade."}),
    # Model-extracted entities: a nested `properties` dict.
    ("sso", WIKI, "feature", "Single Sign-On", [("docs/security.md", 120, 140)],
     {"properties": {"description": "Users sign in once through the corporate identity provider.",
                     "confidence": 0.9, "aliases": ["SSO", "single sign on"]}}),
    ("rate_limiting", WIKI, "concept", "Rate Limiting", [("docs/security.md", 150, 170)],
     {"properties": {"description": "Requests per client are limited to protect the service from abuse; " * 3,
                     "limit": 100, "window": "1m"}}),
    ("session_token", WIKI, "entity", "Session Token", [("docs/security.md", 60, 70)],
     {"properties": {"description": "An opaque token identifying a signed-in user."}}),
    ("admin_console", WIKI, "platform", "Admin Console", [("docs/users.md", 5, 20)], {}),
    # Facts.
    ("fact_bcrypt", REPO, "fact", "Passwords are hashed with bcrypt", [("src/utils/security.py", 6, 18)],
     {"fact_type": "behavior", "subject": "hash_password", "confidence": 0.95}),
    ("fact_email", REPO, "fact", "Emails are validated before saving", [("src/utils/validation.py", 4, 15)],
     {"subject": "validate_email"}),
    ("fact_postgres", WIKI, "fact", "Use PostgreSQL for persistence", [("docs/architecture.md", 10, 12)],
     {"rationale": "Relational data and transactions."}),
    ("fact_retry", WIKI, "fact", "Login is retried three times", [("docs/security.md", 95, 96)], {}),
    ("fact_release", WIKI, "fact", "Release 2026-01-15", [("docs/changelog.md", 1, 2)], {}),
    ("chat_config", REPO, "configuration", "chat.max_length", [("config/chat.yaml", 3, 3)],
     {"file_path": "config/chat.yaml", "default": 4000}),
]

# (source key, target key, type, properties)
RELATIONS = [
    ("user_service", "get_user", "CONTAINS", {"source_toolkit": REPO, "discovered_in_file": "src/services/user_service.py"}),
    ("user_service", "create_user", "CONTAINS", {"source_toolkit": REPO}),
    ("user_service", "delete_user", "CONTAINS", {"source_toolkit": REPO}),
    ("user_service", "user_repository", "USES", {"source_toolkit": REPO}),
    ("user_service", "validate_email", "USES", {"source_toolkit": REPO}),
    ("user_service", "hash_password", "uses", {"source_toolkit": REPO}),
    ("user_service", "config_class", "uses", {}),
    ("get_user", "find_by_id", "calls", {"source_toolkit": REPO}),
    ("create_user", "hash_password", "calls", {"source_toolkit": REPO}),
    ("create_user", "save", "calls", {"source_toolkit": REPO}),
    ("create_user", "user_model", "returns", {}),
    ("user_repository", "find_by_id", "contains", {"source_toolkit": REPO}),
    ("user_repository", "save", "contains", {"source_toolkit": REPO}),
    ("user_repository", "user_model", "uses", {"source_toolkit": REPO}),
    ("user_repository", "database_url", "uses", {}),
    ("account_model", "user_model", "references", {}),
    ("auth_middleware", "user_service", "depends_on", {"source_toolkit": REPO}),
    ("auth_middleware", "session_token", "validates", {"source": "llm", "confidence": 0.8}),
    ("login_endpoint", "auth_middleware", "uses", {"source_toolkit": REPO}),
    ("login_endpoint", "login_flow", "implements", {"source": "llm", "confidence": 0.7}),
    ("users_endpoint", "user_service", "calls", {"source_toolkit": REPO}),
    ("load_config", "config_class", "returns", {}),
    ("config_class", "database_url", "contains", {}),
    ("test_create_user", "user_service", "tests", {"source_toolkit": REPO}),
    ("test_create_user", "create_user", "tests", {}),
    ("user_service_file", "user_service", "defines", {}),
    ("chat_handler", "send_message", "contains", {}),
    ("send_message", "notification_queue", "uses", {}),
    ("chat_handler", "chat_config", "reads", {}),
    ("user_management", "user_service", "describes", {"source_toolkit": WIKI}),
    ("user_management", "admin_console", "describes", {"source_toolkit": WIKI}),
    ("admin_console", "user_service", "uses", {}),
    ("sso", "user_service", "relates_to", {"source": "llm"}),
    ("sso", "authentication", "part_of", {}),
    ("login_flow", "authentication", "part_of", {"source_toolkit": WIKI}),
    ("login_flow", "session_token", "produces", {}),
    ("password_policy", "hash_password", "enforced_by", {}),
    ("config_section", "config_class", "documents", {"source_toolkit": WIKI}),
    ("getting_started", "installation", "contains", {"source_toolkit": WIKI}),
    ("getting_started", "config_section", "references", {}),
    ("chat_feature", "chat_handler", "implemented_by", {}),
    ("notifications_feature", "notification_queue", "implemented_by", {}),
    ("chat_feature", "notifications_feature", "relates_to", {}),
    ("fact_bcrypt", "hash_password", "describes", {}),
    ("fact_email", "validate_email", "describes", {}),
    ("fact_postgres", "user_repository", "describes", {}),
    ("fact_retry", "login_flow", "describes", {}),
    ("rate_limiting", "auth_middleware", "applies_to", {}),
    ("save", "user_model", "writes", {}),
    ("save", "create_user", "called_by", {}),
]


def build_graph(kg_module):
    graph = kg_module.KnowledgeGraph()
    ids = {}
    for key, source, kind, name, citations, properties in ENTITIES:
        normalized = kg_module._normalize_entity_type(kind)
        assert normalized == kind, f"{kind!r} is not canonical ({normalized!r})"
        entity_id = eid(source, kind, name)
        ids[key] = entity_id
        first = citations[0] if citations else None
        citation = None
        if first:
            citation = kg_module.Citation(
                file_path=first[0], line_start=first[1], line_end=first[2],
                source_toolkit=source, doc_id=f"{source}:{first[0]}",
            )
        graph.add_entity(entity_id, name, kind, citation, properties or None)
        for path, start, end in citations[1:]:
            graph.add_entity(entity_id, name, kind, kg_module.Citation(
                file_path=path, line_start=start, line_end=end, source_toolkit=source,
                doc_id=f"{source}:{path}",
            ))
    # User is also cited by the wiki: an entity of two sources.
    graph.add_entity(ids["user_model"], "User", "entity", kg_module.Citation(
        file_path="docs/users.md", line_start=30, line_end=35, source_toolkit=WIKI, doc_id="wiki:docs/users.md"))
    # A legacy single `citation` (no `citations` list).
    graph._graph.nodes[ids["legacy_helper"]]["citation"] = {
        "file_path": "src/legacy.py", "line_start": 5, "line_end": 9, "source_toolkit": REPO,
        "doc_id": None, "content_hash": None,
    }
    for source, target, kind, properties in RELATIONS:
        assert graph.add_relation(ids[source], ids[target], kind, properties or None), (source, target)
    for key, vector in [("user_service", [0.1, -0.25, 0.5]), ("hash_password", [0.3, 0.2, -0.1]),
                        ("sso", [1e-05, 0.0, 1.0])]:
        graph._graph.nodes[ids[key]]["embedding"] = vector
    graph._metadata["embeddings_model"] = "text-embedding-3-small"
    graph._metadata["embeddings_count"] = 3
    graph.set_community_data({
        "communities": {
            "0": {"label": "Users", "size": 6, "members": [ids[k] for k in [
                "user_service", "get_user", "create_user", "delete_user", "user_repository", "user_model"]],
                  "summary": "User management."},
            "1": {"label": "Chat", "size": 4, "members": [ids[k] for k in [
                "chat_handler", "send_message", "notification_queue", "chat_feature"]],
                  "summary": "Chat and notifications."},
        },
        "num_communities": 2,
        "modularity": 0.42,
    })
    graph._rebuild_indices()
    return graph, ids


def cases(ids):
    def i(key):
        return ids[key]

    inv = "inventory"
    srch = "inventory_search"
    js = {"output_format": "json"}
    out = []

    def add(tool, params, family=inv, patches=()):
        out.append({"tool": tool, "family": family, "params": params, "patches": list(patches)})

    # search_graph / search_knowledge_graph
    for params in [
        {"query": "UserService"},
        {"query": "user"},
        {"query": "chat message"},
        {"query": "bcrypt"},
        {"query": "section"},
        {"query": "users.md"},
        {"query": "nothing-matches-this"},
        {"query": "user", "entity_type": "method"},
        {"query": "user", "layer": "code", "top_k": 3},
        {"query": "user", "layer": "testing"},
        {"query": "user", "file_pattern": "src/data/*.py"},
        {"query": "user", "file_pattern": "*.md"},
        {"query": "x", "entity_type": "class", "layer": "code", "file_pattern": "*.zz"},
        {"query": "user", "source_toolkit": "wiki"},
        {"query": "user", "source_toolkit": "nowhere"},
        {"query": "token", "top_k": 2, "source_toolkit": "wiki"},
        {"query": "überblick"},
        {"query": "sign"},
        {"query": "rate"},
        {"query": "users", "top_k": 20},
        dict(js, query="user"),
        dict(js, query="UserService", max_depth=1, show_all_edges=False),
        dict(js, query="hash_password", max_depth=2, show_all_edges=False),
        dict(js, query="chat", show_all_edges=False),
        dict(js, query="config", source_toolkit="wiki", show_all_edges=False),
        dict(js, query="sso", top_k=1),
        dict(js, query="none-at-all", show_all_edges=False),
    ]:
        add("search_graph", params)
    add("search_knowledge_graph", {"query": "user", "top_k": 5}, srch)
    add("search_knowledge_graph", dict(js, query="login", show_all_edges=False), srch)

    # get_entity / get_entity_details
    for params in [
        {"entity_name": "UserService"},
        {"entity_name": "userservice", "include_relations": False},
        {"entity_name": "Config"},
        {"entity_name": "Config (section)"},
        {"entity_name": "Config (class) @ repo - src/config.py"},
        {"entity_name": "Config (widget)"},
        {"entity_name": "Nope (class)"},
        {"entity_name": "Nope"},
        {"entity_name": "User"},
        {"entity_name": "Gizmo"},
        {"entity_name": "Single Sign-On"},
        {"entity_name": "Rate Limiting"},
        {"entity_name": "LegacyHelper"},
        {"entity_name": "create_user"},
        {"entity_name": "Café Überblick"},
        {"entity_name": i("user_service")},
        dict(js, entity_name="UserService"),
        dict(js, entity_name="Config"),
        dict(js, entity_name="Single Sign-On", include_relations=False),
        dict(js, entity_name=i("hash_password")),
    ]:
        add("get_entity", params)
    add("get_entity_details", {"entity_name": "hash_password"}, srch)
    add("get_entity_details", dict(js, entity_name="Login Flow"), srch)

    # get_entity_content (D3)
    for params in [{"entity_name": "UserService"}, {"entity_name": "LegacyHelper"},
                   {"entity_name": "Installation"}, {"entity_name": "user_service.py"},
                   {"entity_name": "zzz-nothing"}, {"entity_name": "Config"}]:
        add("get_entity_content", params, patches=["citation"])
    add("get_entity_content", {"entity_name": "UserService"})

    # impact_analysis
    for params in [
        {"entity_name": "hash_password"},
        {"entity_name": "hash_password", "max_depth": 1},
        {"entity_name": "UserService", "direction": "upstream"},
        {"entity_name": "UserService", "direction": "upstream", "max_depth": 1},
        {"entity_name": "Config (class)"},
        {"entity_name": "LegacyHelper"},
        {"entity_name": "Gizmo", "direction": "upstream"},
        {"entity_name": "Nope"},
        {"entity_name": "Config"},
    ]:
        add("impact_analysis", params, patches=["citation"])
    add("impact_analysis", {"entity_name": "hash_password"})
    for params in [
        dict(js, entity_name="hash_password"),
        dict(js, entity_name="UserService", direction="upstream", max_depth=2),
        dict(js, entity_name="Nope"),
    ]:
        add("impact_analysis", params)

    # get_related_entities (both families)
    for family in (inv, srch):
        for params in [
            {"entity_name": "UserService"},
            {"entity_name": "UserService", "direction": "outgoing"},
            {"entity_name": "UserService", "direction": "incoming", "relation_type": "uses"},
            {"entity_name": "UserService", "relation_type": "nope"},
            {"entity_name": "Gizmo"},
            {"entity_name": "Config (section)"},
            {"entity_name": "Config"},
            dict(js, entity_name="create_user"),
            dict(js, entity_name="create_user", relation_type="calls", direction="outgoing"),
            dict(js, entity_name="missing"),
        ]:
            add("get_related_entities", params, family)

    # get_cross_source_relations, get_stats, graph info
    add("get_cross_source_relations", {})
    add("get_cross_source_relations", js)
    add("get_stats", {})
    add("get_stats", js)
    add("get_graph_info", {})
    add("get_graph_info", js)
    add("list_ingested_sources", {})
    add("list_ingested_sources", js)
    add("load_graph", {"graph_name": "inventory"})
    add("load_graph", {})
    add("list_graphs", {})

    # list_entities_by_*
    for params in [
        {"entity_type": "class"}, {"entity_type": "Method", "limit": 2}, {"entity_type": "code"},
        {"entity_type": "code", "limit": 5}, {"entity_type": "function"},
        {"entity_type": "class", "source_toolkit": "repo"}, {"entity_type": "concept", "source_toolkit": "repo"},
        {"entity_type": "nothing"}, {"entity_type": "widget"},
        dict(js, entity_type="entity"), dict(js, entity_type="knowledge"),
        dict(js, entity_type="class", source_toolkit="repo", limit=2),
    ]:
        add("list_entities_by_type", params, patches=["citation"] if "output_format" not in params else [])
    add("list_entities_by_type", {"entity_type": "class"})
    for params in [
        {"layer": "code"}, {"layer": "documentation"}, {"layer": "knowledge", "limit": 3},
        {"layer": "testing"}, {"layer": "nothing"}, {"layer": "code", "source_toolkit": "wiki"},
        {"layer": "Domain", "source_toolkit": "wiki"},
        dict(js, layer="service"), dict(js, layer="data", source_toolkit="wiki"),
    ]:
        add("list_entities_by_layer", params)
    for params in [
        {"source_toolkit": "wiki"}, {"source_toolkit": "repo", "limit": 5},
        {"source_toolkit": "repo", "entity_type": "Method"}, {"source_toolkit": "nowhere"},
        {"source_toolkit": "wiki", "entity_type": "class"}, {},
        dict(js, source_toolkit="wiki", entity_type="feature"), dict(js, source_toolkit="repo", limit=3),
    ]:
        add("list_entities_by_source", params)

    # get_entities_by_ids / get_entity_neighbors
    for params in [
        {"entity_ids": [i("user_service"), i("get_user")]},
        {"entity_ids": [i("validate_email"), i("find_by_id")]},
        {"entity_ids": [i("validate_email"), i("find_by_id")], "include_bridging": False},
        {"entity_ids": [i("validate_email"), i("find_by_id")], "max_bridge_length": 3},
        {"entity_ids": [i("chat_handler"), i("user_service"), "missing-id"]},
        {"entity_ids": [i("gizmo"), i("legacy_helper")]},
        {"entity_ids": [i("user_service")], "include_edges": False},
        {"entity_ids": []},
        {"entity_ids": [i("validate_email"), i("find_by_id"), i("chat_feature")], "output_format": "text"},
        {"entity_ids": [i("hash_password"), i("save")], "output_format": "text"},
        {"entity_ids": [], "output_format": "text"},
    ]:
        add("get_entities_by_ids", params)
    for params in [
        {"entity_id": i("hash_password")},
        {"entity_id": i("hash_password"), "depth": 2},
        {"entity_id": i("gizmo")},
        {"entity_id": i("chat_feature"), "depth": 9},
        {"entity_id": "missing"},
        {},
        {"entity_id": i("hash_password"), "output_format": "text"},
        {"entity_id": i("chat_feature"), "depth": 0, "output_format": "text"},
        {"entity_id": "missing", "output_format": "text"},
    ]:
        add("get_entity_neighbors", params)

    # query_graph
    for params in [
        {"query": "type:class"},
        {"query": "type:class,method layer:code limit:4"},
        {"query": "layer:documentation"},
        {"query": "type:code"},
        {"query": "file:src/data/*.py"},
        {"query": "file:src/**.py type:function"},
        {"query": "name:user"},
        {"query": "user service"},
        {"query": "type:fact has_rel:true"},
        {"query": "has_rel:false"},
        {"query": "related:UserService"},
        {"query": "related:UserService dir:out type:method"},
        {"query": "related:UserService dir:in rel:uses,tests"},
        {"query": 'related:"Config (class) @ repo - src/config.py"'},
        {"query": "related:Config"},
        {"query": "related:Nope"},
        {"query": "related:UserService limit:2"},
        {"query": "related:UserService layer:documentation"},
        {"query": "related:UserService name:get"},
        {"query": "type:zzz"},
        {"query": "type:class name:zzz layer:code file:*.py"},
        {"query": "bogus:thing other"},
        {"query": 'unbalanced "quote type:class'},
        {"types": "class,entity", "limit": 3},
        {"types": ["feature"], "layers": "product"},
        {"related_to": "hash_password", "direction": "in"},
        {"query": "type:class", "types": "entity"},
        {},
        dict(js, query="type:class limit:3"),
        dict(js, query="related:create_user"),
        dict(js, query="related:Nope"),
        dict(js, query="type:zzz"),
    ]:
        add("query_graph", params, srch)

    add("list_entity_types", {}, srch)
    return out


@contextlib.contextmanager
def legacy_citation(wrapper):
    graph = wrapper._knowledge_graph._graph
    added = []
    for node_id, data in graph.nodes(data=True):
        citations = data.get("citations") or []
        if "citation" not in data and citations:
            data["citation"] = citations[0]
            added.append(node_id)
    try:
        yield
    finally:
        for node_id in added:
            graph.nodes[node_id].pop("citation", None)


def make_wrapper(retrieval, kg_module, graph_path, patched):
    wrapper = retrieval.InventoryRetrievalApiWrapper(graph_path=graph_path, base_directory=None, source_toolkits={})
    kg = wrapper._knowledge_graph
    if patched:
        entity_index = {}
        type_index = {}
        for node_id, data in kg._graph.nodes(data=True):
            name = data.get("name", "").lower()
            if name:
                entity_index.setdefault(name, OrderedSet()).add(node_id)
            kind = data.get("type", "")
            if kind:
                type_index.setdefault(kind.lower(), OrderedSet()).add(node_id)
            data["id"] = node_id  # D1
        kg._entity_index = entity_index
        kg._type_index = type_index
    return wrapper


def run_cases(graph_path, case_list, patched):
    kg_module, retrieval, tools = load_modules()
    if patched:
        kg_module.KnowledgeGraph.LAYER_TYPE_MAPPING = {
            layer: OrderedSet(kinds) for layer, kinds in LAYER_ORDER.items()
        }
        kg_module.set = OrderedSet
        tools.set = OrderedSet
        original_edges = tools.Method._get_edges_for_entities

        def edges_only(self, wrapper, entity_ids):  # D2
            # search_graph unpacks the pair; the three JSON listings and
            # impact_analysis serialised it whole.
            pair = original_edges(self, wrapper, entity_ids)
            if sys._getframe(1).f_code.co_name == "_tool_search_graph":
                return pair
            return pair[0]

        tools.Method._get_edges_for_entities = edges_only

    class NullCache:
        def touch(self, *_a, **_k):
            return None

    class Host(tools.Method):
        def __init__(self, wrapper):
            self.graph_instances = {}
            self.cache_manager = NullCache()
            self._wrapper = wrapper

        def _get_or_create_wrapper(self, graph_path, request_data=None):  # noqa: ARG002
            return self._wrapper

    results = []
    for case in case_list:
        wrapper = make_wrapper(retrieval, kg_module, graph_path, patched)
        host = Host(wrapper)
        handler_name = TOOLS[case["family"]][case["tool"]]
        handler = getattr(host, handler_name)
        params = json.loads(json.dumps(case["params"]))
        request_data = {"project_id": 1, "configuration": {"project_id": 1, "application_id": 2,
                                                           "parameters": params}, "parameters": params}
        context = legacy_citation(wrapper) if patched and "citation" in case["patches"] else contextlib.nullcontext()
        try:
            with context:
                outcome = handler(params, graph_path, request_data)
            results.append({"result": outcome})
        except Exception as error:  # noqa: BLE001
            results.append({"raised": f"{type(error).__name__}: {error}"})
    return results


TOOLS = {
    "inventory": {
        "search_graph": "_tool_search_graph", "get_entity": "_tool_get_entity",
        "get_entity_content": "_tool_get_entity_content", "impact_analysis": "_tool_impact_analysis",
        "get_related_entities": "_tool_get_related_entities",
        "get_cross_source_relations": "_tool_get_cross_source_relations", "get_stats": "_tool_get_stats",
        "list_entities_by_type": "_tool_list_entities_by_type",
        "list_entities_by_layer": "_tool_list_entities_by_layer",
        "list_entities_by_source": "_tool_list_entities_by_source",
        "get_entities_by_ids": "_tool_get_entities_by_ids", "get_entity_neighbors": "_tool_get_entity_neighbors",
        "get_graph_info": "_tool_get_graph_info", "list_ingested_sources": "_tool_list_ingested_sources",
        "list_graphs": "_tool_list_graphs", "load_graph": "_tool_load_graph",
    },
    "inventory_search": {
        "search_knowledge_graph": "_tool_search_graph", "get_entity_details": "_tool_get_entity",
        "get_related_entities": "_tool_get_related_entities", "query_graph": "_tool_query_graph",
        "list_entity_types": "_tool_list_entity_types_only",
    },
}

# The graph path the handlers see: relative to the run directory, shaped
# `{bucket}/{graph_name}/graph.json` with the defaults the port reports.
GRAPH_PATH = "inventory/inventory/graph.json"


def child(mode, workdir):
    """Run every case in this process and print the results as JSON."""
    os.chdir(workdir)
    with open(os.path.join(HERE, "cases.tmp.json"), encoding="utf-8") as handle:
        case_list = json.load(handle)
    print(json.dumps(run_cases(GRAPH_PATH, case_list, patched=(mode == "patched"))))


def main():
    if len(sys.argv) == 4 and sys.argv[1] == "--child":
        child(sys.argv[2], sys.argv[3])
        return
    kg_module, _retrieval, _tools = load_modules()
    graph, ids = build_graph(kg_module)
    graph_file = os.path.join(HERE, "graph.json")
    graph.dump_to_json(graph_file)
    # A stable `last_saved`, so the golden does not change on every run.
    with open(graph_file, encoding="utf-8") as handle:
        document = json.load(handle)
    document["_metadata"]["last_saved"] = "2026-10-07T12:00:00"
    with open(graph_file, "w", encoding="utf-8") as handle:
        json.dump(document, handle, indent=2)
        handle.write("\n")

    case_list = cases(ids)
    with open(os.path.join(HERE, "cases.tmp.json"), "w", encoding="utf-8") as handle:
        json.dump(case_list, handle)
    workdir = tempfile.mkdtemp()
    try:
        os.makedirs(os.path.join(workdir, "inventory", "inventory"))
        shutil.copy(graph_file, os.path.join(workdir, GRAPH_PATH))
        runs = {}
        for mode in ("patched", "plain"):
            for seed in ("0", "1", "2"):
                env = dict(os.environ, PYTHONHASHSEED=seed)
                output = subprocess.run(
                    [sys.executable, __file__, "--child", mode, workdir],
                    check=True, capture_output=True, text=True, env=env,
                ).stdout
                runs[(mode, seed)] = json.loads(output)
    finally:
        shutil.rmtree(workdir)
        os.remove(os.path.join(HERE, "cases.tmp.json"))

    goldens = []
    for index, case in enumerate(case_list):
        patched = [runs[("patched", seed)][index] for seed in ("0", "1", "2")]
        plain = runs[("plain", "0")][index]
        entry = dict(case)
        entry["expected"] = patched[0]
        entry["unordered"] = any(p != patched[0] for p in patched[1:])
        if "raised" in plain:
            entry["python_raised"] = plain["raised"]
        goldens.append(entry)
    with open(os.path.join(HERE, "goldens.json"), "w", encoding="utf-8") as handle:
        json.dump({"ids": ids, "cases": goldens}, handle, indent=1, ensure_ascii=False)
        handle.write("\n")
    print(f"{len(goldens)} cases, {sum(g['unordered'] for g in goldens)} unordered, "
          f"{sum('python_raised' in g for g in goldens)} where unpatched Python raised")


if __name__ == "__main__":
    main()
