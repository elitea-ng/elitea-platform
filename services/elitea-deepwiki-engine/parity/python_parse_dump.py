#!/usr/bin/env python3
"""Dump one Python rich parser's ParseResults for a repository, reproducibly.

The per-parser reference for an ADR-0026 parser port: what the Python
engine's ``parse_multiple_files`` returns for every file of one language,
before the graph builder turns it into nodes and edges. A port is checked
here first, file by file, so a difference is found in the parser rather than
in the graph.

    python parity/python_parse_dump.py <language> <repo> <out.jsonl>

One JSON object per file, sorted by path: ``file_path`` (relative to the
repository), ``language``, ``symbols``, ``relationships``, ``imports``,
``exports``, ``dependencies`` (sorted), ``module_docstring``, ``errors``.
Enum values are their wire strings. Paths inside symbols and relationships
are made repository-relative too.

The same reproducibility patch as ``python_reference.py``: inline executors,
submission-order ``as_completed``, sorted files. File discovery is the graph
builder's own.
"""

from __future__ import annotations

import dataclasses
import enum
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from python_reference import InlineExecutor, _make_reproducible  # noqa: E402


def plain(value, root: str):
    if isinstance(value, enum.Enum):
        return value.value
    if dataclasses.is_dataclass(value):
        return {f.name: plain(getattr(value, f.name), root) for f in dataclasses.fields(value)}
    if isinstance(value, dict):
        return {str(k): plain(v, root) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [plain(v, root) for v in value]
    if isinstance(value, set):
        return sorted(plain(v, root) for v in value)
    if isinstance(value, str) and value.startswith(root):
        return value[len(root):]
    if isinstance(value, (str, int, float, bool)) or value is None:
        return value
    return repr(value)


def main() -> int:
    if len(sys.argv) != 4:
        print(__doc__, file=sys.stderr)
        return 2
    language, repo, out = sys.argv[1], Path(sys.argv[2]).resolve(), Path(sys.argv[3])
    _make_reproducible()
    from elitea_deepwiki.engine.code_graph import graph_builder as gb  # noqa: PLC0415

    for module in list(sys.modules.values()):
        name = getattr(module, "__name__", "")
        if name.startswith(("elitea_deepwiki", "plugin_implementation")) and hasattr(module, "ThreadPoolExecutor"):
            module.ThreadPoolExecutor = InlineExecutor

    builder = gb.EnhancedUnifiedGraphBuilder(max_workers=1)
    files = sorted(builder._discover_files_by_language(str(repo)).get(language, []))
    parser = builder.rich_parsers[language]
    results = parser.parse_multiple_files(files, max_workers=1)
    root = str(repo) + "/"
    out.parent.mkdir(parents=True, exist_ok=True)
    with open(out, "w", encoding="utf-8") as handle:
        for path in sorted(results):
            result = plain(results[path], root)
            result.pop("file_hash", None)
            result.pop("parse_time", None)
            result.pop("warnings", None)
            result["file_path"] = path[len(root):] if path.startswith(root) else path
            handle.write(json.dumps(result, sort_keys=True, ensure_ascii=False) + "\n")
    print(json.dumps({"language": language, "files": len(results)}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
