#!/usr/bin/env python3
"""Dump the Python engine's ask and deep research, reproducibly.

The reference side of the ADR-0026 phase 6 gate. It builds the index of a
small repository as ``python_pages_dump.py`` does (Phase 1, 1c, 2 with the
stand-in embedding, 3) into a ``.wiki.db``, then runs the LIVE worker
coroutines — ``ask_subprocess_worker.run_ask_agentic_async`` and
``deep_research_subprocess_worker.run_deep_research_async`` — against
``services/elitea-deepwiki/e2e/llm_stub.py`` with a SCRIPTED conversation
(``--script``: ``{"ask": [turns], "deep_research": [turns]}``, the stub's
``set_script`` format). Only the worker's environment is patched:

* the model is ``ChatOpenAI`` on the stub (the worker's own factory);
  the embeddings are the stand-in embedding (the index's);
* ``resolve_unified_db_path`` answers the built ``.wiki.db``;
* the date in the system prompts is ``--date``;
* deep research gets no ``FilesystemBackend`` (the import is hidden), so it
  runs on deepagents' ``StateBackend``, the in-memory file system ADR-0026
  decision 8 keeps. The engine hands ``create_deep_agent`` a backend
  FACTORY, which the pinned deepagents 0.7.13 refuses (``TypeError: backend
  must be an initialized backend instance``): the live Python deep research
  fails before its first model call. The dump passes ``StateBackend()``
  instead, the one substitution that lets the reference run at all.
* deep research is the agent ADR-0026 decision 8 specifies: the pinned
  deepagents no longer installs a todo list, so the dump adds LangChain's
  ``TodoListMiddleware`` (``write_todos``, which the research prompt asks
  for), and it switches the general-purpose sub-agent (the ``task`` tool)
  off, which the engine meant to do with ``subagents=[]``.

The worker's stdout markers are turned into sidecar lines exactly as
``tool_operations._run_ask_subprocess`` / ``_run_deep_research_subprocess``
turn them (transcribed below; plain log lines are dropped).

Written to ``<out-dir>``:

``{tool}/requests.jsonl``  every chat request body, in order
``{tool}/lines.jsonl``     the sidecar lines (``{"thinking": …}``, ``{"token": …}``)
``{tool}/result.json``     the worker's result
``tools.jsonl``            direct tool calls (``--calls``): name, arguments, result
``nodes.jsonl``, ``edges.jsonl``, ``embeddings.jsonl``  the index

    PYTHONHASHSEED=0 PYTHONPATH=services/elitea-deepwiki/src \\
        python parity/python_ask_dump.py <repo> <out-dir> --script s.json [--calls c.json]
"""

from __future__ import annotations

import argparse
import asyncio
import datetime as _dt
import json
import os
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(HERE.parent.parent / "elitea-deepwiki" / "e2e"))

import python_pages_dump as pages  # noqa: E402
import python_phase3_dump as p3  # noqa: E402


from langchain_core.embeddings import Embeddings  # noqa: E402


class StandInEmbeddings(Embeddings):
    """The index's stand-in embedding behind the LangChain interface."""

    def embed_query(self, text: str) -> list[float]:
        return p3.stand_in_embedding(text)

    def embed_documents(self, texts: list[str]) -> list[list[float]]:
        return [p3.stand_in_embedding(t) for t in texts]


def freeze_date(day: str) -> None:
    """``datetime.now()`` in the prompt modules answers ``day``."""
    from elitea_deepwiki.engine import ask_prompts  # noqa: PLC0415
    from elitea_deepwiki.engine.deep_research import research_engine, research_prompts  # noqa: PLC0415

    fixed = _dt.datetime.fromisoformat(day)

    class Frozen(_dt.datetime):
        @classmethod
        def now(cls, tz=None):  # noqa: ARG003
            return fixed

    ask_prompts.datetime = Frozen
    research_prompts.datetime = Frozen
    research_prompts.RESEARCH_INSTRUCTIONS = research_prompts.get_research_instructions()
    research_engine.RESEARCH_INSTRUCTIONS = research_prompts.RESEARCH_INSTRUCTIONS


