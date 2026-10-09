"""Regenerate goldens.json for tests/code_like.rs: what the Python engine's
real ingestion path made of a code file that has no dedicated parser
(`.sh`, `.rb`, `.lua`, a Makefile, …).

Run from the repository root:

    uv run --python 3.13 --with "networkx>=3.5,<4" --with pydantic --with "langchain-core>=0.3,<2" \\
        python services/elitea-inventory-engine/tests/fixtures/code_like/generate.py

The Python pipeline has TWO per-file paths:

* `_process_file_with_chunks` — what `run()` (the `run_ingestion` tool)
  uses for every file. Its parser branch is `if parser and
  _is_code_file(path)`, with no else: a code-like file without a parser gets
  its file node and the model's entities and facts (with the CODE fact
  prompt, `_is_code_file or _is_code_like_file`), and nothing from a parser.
* `_extract_entities_from_doc` — the "HYBRID FALLBACK" that runs
  `TextParser` over such a file ("See X", "Depends on Y", tickets, URLs,
  versions). It is reached only from `run_from_generator`, whose one caller
  is `delta_update`, a tool no router ever served (the host refuses it:
  `DeferredTools`). No graph on any platform ever received its relations.

This generator runs the REAL `_process_file_with_chunks` (bound to a
stand-in `self` without a model: the model stage is tested elsewhere) over
the files below, and records the entities and relations it returned, and
`_is_code_file` / `_is_code_like_file` for a list of paths. For reference
it also records what `TextParser` does with each file (`text_parser`): it
RAISES on the first reference it finds, so even the dead fallback path added
nothing from such a file. The port adds nothing either (`tests/code_like.rs`).
"""

import importlib
import json
import os
import sys
import types

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

ingestion = importlib.import_module("elitea_inventory.engine.inventory.ingestion")
from langchain_core.documents import Document  # noqa: E402

FILES = {
    "scripts/deploy.sh": "#!/bin/sh\n# Deploy the service. See DeployGuide and depends on Helm.\n# Fixes PROJ-123, see #42 and https://example.com/docs (v1.2.3)\nhelm upgrade --install app ./chart\n",
    "lib/billing.rb": "# Billing helpers. Uses PaymentGateway and extends BaseService.\nclass Billing < BaseService\n  def charge(amount)\n    PaymentGateway.charge(amount)\n  end\nend\n",
    "init.lua": "-- Calls Loader.start; requires Config\nlocal M = {}\nfunction M.start() return Loader.start() end\nreturn M\n",
    "Makefile": "# Build. Refer to BuildNotes.\nall:\n\tcc -o app main.c\n",
    "src/main.c": "/* Implements Server. commit: abcdef1234 */\nint main(void) { return 0; }\n",
}

me = types.SimpleNamespace(
    _entity_extractor=None,
    llm=None,
    min_file_lines=20,
    min_file_chars=300,
)
me._generate_entity_id = lambda *args: ingestion.IngestionPipeline._generate_entity_id(me, *args)

def stored_type(entity):
    """The type the graph keeps: `_process_file_batch_and_update_graph`
    adds every entity through `KnowledgeGraph.add_entity`, which normalises."""
    graph = ingestion.KnowledgeGraph()
    graph.add_entity(entity_id=entity["id"], name=entity["name"], entity_type=entity["type"],
                     citation=entity["citation"], properties=entity["properties"])
    return graph._graph.nodes[entity["id"]]["type"]


files = []
for path, text in FILES.items():
    raw = Document(page_content=text, metadata={"file_path": path, "source_toolkit": "repo"})
    entities, relations, content_hash = ingestion.IngestionPipeline._process_file_with_chunks(
        me, path, [], raw, "repo", None
    )
    # What the dead hybrid path would have added: TextParser raises on the
    # first reference it finds (its `Range(start_line=…)` does not match
    # `Range`), and the fallback caught that and logged a warning. So even
    # that path never added a relation from a file with a reference in it.
    try:
        found = importlib.import_module("elitea_inventory.engine.inventory.parsers").TextParser().parse_file(
            path, content=text).relationships
        text_parser = {"relations": [[r.source_symbol, r.target_symbol] for r in found]}
    except Exception as error:  # noqa: BLE001 - the error is the golden
        text_parser = {"error_type": type(error).__name__, "message": str(error)}
    files.append({
        "path": path,
        "text": text,
        "content_hash": content_hash,
        "entities": [{"id": e["id"], "name": e["name"], "type": e["type"],
                      "stored_type": stored_type(e)} for e in entities],
        "relations": relations,
        "text_parser": text_parser,
    })

PATHS = ["a.sh", "a.bash", "a.zsh", "a.rb", "a.lua", "a.pl", "a.pm", "a.php", "a.bat", "a.c", "a.cpp",
         "a.h", "a.hpp", "a.m", "a.dart", "a.groovy", "a.scala", "a.hs", "a.r", "a.R", "Makefile",
         "GNUmakefile", "a.py", "a.kt", "a.md", "a.txt", "a.sql", "a.pas", "a.asm", "a.yaml"]
goldens = {
    "_comment": "Generated by generate.py with the Python Inventory engine. Do not edit by hand.",
    "files": files,
    "classification": [
        {"path": p, "code_file": ingestion._is_code_file(p), "code_like": ingestion._is_code_like_file(p)}
        for p in PATHS
    ],
}
with open(os.path.join(HERE, "goldens.json"), "w", encoding="utf-8") as handle:
    json.dump(goldens, handle, indent=1, ensure_ascii=False)
    handle.write("\n")
print(f"{len(files)} files")
