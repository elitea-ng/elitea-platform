"""The streaming ingestion path must actually produce chunks.

`IngestionPipeline.run` chunks each document as it streams, guarded by a
`try:` that imports the two SDK chunkers AND a text splitter. On ImportError
it sets `has_chunker = False`, logs "Chunkers not available, using raw
documents", and falls back to `chunks = [document]` — one chunk per file,
whatever its size. The run still reports success, so the failure is silent:
nothing about the result distinguishes "chunked" from "did not chunk", which
is why this asserts the CHUNKS handed to the batch processor rather than any
import or any flag.

The splitter is the non-code half of that path (code files go to the
tree-sitter parser instead), so a large prose document is what measures it.
"""

from __future__ import annotations

import types

from langchain_core.documents import Document

from elitea_inventory.engine.inventory.ingestion import IngestionPipeline

# Comfortably past the 1000-character chunk size the engine configures, in
# paragraphs the splitter can break on.
PROSE = "\n\n".join(f"Paragraph {index}. " + ("prose " * 60) for index in range(40))


class _Toolkit:
    """One prose document, streamed the way a source toolkit streams."""

    def loader(self, **_kwargs):
        yield Document(page_content=PROSE, metadata={"file_path": "docs/guide.md"})


class _LLM:
    """Truthy enough for `_init_extractors`; extraction never runs here."""

    def invoke(self, *_args, **_kwargs):
        return types.SimpleNamespace(content='{"entities": [], "relations": []}')


def test_a_large_prose_document_reaches_the_batch_as_many_chunks(tmp_path, monkeypatch):
    pipeline = IngestionPipeline(
        llm=_LLM(),
        graph_path=str(tmp_path / "graph.json"),
        source_toolkits={"src": _Toolkit()},
    )

    # The seam: every batch the streaming loop assembles arrives here as
    # (file_path, chunks, raw_doc). Capturing it measures what the loop
    # produced without running entity extraction or touching a graph.
    captured: list[tuple[str, list[Document], Document]] = []
    monkeypatch.setattr(
        pipeline,
        "_process_file_batch_and_update_graph",
        lambda file_batch, *_args, **_kwargs: captured.extend(file_batch),
    )

    result = pipeline.run(source="src", extract_relations=False, resume=False, max_documents=1)

    assert result.success, result.errors
    assert captured, "the streaming path handed the batch processor nothing at all"

    _file_path, chunks, raw_document = captured[0]
    assert raw_document.page_content == PROSE
    assert len(chunks) > 1, (
        f"a {len(PROSE)}-character document reached the batch as {len(chunks)} chunk(s): "
        "the streaming path is not chunking, it is passing raw documents through"
    )
