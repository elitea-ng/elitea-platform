#!/usr/bin/env python3
"""Turn the benchmark's raw runs into the report's numbers (benchmark-2026-10).

Inputs, under ``<bench>`` (default ``~/.cache/elitea-dw-bench``):

* ``runs/<engine>/<corpus>/`` — ``run_engine.py`` output;
* ``model_requests.jsonl`` — ``model_router.py``'s log;
* ``search/<engine>/<corpus>.jsonl`` — the retrieval runs;
* ``judge/<corpus>.<pair>.summary.json`` — ``judge.py``;
* ``graphs/<corpus>/nodes.jsonl`` — the engine-independent graph dump.

Model time of a window is the UNION of the intervals of the model requests
labelled for it (overlapping requests count once); engine time is the
window's wall time minus that union. Writes ``summary.json`` and prints the
markdown tables of the report.
"""

from __future__ import annotations

import argparse
import json
import re
import statistics
from pathlib import Path

ENGINES = ["rust", "python-shipped", "python-patched"]
CORPORA = ["petclinic", "cleanarch", "leveldb", "express"]
ARCH_KINDS = {"class", "interface", "struct", "enum", "record", "trait", "function"}


def union(intervals: list[tuple[float, float]]) -> float:
    total, end = 0.0, None
    start = None
    for a, b in sorted(intervals):
        if end is None or a > end:
            if end is not None:
                total += end - start
            start, end = a, b
        else:
            end = max(end, b)
    if end is not None:
        total += end - start
    return total


def load_requests(path: Path) -> list[dict]:
    return [json.loads(l) for l in path.open() if l.strip()] if path.exists() else []


def model_stats(requests: list[dict], label: str, prefix: bool = False,
                window: tuple[float, float] | None = None) -> dict:
    """The model requests of one label (or label prefix), inside ``window``
    when given (a label reused by a later diagnostic run must not count)."""
    rows = [r for r in requests if (r["label"].startswith(label) if prefix else r["label"] == label)]
    if window:
        rows = [r for r in rows if r["start"] >= window[0] - 1 and r["end"] <= window[1] + 1]
    chat = [r for r in rows if r["kind"] == "chat"]
    emb = [r for r in rows if r["kind"] == "embeddings"]
    return {
        "model_s": union([(r["start"], r["end"]) for r in rows]),
        "chat_s": union([(r["start"], r["end"]) for r in chat]),
        "embed_s": union([(r["start"], r["end"]) for r in emb]),
        "chat_requests": len(chat),
        "embed_requests": len(emb),
        "embed_inputs": sum(r.get("inputs") or 0 for r in emb),
        "embed_input_forms": sorted({r.get("input_form") for r in emb if r.get("input_form")}),
        "prompt_tokens": sum(r.get("prompt_tokens") or 0 for r in chat),
        "completion_tokens": sum(r.get("completion_tokens") or 0 for r in chat),
        "reasoning_tokens": sum(r.get("reasoning_tokens") or 0 for r in chat),
        "embed_tokens": sum(r.get("prompt_tokens") or 0 for r in emb),
        # A client that hung up after [DONE] is not an error: the router of
        # the first run logged it as one, with the usage already recorded.
        "errors": sum(1 for r in rows if (r.get("status") or 200) >= 400
                      or (r.get("error") and not r.get("completion_tokens"))),
        "longest_chat_s": max((r["latency"] for r in chat), default=0.0),
        "max_inflight": max((r.get("inflight_at_start") or 0 for r in rows), default=0),
    }


# Progress markers -> phase starts. The first line containing the marker
# starts the phase; a phase ends where the next one starts.
PHASES = {
    "rust": [
        ("clone", "[worker] Clone config built"),
        ("index", "Repository cloned"),
        ("embed", "Writing unified DB"),
        ("phase2", "Unified DB: "),
        ("phase3", "Phase 2 complete"),
        ("analysis+structure", "Phase 3 complete"),
        ("pages", "Wiki structure planned"),
        ("publish", "Publishing the index"),
        ("done", "[worker] Done"),
    ],
    "python": [
        ("clone", "Clone config built"),
        ("index", "Repository cloned"),
        ("embed", "Writing unified DB"),
        # Python logs "Unified DB: N node embeddings stored", or, as shipped
        # with a 2560-dimension model, "Embedding population failed".
        ("phase2", ("Unified DB: ", "Embedding population failed")),
        ("phase3", "Phase 2 complete"),
        ("analysis+structure", "Phase 3 complete"),
        ("pages", "WikiStructureSpec created"),
        ("done", "[worker] Done"),
    ],
}


