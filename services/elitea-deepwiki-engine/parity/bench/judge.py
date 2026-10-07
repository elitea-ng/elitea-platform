#!/usr/bin/env python3
"""Blinded pairwise judging of two engines' ``ask`` answers (benchmark-2026-10).

For every question both engines answered, the chat model judges the pair
TWICE, once in each order (the engine shown as "A" swaps), the two calls in a
seeded random order. The judge sees the question, the gold code (the source
of the symbol the question was generated from, from the engine-independent
graph dump) and the two answers, never the engine names, and returns JSON:
a 1-5 faithfulness and helpfulness score per answer and a winner (A, B or
tie). Rubric: faithfulness to the code first (claims the gold code or the
repository contradicts, invented identifiers), helpfulness second.

Per question the two verdicts are mapped back to engines. They AGREE when
both name the same engine or both say tie; the final verdict is the agreed
one, and a disagreement counts as a tie. Reported: win/tie/loss for the
first engine, the order-swap agreement rate, and mean scores.

    python parity/bench/judge.py <questions.jsonl> <graph-dir> \\
        <first-engine>=<ask.jsonl> <second-engine>=<ask.jsonl> <out.jsonl> \\
        --api-base http://127.0.0.1:18950/v1 --model RadixArk/Qwen3.8-27B-NVFP4
"""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import random
import re
import urllib.request
from pathlib import Path

PROMPT = """You are an expert code reviewer judging two answers to a developer's question \
about the repository "{repo}". The answers come from two systems; you do not know which.

Question:
{question}

Reference code (the code the question is about; the repository contains more):
```
{gold}
```

Answer A:
<<<
{a}
>>>

Answer B:
<<<
{b}
>>>

Judge each answer on:
1. Faithfulness (most important): are its claims about the code correct and consistent \
with the reference code? Invented functions, files or behaviour are serious errors.
2. Helpfulness: does it actually answer the question, pointing to the right code?
An empty answer or an error message scores 1 on both.

Reply with ONLY a JSON object, no prose:
{{"faithfulness_a": <1-5>, "helpfulness_a": <1-5>, "faithfulness_b": <1-5>, \
"helpfulness_b": <1-5>, "winner": "A" | "B" | "tie"}}"""


def chat(api_base: str, model: str, prompt: str) -> str:
    body = {
        "model": model, "messages": [{"role": "user", "content": prompt}],
        "temperature": 0, "seed": 11, "max_tokens": 8000, "reasoning_effort": "medium",
    }
    request = urllib.request.Request(
        f"{api_base}/chat/completions", data=json.dumps(body).encode(),
        headers={"content-type": "application/json"},
    )
    with urllib.request.urlopen(request, timeout=900) as response:
        data = json.load(response)
    return data["choices"][0]["message"].get("content") or ""


def parse(text: str) -> dict | None:
    match = re.search(r"\{.*\}", text, re.S)
    if not match:
        return None
    try:
        verdict = json.loads(match.group(0))
    except ValueError:
        return None
    if verdict.get("winner") not in {"A", "B", "tie"}:
        return None
    return verdict


