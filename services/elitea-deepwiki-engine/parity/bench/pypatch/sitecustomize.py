"""Benchmark-only runtime patch of the Python engine (benchmark-2026-10).

On the PYTHONPATH of the patched Python variant, and only when
``DWB_EMBED_DIM`` is set: it changes ONE thing, the default
``embedding_dim`` of ``UnifiedWikiDB`` (and of the storage adapter that
forwards it), from the shipped ``1536`` to ``DWB_EMBED_DIM``. The shipped
engine creates ``repo_vec`` as ``float32[1536]``, so a 2560-dimension model's
vectors are refused by sqlite-vec and the index has no dense rows (ADR-0026
context fact 6). The engine copy is never edited: the default is replaced
when the module is first imported, in every process that imports it — the
sidecar and its subprocess workers, which inherit PYTHONPATH.
"""

from __future__ import annotations

import importlib.abc
import importlib.machinery
import os
import sys

_DIM = os.environ.get("DWB_EMBED_DIM")
# The subprocess workers import the engine under an alias package as well
# (``plugin_implementation.unified_db``, the legacy plugin layout), so a
# module is matched by its last components, not its full dotted name.
def _is_target(name: str) -> bool:
    return name.endswith(".unified_db") or name.endswith("storage.sqlite")


def _patch(module) -> None:
    dim = int(_DIM)
    if module.__name__.endswith(".unified_db"):
        init = module.UnifiedWikiDB.__init__
        init.__defaults__ = (dim,)
    else:
        for value in vars(module).values():
            init = getattr(value, "__init__", None)
            code = getattr(init, "__code__", None)
            if code is not None and "embedding_dim" in code.co_varnames and init.__defaults__:
                names = code.co_varnames[: code.co_argcount]
                defaults = list(init.__defaults__)
                offset = len(names) - len(defaults)
                if "embedding_dim" in names[offset:]:
                    defaults[names.index("embedding_dim") - offset] = dim
                    init.__defaults__ = tuple(defaults)
    sys.stderr.write(f"[dwb-patch] {module.__name__}: embedding_dim default -> {dim}\n")


class _Loader(importlib.abc.Loader):
    def __init__(self, inner):
        self._inner = inner

    def create_module(self, spec):
        return self._inner.create_module(spec)

    def exec_module(self, module):
        self._inner.exec_module(module)
        _patch(module)


class _Finder(importlib.abc.MetaPathFinder):
    def find_spec(self, name, path, target=None):
        if not _is_target(name):
            return None
        spec = importlib.machinery.PathFinder.find_spec(name, path)
        if spec is not None and spec.loader is not None:
            spec.loader = _Loader(spec.loader)
        return spec


if _DIM:
    sys.meta_path.insert(0, _Finder())