def phases(lines_path: Path, engine: str, wall: float) -> dict:
    markers = PHASES["rust" if engine == "rust" else "python"]
    stamps: dict[str, float] = {}
    if lines_path.exists():
        for line in lines_path.open():
            item = json.loads(line)
            text = item.get("thinking") or ""
            for name, marker in markers:
                alternatives = marker if isinstance(marker, tuple) else (marker,)
                if name not in stamps and any(m in text for m in alternatives):
                    stamps[name] = item["t"]
    ordered = [(n, stamps[n]) for n, _ in markers if n in stamps]
    out = {}
    for i, (name, start) in enumerate(ordered):
        if name == "done":
            break
        end = ordered[i + 1][1] if i + 1 < len(ordered) else wall
        out[name] = round(end - start, 1)
    out["_stamps"] = stamps
    return out


def pct(values: list[float], q: float) -> float | None:
    if not values:
        return None
    values = sorted(values)
    k = (len(values) - 1) * q
    f = int(k)
    c = min(f + 1, len(values) - 1)
    return values[f] + (values[c] - values[f]) * (k - f)


def retrieval(questions: list[dict], hits_path: Path, k: int = 10) -> dict | None:
    if not hits_path.exists():
        return None
    hits = {json.loads(l)["id"]: json.loads(l) for l in hits_path.open() if l.strip()}
    node_recall, node_rr, file_recall, file_rr, lat = [], [], [], [], []
    for q in questions:
        h = hits.get(q["id"])
        if h is None:
            continue
        ranked = h["hits"][:k]
        gold_nodes, gold_files = set(q["gold_node_ids"]), set(q["gold_files"])
        node_rank = next((i + 1 for i, x in enumerate(ranked) if x["node_id"] in gold_nodes), None)
        file_rank = next((i + 1 for i, x in enumerate(ranked) if x["rel_path"] in gold_files), None)
        node_recall.append(1.0 if node_rank else 0.0)
        node_rr.append(1.0 / node_rank if node_rank else 0.0)
        file_recall.append(1.0 if file_rank else 0.0)
        file_rr.append(1.0 / file_rank if file_rank else 0.0)
        lat.append(h.get("search_ms") or 0.0)
    n = len(node_recall)
    if not n:
        return None
    vectors = next((h.get("index_vectors") for h in hits.values() if "index_vectors" in h), None)
    return {
        "n": n, "node_recall@10": sum(node_recall) / n, "node_mrr@10": sum(node_rr) / n,
        "file_recall@10": sum(file_recall) / n, "file_mrr@10": sum(file_rr) / n,
        "search_ms_p50": pct(lat, 0.5), "index_vectors": vectors,
    }


