"""Regenerate the ingestion goldens from the Python engine's own code.

Run from the repository root (standard library only):

    python3 services/elitea-inventory-engine/tests/fixtures/ingest/generate.py

The functions are taken out of ingestion.py with `ast` and executed as
they are, so the goldens are what the Python engine computes, not a
transcription of it. ingestion.py itself imports langchain and the SDK,
which none of these functions use.
"""

import ast
import hashlib
import json
import os
import re

HERE = os.path.dirname(os.path.abspath(__file__))
INGESTION = os.path.join(
    HERE, "../../../../elitea-inventory/src/elitea_inventory/engine/inventory/ingestion.py"
)

with open(INGESTION, encoding="utf-8") as handle:
    tree = ast.parse(handle.read())

namespace = {"re": re, "hashlib": hashlib}
wanted_assignments = {"CONTEXT_DEPENDENT_TYPES"}
for node in tree.body:
    if isinstance(node, ast.Assign) and any(
        isinstance(target, ast.Name) and target.id in wanted_assignments for target in node.targets
    ):
        exec(compile(ast.Module([node], []), INGESTION, "exec"), namespace)
pipeline = next(
    node for node in tree.body if isinstance(node, ast.ClassDef) and node.name == "IngestionPipeline"
)
method = next(
    node
    for node in pipeline.body
    if isinstance(node, ast.FunctionDef) and node.name == "_generate_entity_id"
)
method.args.args[1].annotation = None  # the annotations name typing imports
exec(compile(ast.Module([method], []), INGESTION, "exec"), namespace)
generate_entity_id = namespace["_generate_entity_id"]

CASES = [
    ("file", "src/users.py", "src/users.py"),
    ("file", "Docs/README.md", "Docs/README.md"),
    ("class", "UserService", "src/users.py"),
    ("class", "UserAuthenticationServiceImpl", "src/a.py"),
    ("class", "UserAuthenticationServiceImpl", "src/b.py"),
    ("function", "get", "src/a.py"),
    ("function", "get_user_by_identifier", "src/a.py"),
    ("function", "  Padded Name  ", "src/a.py"),
    ("method", "a_long_descriptive_method_name", "src/a.py"),
    ("concept", "Billing", "docs/x.md"),
    ("concept", "Billing", None),
    ("Class", "HTTPServerFactoryBuilder", "x.py"),
    ("fact", "validation_create_user", "src/users.py"),
    ("function", "parseHTTPResponseBody", "x.go"),
    ("module", "Ωμέγα_ΣΟΦΟΣ_μεγάλο", "src/greek.py"),
    ("function", "short", ""),
]

golden = [
    {"type": t, "name": n, "file_path": p, "id": generate_entity_id(None, t, n, p)}
    for t, n, p in CASES
]
with open(os.path.join(HERE, "entity_ids.json"), "w", encoding="utf-8") as handle:
    json.dump(golden, handle, indent=2, ensure_ascii=False)
    handle.write("\n")