def sidecar_lines(printed: list[str], tool: str) -> list[dict]:
    """The worker's markers as the sidecar sends them (tool_operations)."""
    lines: list[dict] = []
    if tool == "ask":
        lines.append({"thinking": "Processing your question..."})
    else:
        lines.append({"thinking": "Starting deep research analysis..."})
    for line in printed:
        line = line.strip()
        if tool == "ask":
            if line.startswith("[THINKING_STEP] "):
                lines.append({"thinking": line[16:]})
            elif line.startswith("[TODO_UPDATE] "):
                lines.append({"thinking": json.dumps(
                    {"event": "todo_update", "data": {"items": json.loads(line[14:])}})})
            elif line.startswith("[LLM_CHUNK] "):
                fragment = json.loads(line[12:]).get("text") or ""
                if fragment:
                    lines.append({"token": fragment})
            elif line.startswith("[ASK_EVENT] "):
                lines.append({"thinking": line[12:]})
            continue
        if line.startswith("[THINKING_STEP] "):
            parsed = json.loads(line[16:])
            event_type = parsed.get("type", "log")
            title = parsed.get("title", "")
            content = parsed.get("content", "")
            metadata = parsed.get("metadata", {})
            if event_type == "tool_call":
                display = f"\U0001f527 {title}"
            elif event_type == "tool_result":
                tool_name = metadata.get("tool", "tool")
                brief = (content[:200] + "...") if len(content) > 200 else content
                display = f"✓ {tool_name}: {brief}"
            elif content and content != title:
                display = f"{title}\n{content}"
            else:
                display = title
            lines.append({"thinking": display})
        elif line.startswith("[TODO_UPDATE] "):
            todos = json.loads(line[14:])
            normalized = []
            for todo in todos:
                normalized.append({
                    "id": todo.get("id", len(normalized)),
                    "title": todo.get("content", todo.get("title", "")),
                    "description": todo.get("description", ""),
                    "status": todo.get("status", "not-started").replace("_", "-").replace("pending", "not-started"),
                })
            lines.append({"thinking": json.dumps({"event": "todo_update", "data": {"items": normalized}})})
    return lines


def run_worker(tool: str, base_path: Path, db_path: str, base_url: str, payload: dict,
               adr_reference: bool = True):
    from elitea_deepwiki.engine import repo_resolution  # noqa: PLC0415

    repo_resolution.resolve_unified_db_path = lambda **_k: db_path
    if tool == "ask":
        from elitea_deepwiki.engine import ask_subprocess_worker as worker  # noqa: PLC0415

        run = worker.run_ask_agentic_async
    else:
        from elitea_deepwiki.engine import deep_research_subprocess_worker as worker  # noqa: PLC0415

        run = worker.run_deep_research_async
    original_build = worker._build_llm_and_embeddings
    worker._build_llm_and_embeddings = lambda settings, model: (
        original_build(settings, model)[0], StandInEmbeddings())
    printed: list[str] = []
    worker._print = printed.append

    import deepagents.backends as backends  # noqa: PLC0415
    from elitea_deepwiki.engine.deep_research import research_engine  # noqa: PLC0415

    original_create = research_engine.create_deep_agent

    def create_deep_agent(*a, backend=None, middleware=(), **k):
        if backend is None or callable(backend):
            backend = backends.StateBackend()
        if adr_reference:
            from langchain.agents.middleware import TodoListMiddleware  # noqa: PLC0415

            middleware = [*middleware, TodoListMiddleware()]
        return original_create(*a, backend=backend, middleware=middleware, **k)

    research_engine.create_deep_agent = create_deep_agent
    import deepagents.graph as graph  # noqa: PLC0415
    from types import SimpleNamespace  # noqa: PLC0415

    original_gp = graph.GeneralPurposeSubagentProfile
    if adr_reference:
        graph.GeneralPurposeSubagentProfile = lambda: SimpleNamespace(enabled=False)
    hidden = backends.__dict__.pop("FilesystemBackend", None)
    try:
        result = asyncio.run(run(dict(payload, base_path=str(base_path))))
    finally:
        if hidden is not None:
            backends.FilesystemBackend = hidden
        graph.GeneralPurposeSubagentProfile = original_gp
        research_engine.create_deep_agent = original_create
    return result, sidecar_lines(printed, tool)


def direct_calls(db_path: str, calls: list[dict]) -> list[dict]:
    """Every tool called once, outside an agent, over the same index."""
    from elitea_deepwiki.engine.code_graph.storage_query_service import StorageQueryService  # noqa: PLC0415
    from elitea_deepwiki.engine.deep_research.research_tools import create_codebase_tools  # noqa: PLC0415
    from elitea_deepwiki.engine.unified_db import UnifiedWikiDB  # noqa: PLC0415
    from elitea_deepwiki.engine.unified_retriever import UnifiedRetriever  # noqa: PLC0415

    udb = UnifiedWikiDB(db_path, readonly=True)
    embeddings = StandInEmbeddings()
    retriever = UnifiedRetriever(db=udb, embedding_fn=embeddings.embed_query, embeddings=embeddings)
    service = StorageQueryService(udb)
    tools = {}
    for progressive in ("1", ""):
        os.environ["DEEPWIKI_PROGRESSIVE_TOOLS"] = progressive
        for t in create_codebase_tools(
            retriever_stack=retriever, graph_manager=None, code_graph=None,
            repo_analysis={}, query_service=service,
        ):
            tools[t.name] = t
    os.environ.pop("DEEPWIKI_PROGRESSIVE_TOOLS", None)
    out = []
    for call in calls:
        result = tools[call["name"]].invoke(call["arguments"])
        out.append({"name": call["name"], "arguments": call["arguments"], "result": result})
    return out


