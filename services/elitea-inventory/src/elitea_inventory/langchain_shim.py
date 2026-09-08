"""``langchain.text_splitter`` under the name the copied engine imports.

``engine/inventory/ingestion.py`` opens its streaming chunker block with

    from elitea_sdk.tools.chunkers.universal_chunker import chunk_single_document
    from elitea_sdk.tools.chunkers.code.codeparser import parse_code_files_for_db
    from langchain.text_splitter import RecursiveCharacterTextSplitter

LangChain 1.x moved that module out of the ``langchain`` package and into its
own distribution, ``langchain_text_splitters``. The first two imports resolve;
the third raises ``ImportError``, and because all three share one ``try:``,
the failure takes the chunkers down with it: ``has_chunker`` becomes False and
every document is passed through as a single raw chunk, whatever its size. The
run still reports success — the only trace is one log line, "Chunkers not
available, using raw documents".

The engine files are copied BYTE FOR BYTE (``COPY_MANIFEST.json`` pins their
digests), so the import cannot be rewritten in place, and this package's copy
tool applies substitutions only to the tool layer. The same reasoning that
produced ``pylon_shim`` applies here, with one difference worth stating: that
one FABRICATES a module Pylon would have provided, while this one is an ALIAS.
``langchain_text_splitters`` is where LangChain moved the very code the old
name exported, so binding the old name to the real successor module is a
rename, not a stub — every splitter the module ever exported is present, and
behaviour is LangChain's own.

Registered under the parent package's attribute as well as in ``sys.modules``
so that both ``from langchain.text_splitter import X`` and
``import langchain.text_splitter`` resolve.
"""

from __future__ import annotations

import sys

MODULE = "langchain.text_splitter"


def install() -> None:
    """Bind the old name to the successor module, once.

    Idempotent, and it never displaces a real ``langchain.text_splitter``: on a
    LangChain that still ships one, the import below succeeds and this returns
    without touching ``sys.modules``.
    """
    if MODULE in sys.modules:
        return
    try:
        __import__(MODULE)
        return  # a LangChain that still provides it — leave it alone
    except ImportError:
        pass

    try:
        import langchain_text_splitters
    except ImportError:
        # Neither name is installed. Do NOT bind a stub: the engine's own
        # ImportError fallback is the honest outcome, and a fabricated
        # splitter would chunk nothing while claiming to chunk.
        return

    sys.modules[MODULE] = langchain_text_splitters
    try:
        import langchain
    except ImportError:  # pragma: no cover - langchain_text_splitters implies it
        return
    langchain.text_splitter = langchain_text_splitters


__all__ = ["install"]
