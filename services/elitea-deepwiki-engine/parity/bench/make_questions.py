#!/usr/bin/env python3
"""Build the fixed retrieval question set of one corpus (benchmark-2026-10).

Engine-independent by construction: the candidates come from the code graph
``deepwiki-parity graph-dump`` writes (Phase 1 + 1c, no model, no index), and
the questions from the chat model, one call per candidate.

1. Candidates: architectural code symbols — ``class``, ``interface``,
   ``struct``, ``enum``, ``record``, ``trait``, ``function`` (and, for
   JavaScript, a ``constant`` whose source is a function expression; and a
   ``method`` of at least 300 characters) — not a test, not a document, not
   under a test directory, with at least 100 characters of source. Sorted by
   node id, then a seeded sample of ``--candidates``.
2. One question per candidate: the model sees the symbol's file, kind and
   source (first 80 lines) and writes ONE question a developer new to the
   repository would ask, whose answer is that code, WITHOUT naming the
   identifier or the file (so retrieval has to be semantic or lexical on the
   behaviour, not an identifier lookup).
3. Rule filter (no hand edits; a rejected question gets ONE retry, with
   the reason appended to the prompt): one sentence ending in ``?``, 6-40 words, no
   identifier of the symbol (case-insensitive, names of 4+ characters), no
   file name, no "test", no near-duplicate (word-set Jaccard > 0.7 with a
   kept question).
4. Gold: the symbol's ``node_id`` and its ``rel_path``.

The first ``--keep`` survivors (in sample order) are written to
``<out>``, one JSON object per line; the first 15 are the ``ask`` subset.

    python parity/bench/make_questions.py <graph-dir> <corpus> <commit> <out.jsonl> \\
        --api-base http://127.0.0.1:18950/v1 --model RadixArk/Qwen3.8-27B-NVFP4
"""

from __future__ import annotations

import argparse
import json
import random
import re
import sys
import urllib.request
from pathlib import Path

KINDS = {"class", "interface", "struct", "enum", "record", "trait", "function"}

PROMPT = """You are helping build a code-search benchmark for the repository "{repo}".

Below is one {kind} from the file `{path}`:

```
{source}
```

Write ONE natural question that a developer who is new to this repository might ask, \
whose best answer is exactly this code. Describe the behaviour or responsibility in \
plain words. Do NOT mention the identifier `{name}` or any other identifier from the \
code, and do NOT mention the file name. Do not ask about tests. \
Reply with the question only, on one line, ending with a question mark."""


def chat(api_base: str, model: str, prompt: str) -> str:
    body = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "temperature": 0,
        "seed": 7,
        "max_tokens": 4000,
        # The model reasons before it answers; a question needs little of it
        # (2,200 reasoning tokens, 38 s, per question at the default effort).
        "reasoning_effort": "low",
    }
    request = urllib.request.Request(
        f"{api_base}/chat/completions",
        data=json.dumps(body).encode(),
        headers={"content-type": "application/json"},
    )
    with urllib.request.urlopen(request, timeout=600) as response:
        data = json.load(response)
    return (data["choices"][0]["message"].get("content") or "").strip()


def candidates(graph: Path) -> list[dict]:
    out = []
    for line in (graph / "nodes.jsonl").open(encoding="utf-8"):
        node = json.loads(line)
        if node.get("is_test") or node.get("is_doc"):
            continue
        kind = node.get("symbol_type") or ""
        source = node.get("source_text") or ""
        if len(source) < 100:
            continue
        is_function_constant = (
            kind == "constant"
            and node.get("language") in {"javascript", "typescript"}
            and re.search(r"\bfunction\b|=>", source[:200])
            and len(source) >= 200
        )
        is_long_method = kind == "method" and len(source) >= 300
        if kind not in KINDS and not is_function_constant and not is_long_method:
            continue
        if re.search(r"(^|/)(tests?|spec|__tests__)/", node.get("rel_path") or ""):
            continue
        out.append(node)
    out.sort(key=lambda n: n["node_id"])
    return out