def coverage(run: Path, graph: Path) -> dict | None:
    pages = sorted((run / "artifacts").rglob("*.md")) if (run / "artifacts").exists() else []
    if not pages:
        return None
    text = "\n".join(p.read_text(encoding="utf-8", errors="replace") for p in pages)
    symbols = []
    for line in (graph / "nodes.jsonl").open(encoding="utf-8"):
        node = json.loads(line)
        if node.get("is_test") or node.get("is_doc") or not node.get("is_architectural"):
            continue
        if node.get("symbol_type") not in ARCH_KINDS:
            continue
        name = (node.get("symbol_name") or "").split(".")[-1]
        if len(name) < 3:
            continue
        symbols.append((name, node.get("rel_path") or ""))
    names = {n for n, _ in symbols}
    mentioned = {n for n in names if re.search(r"(?<![A-Za-z0-9_])" + re.escape(n) + r"(?![A-Za-z0-9_])", text)}
    files = {p for _, p in symbols}
    files_cited = {p for p in files if p and (p in text or Path(p).name in text)}
    structure = sorted((run / "artifacts").rglob("wiki_structure_*.json"))
    sections = None
    if structure:
        try:
            spec = json.loads(structure[0].read_text())
            sections = len(spec.get("sections") or [])
        except ValueError:
            pass
    words = len(text.split())
    readme = [p for p in pages if p.name == "README.md"]
    return {
        "pages": len(pages) - len(readme), "sections": sections, "words": words,
        "arch_symbols": len(names), "symbols_covered": len(mentioned),
        "symbol_coverage": len(mentioned) / len(names) if names else None,
        "arch_files": len(files), "files_cited": len(files_cited),
        "file_coverage": len(files_cited) / len(files) if files else None,
        "mermaid_blocks": text.count("```mermaid"),
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--bench", type=Path, default=Path.home() / ".cache/elitea-dw-bench")
    parser.add_argument("--questions", type=Path, default=Path(__file__).resolve().parent.parent / "questions")
    args = parser.parse_args()
    requests = load_requests(args.bench / "model_requests.jsonl")
    summary: dict = {}
    for corpus in CORPORA:
        qpath = args.questions / f"{corpus}.jsonl"
        questions = [json.loads(l) for l in qpath.open()] if qpath.exists() else []
        for engine in ENGINES:
            run = args.bench / "runs" / engine / corpus
            if not (run / "run.json").exists():
                continue
            metrics = json.loads((run / "run.json").read_text())
            entry: dict = {"cold_start_s": metrics.get("cold_start_s"),
                           "rss_peak_tree_mb": {k: round(v / 1024, 1) for k, v in (metrics.get("rss_peak_tree_kb") or {}).items()},
                           "time_l_max_rss_mb": round((metrics.get("time_l_max_rss_bytes") or 0) / 2**20, 1)}
            gen = metrics.get("generate")
            if gen:
                wall = gen["wall_s"]
                model = model_stats(requests, f"{engine}/{corpus}/generate", window=(gen["start"], gen["end"]))
                entry["generate"] = {
                    "success": gen.get("success"), "wall_s": round(wall, 1), "pages": gen.get("pages"),
                    "commit": gen.get("commit_hash"), "errors": len(gen.get("errors") or []),
                    "model": model, "engine_s": round(wall - model["model_s"], 1),
                    "phases": phases(run / "generate_lines.jsonl", engine, wall),
                }
            asks = metrics.get("ask") or []
            ask_dir = run
            # The Rust asks of a corpus whose generation ran on the older
            # binary were re-run with the review-fixed one (ask-p7/).
            # Python asks first run without the image's DEEPWIKI_ASK_AGENTIC=1
            # were re-run with it (ask-rerun/).
            rerun_dir = next((run / d for d in ("ask-p7", "ask-rerun") if (run / d / "run.json").exists()), None)
            if rerun_dir is not None:
                ask_dir = rerun_dir
                rerun = json.loads((ask_dir / "run.json").read_text())
                asks = rerun.get("ask") or []
                entry["rss_peak_tree_mb"]["ask"] = round((rerun.get("rss_peak_tree_kb") or {}).get("ask", 0) / 1024, 1)
                entry["ask_rerun"] = rerun_dir.name
            answers = {}
            if (ask_dir / "ask.jsonl").exists():
                answers = {json.loads(l)["id"]: json.loads(l) for l in (ask_dir / "ask.jsonl").open() if l.strip()}
            if asks:
                walls, engine_only, ok = [], [], 0
                for a in asks:
                    m = model_stats(requests, f"{engine}/{corpus}/ask/{a['id']}", window=(a["start"], a["end"]))
                    walls.append(a["wall_s"])
                    engine_only.append(a["wall_s"] - m["model_s"])
                    ok += bool(a.get("success"))
                entry["ask"] = {
                    "n": len(asks), "success": ok,
                    # success: true with nothing to show (the model's last
                    # turn after a tool result was empty).
                    "empty_answers": sum(1 for a in answers.values() if not (a.get("answer") or "").strip()),
                    "wall_p50_s": pct(walls, 0.5), "wall_p95_s": pct(walls, 0.95),
                    "engine_p50_s": pct(engine_only, 0.5), "engine_p95_s": pct(engine_only, 0.95),
                    "model": model_stats(requests, f"{engine}/{corpus}/ask/", prefix=True,
                                         window=(asks[0]["start"], asks[-1]["end"])),
                }
            entry["retrieval"] = retrieval(questions, args.bench / "search" / engine / f"{corpus}.jsonl")
            entry["coverage"] = coverage(run, args.bench / "graphs" / corpus)
            summary.setdefault(corpus, {})[engine] = entry
        # The retrieval-only diagnostic (python_search.py --text-embeddings).
        diag = retrieval(questions, args.bench / "search" / "python-patched-textemb" / f"{corpus}.jsonl")
        if diag:
            summary.setdefault(corpus, {})["python-patched-textemb"] = {"retrieval": diag}
        for judged in sorted((args.bench / "judge").glob(f"{corpus}.*.summary.json")):
            summary.setdefault(corpus, {}).setdefault("_judge", {})[judged.name.split(".")[1]] = json.loads(judged.read_text())
    (args.bench / "summary.json").write_text(json.dumps(summary, indent=1, default=str))
    print(json.dumps(summary, indent=1, default=str)[:20000])
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
