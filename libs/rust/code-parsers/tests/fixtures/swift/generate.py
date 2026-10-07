"""Regenerate expected.json: the Inventory engine's Python SwiftParser
output for every sample in this directory (plus one missing file).

Run from the repository root:

    python3 libs/rust/code-parsers/tests/fixtures/swift/generate.py

The parser module is loaded WITHOUT the elitea_inventory package __init__
chain (which imports langchain): stub parent packages with __path__ set are
placed in sys.modules, then the parser module itself is imported. It needs
only the standard library.
"""

import dataclasses
import enum
import importlib
import json
import os
import sys
import types
from pathlib import Path

LANGUAGE = "swift"
MODULE = "swift_parser"
CLASS = "SwiftParser"
EXTENSIONS = (".swift",)
MISSING = "Missing.swift"

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[5]
SRC = REPO / "services" / "elitea-inventory" / "src"
PACKAGE = "elitea_inventory.engine.inventory.parsers"


def load_parser_module():
    parts = PACKAGE.split(".")
    for depth in range(1, len(parts) + 1):
        name = ".".join(parts[:depth])
        stub = types.ModuleType(name)
        stub.__path__ = [str(SRC.joinpath(*parts[:depth]))]
        sys.modules[name] = stub
    return importlib.import_module(f"{PACKAGE}.{MODULE}")


def plain(value):
    if dataclasses.is_dataclass(value):
        return {f.name: plain(getattr(value, f.name)) for f in dataclasses.fields(value)}
    if isinstance(value, enum.Enum):
        return value.value
    if isinstance(value, dict):
        return {k: plain(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [plain(v) for v in value]
    if isinstance(value, set):
        return sorted(plain(v) for v in value)
    return value


def main():
    module = load_parser_module()
    parser = getattr(module, CLASS)()
    os.chdir(HERE)  # file paths in the dump are relative to this directory
    names = sorted(p.name for p in HERE.iterdir() if p.suffix in EXTENSIONS)
    files = {}
    for name in names + [MISSING]:
        result = plain(parser.parse_file(name))
        result.pop("parse_time", None)  # wall clock, not output
        files[name] = result
    out = HERE / "expected.json"
    out.write_text(
        json.dumps({"language": LANGUAGE, "files": files}, indent=1, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    print(f"wrote {out.relative_to(REPO)}: {len(files)} files")


if __name__ == "__main__":
    main()