def identifiers(node: dict) -> list[str]:
    names = {node.get("symbol_name") or ""}
    names.update(re.split(r"[.:#]", node.get("symbol_name") or ""))
    return [n for n in names if len(n) >= 4]


def acceptable(question: str, node: dict, seen: set[frozenset]) -> str | None:
    """The reason a question is rejected, or None."""
    q = question.strip()
    if "\n" in q or not q.endswith("?") or q.count("?") != 1:
        return "shape"
    words = q.split()
    if not 6 <= len(words) <= 40:
        return "length"
    lowered = q.lower()
    for name in identifiers(node):
        if name.lower() in lowered:
            return "names the identifier"
    stem = Path(node.get("rel_path") or "").stem.lower()
    if len(stem) >= 4 and stem in lowered:
        return "names the file"
    if re.search(r"\btests?\b|\bunit test", lowered):
        return "about tests"
    bag = frozenset(w.lower().strip(",.?!'\"()") for w in words)
    for other in seen:
        if len(bag & other) / len(bag | other) > 0.7:
            return "near duplicate"
    seen.add(bag)
    return None


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("graph", type=Path)
    parser.add_argument("corpus")
    parser.add_argument("commit")
    parser.add_argument("out", type=Path)
    parser.add_argument("--repo", required=True, help="owner/name, shown to the model")
    parser.add_argument("--api-base", required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--candidates", type=int, default=60)
    parser.add_argument("--keep", type=int, default=36)
    parser.add_argument("--seed", type=int, default=20261006)
    args = parser.parse_args()

    pool = candidates(args.graph)
    rng = random.Random(args.seed)
    sample = rng.sample(pool, min(args.candidates, len(pool)))
    print(f"{args.corpus}: {len(pool)} candidates, sampled {len(sample)}", file=sys.stderr)
    seen: set[frozenset] = set()
    kept, rejected = [], []
    for node in sample:
        if len(kept) >= args.keep:
            break
        source = "\n".join((node.get("source_text") or "").split("\n")[:80])
        prompt = PROMPT.format(
            repo=args.repo, kind=node.get("symbol_type"), path=node.get("rel_path"),
            source=source, name=node.get("symbol_name"),
        )
        question = chat(args.api_base, args.model, prompt).strip().strip('"').strip()
        reason = acceptable(question, node, seen)
        if reason and reason != "near duplicate":
            # One retry, told what was wrong; the rules then decide again.
            retry = prompt + f"\n\nYour previous answer was rejected ({reason}): {question}"
            question = chat(args.api_base, args.model, retry).strip().strip('"').strip()
            reason = acceptable(question, node, seen)
        if reason:
            rejected.append({"node_id": node["node_id"], "question": question, "reason": reason})
            continue
        kept.append({
            "id": f"{args.corpus}-{len(kept) + 1:02d}",
            "corpus": args.corpus,
            "commit": args.commit,
            "question": question,
            "gold_node_ids": [node["node_id"]],
            "gold_files": [node["rel_path"]],
            "symbol_name": node.get("symbol_name"),
            "symbol_type": node.get("symbol_type"),
            "ask_subset": len(kept) < 15,
        })
    args.out.parent.mkdir(parents=True, exist_ok=True)
    with args.out.open("w", encoding="utf-8") as f:
        for item in kept:
            f.write(json.dumps(item, ensure_ascii=False) + "\n")
    with args.out.with_suffix(".rejected.jsonl").open("w", encoding="utf-8") as f:
        for item in rejected:
            f.write(json.dumps(item, ensure_ascii=False) + "\n")
    print(f"{args.corpus}: kept {len(kept)}, rejected {len(rejected)}", file=sys.stderr)
    return 0 if len(kept) >= 30 else 1


if __name__ == "__main__":
    raise SystemExit(main())