def run_vfs_probe(base_url: str, recorded: list, stub, calls: list[dict]) -> list[dict]:
    """Each call alone in its own turn of a deep agent on ``StateBackend``
    (with the todo list, without the sub-agent), as deep research runs.
    Returns the ToolMessage text each call got, in order."""
    import deepagents.graph as graph  # noqa: PLC0415
    from types import SimpleNamespace  # noqa: PLC0415

    from deepagents import create_deep_agent  # noqa: PLC0415
    from deepagents.backends import StateBackend  # noqa: PLC0415
    from langchain.agents.middleware import TodoListMiddleware  # noqa: PLC0415
    from langchain_openai import ChatOpenAI  # noqa: PLC0415

    original_gp = graph.GeneralPurposeSubagentProfile
    graph.GeneralPurposeSubagentProfile = lambda: SimpleNamespace(enabled=False)
    try:
        model = ChatOpenAI(model="gpt-4o", api_key="stub-key", base_url=base_url, temperature=0.1,
                           streaming=False, max_tokens=8192)
        agent = create_deep_agent(model=model, tools=[], system_prompt="probe",
                                  backend=StateBackend(), middleware=[TodoListMiddleware()])
        turns = [{"tool_calls": [dict(call, id=f"call_v{i}")]} for i, call in enumerate(calls)]
        stub.set_script([*turns, {"content": "done"}])
        recorded.clear()
        agent.invoke({"messages": [{"role": "user", "content": "probe"}]})
    finally:
        graph.GeneralPurposeSubagentProfile = original_gp
    final = recorded[-1]["messages"]
    by_id = {m["tool_call_id"]: m["content"] for m in final if m.get("role") == "tool"}
    return [
        {"name": call["name"], "arguments": call["arguments"], "result": by_id.get(f"call_v{i}")}
        for i, call in enumerate(calls)
    ]


def dump_embeddings(db_path: str, out: Path) -> None:
    import sqlite3  # noqa: PLC0415

    nodes = [json.loads(line)["node_id"] for line in open(out / "nodes.jsonl", encoding="utf-8")]
    from elitea_deepwiki.engine.unified_db import UnifiedWikiDB  # noqa: PLC0415

    udb = UnifiedWikiDB(db_path, embedding_dim=p3.EMBEDDING_DIM, readonly=True)
    with open(out / "embeddings.jsonl", "w", encoding="utf-8") as handle:
        for node_id in nodes:
            vector = udb.get_embedding_by_id(node_id)
            if vector is not None:
                handle.write(json.dumps({"node_id": node_id, "embedding": [float(x) for x in vector]}) + "\n")
    udb.close()
    del sqlite3


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("repo", type=Path)
    parser.add_argument("out", type=Path)
    parser.add_argument("--script", type=Path, required=True)
    parser.add_argument("--calls", type=Path)
    parser.add_argument("--date", default="2026-01-02T03:04:05")
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    script = json.loads(args.script.read_text(encoding="utf-8"))

    server, recorded, stub = pages.start_stub()
    base_url = f"http://127.0.0.1:{server.server_address[1]}/v1"
    freeze_date(args.date)

    with tempfile.TemporaryDirectory() as tmp:
        tmp = Path(tmp)
        db_path = str(tmp / "index.wiki.db")
        _graph, udb = pages.build_index(args.repo.resolve(), db_path)
        pages.dump_rows(udb, args.out)
        udb.close()
        dump_embeddings(db_path, args.out)

        payload = {
            "question": script["question"],
            "llm_settings": {"api_base": base_url, "api_key": "stub-key", "model_name": "gpt-4o"},
            "embedding_model": "text-embedding-3-small",
            "repo_config": {"provider_type": "github", "repository": "acme/notes", "branch": "main",
                            "provider_config": {}, "project": None},
            "chat_history": script.get("chat_history", []),
            "k": 15,
            "repo_identifier_override": "acme/notes:main:01234567",
        }
        os.environ["DEEPWIKI_ASK_AGENTIC"] = "1"
        for tool in ("ask", "deep_research"):
            if tool not in script:
                continue
            stub.set_script(script[tool])
            recorded.clear()
            result, lines = run_worker(tool, tmp / tool, db_path, base_url, payload)
            target = args.out / tool
            target.mkdir(exist_ok=True)
            with open(target / "requests.jsonl", "w", encoding="utf-8") as handle:
                for body in recorded:
                    handle.write(json.dumps(body, ensure_ascii=False, sort_keys=True) + "\n")
            with open(target / "lines.jsonl", "w", encoding="utf-8") as handle:
                for line in lines:
                    handle.write(json.dumps(line, ensure_ascii=False) + "\n")
            (target / "result.json").write_text(
                json.dumps(result, indent=2, ensure_ascii=False, sort_keys=True) + "\n", encoding="utf-8")
        if "vfs" in script:
            with open(args.out / "vfs.jsonl", "w", encoding="utf-8") as handle:
                for record in run_vfs_probe(base_url, recorded, stub, script["vfs"]):
                    handle.write(json.dumps(record, ensure_ascii=False) + "\n")
        if args.calls:
            calls = json.loads(args.calls.read_text(encoding="utf-8"))
            with open(args.out / "tools.jsonl", "w", encoding="utf-8") as handle:
                for record in direct_calls(db_path, calls):
                    handle.write(json.dumps(record, ensure_ascii=False) + "\n")
    server.shutdown()


if __name__ == "__main__":
    main()