def clip(text: str, limit: int = 8000) -> str:
    text = text or "(empty answer)"
    return text if len(text) <= limit else text[:limit] + "\n[... truncated]"


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("questions", type=Path)
    parser.add_argument("graph", type=Path)
    parser.add_argument("first")
    parser.add_argument("second")
    parser.add_argument("out", type=Path)
    parser.add_argument("--repo", required=True)
    parser.add_argument("--api-base", required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--concurrency", type=int, default=3)
    parser.add_argument("--seed", type=int, default=20261006)
    args = parser.parse_args()

    names, answers = [], []
    for spec in (args.first, args.second):
        name, path = spec.split("=", 1)
        names.append(name)
        answers.append({json.loads(l)["id"]: json.loads(l) for l in Path(path).open() if l.strip()})
    nodes = {}
    for line in (args.graph / "nodes.jsonl").open(encoding="utf-8"):
        node = json.loads(line)
        nodes[node["node_id"]] = node
    questions = [json.loads(l) for l in args.questions.open() if l.strip()]
    questions = [q for q in questions if q["id"] in answers[0] and q["id"] in answers[1]]

    rng = random.Random(args.seed)
    jobs = []
    for q in questions:
        gold = "\n\n".join(
            f"// {nodes[n]['rel_path']}\n" + "\n".join((nodes[n].get("source_text") or "").split("\n")[:120])
            for n in q["gold_node_ids"] if n in nodes
        )
        texts = [answers[i][q["id"]].get("answer") or (
            "ERROR: " + json.dumps(answers[i][q["id"]].get("error"))) for i in (0, 1)]
        orders = [0, 1]
        rng.shuffle(orders)
        for first_shown in orders:  # which engine is "A"
            a, b = (texts[0], texts[1]) if first_shown == 0 else (texts[1], texts[0])
            prompt = PROMPT.format(repo=args.repo, question=q["question"], gold=gold, a=clip(a), b=clip(b))
            jobs.append((q["id"], first_shown, prompt))

    def run(job):
        qid, first_shown, prompt = job
        verdict = None
        for _ in range(2):
            verdict = parse(chat(args.api_base, args.model, prompt))
            if verdict:
                break
        return qid, first_shown, verdict

    results: dict[str, dict[int, dict | None]] = {}
    with concurrent.futures.ThreadPoolExecutor(args.concurrency) as pool:
        for qid, first_shown, verdict in pool.map(run, jobs):
            results.setdefault(qid, {})[first_shown] = verdict

    def to_engine(verdict: dict | None, first_shown: int) -> tuple[str, dict]:
        """('first'|'second'|'tie'|'invalid', per-engine scores)."""
        if not verdict:
            return "invalid", {}
        a_is = 0 if first_shown == 0 else 1
        mapping = {"A": a_is, "B": 1 - a_is}
        scores = {
            names[mapping["A"]]: (verdict.get("faithfulness_a"), verdict.get("helpfulness_a")),
            names[mapping["B"]]: (verdict.get("faithfulness_b"), verdict.get("helpfulness_b")),
        }
        if verdict["winner"] == "tie":
            return "tie", scores
        return ("first" if mapping[verdict["winner"]] == 0 else "second"), scores

    tally = {"first": 0, "second": 0, "tie": 0}
    agree = 0
    counted = 0
    sums: dict[str, list[float]] = {n: [0.0, 0.0, 0] for n in names}
    with args.out.open("w", encoding="utf-8") as out:
        for q in questions:
            pair = results.get(q["id"], {})
            o0, s0 = to_engine(pair.get(0), 0)
            o1, s1 = to_engine(pair.get(1), 1)
            valid = [o for o in (o0, o1) if o != "invalid"]
            if not valid:
                final = "tie"
            elif len(valid) == 1:
                final = valid[0]
            else:
                counted += 1
                if o0 == o1:
                    agree += 1
                    final = o0
                else:
                    final = "tie"
            tally[final] += 1
            for scores in (s0, s1):
                for name, (f, h) in scores.items():
                    if isinstance(f, (int, float)) and isinstance(h, (int, float)):
                        sums[name][0] += f
                        sums[name][1] += h
                        sums[name][2] += 1
            out.write(json.dumps({"id": q["id"], "order_first_as_A": o0, "order_second_as_A": o1,
                                  "final": final, "verdicts": {str(k): v for k, v in pair.items()}}) + "\n")
    summary = {
        "first": names[0], "second": names[1], "questions": len(questions),
        "first_wins": tally["first"], "ties": tally["tie"], "second_wins": tally["second"],
        "order_swap_agreement": agree / counted if counted else None, "pairs_with_both_verdicts": counted,
        "mean_scores": {n: {"faithfulness": s[0] / s[2], "helpfulness": s[1] / s[2]} if s[2] else None
                        for n, s in sums.items()},
    }
    args.out.with_suffix(".summary.json").write_text(json.dumps(summary, indent=1))
    print(json.dumps(summary))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
