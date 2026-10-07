#!/usr/bin/env python3
"""Print the report's markdown tables from ``analyze.py``'s summary.json."""

from __future__ import annotations

import json
import sys
from pathlib import Path

ENGINES = [("rust", "Rust native"), ("python-patched", "Python (dim patched)"), ("python-shipped", "Python (as shipped)")]
CORPORA = [("petclinic", "spring-petclinic"), ("cleanarch", "CleanArchitecture"), ("leveldb", "leveldb"), ("express", "express")]


def f(value, digits=1, suffix=""):
    if value is None:
        return "–"
    if isinstance(value, float):
        return f"{value:.{digits}f}{suffix}"
    return f"{value}{suffix}"


def main() -> int:
    path = Path(sys.argv[1]) if len(sys.argv) > 1 else Path.home() / ".cache/elitea-dw-bench/summary.json"
    s = json.loads(path.read_text())

    print("### Generation (wall time, seconds)\n")
    print("| Corpus | Engine | Total | Engine time | Model time | Clone+index (to embeddings) | Embed | Phase 2 | Phase 3 | Analysis+structure | Pages | Publish | Pages ok/failed | Chat tokens out (reasoning) |")
    print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for c, cname in CORPORA:
        for e, ename in ENGINES:
            r = s.get(c, {}).get(e, {}).get("generate")
            if not r:
                continue
            p = r["phases"]
            m = r["model"]
            idx = (p.get("clone") or 0) + (p.get("index") or 0)
            print(f"| {cname} | {ename} | {f(r['wall_s'],0)} | {f(r['engine_s'],0)} | {f(m['model_s'],0)} | {f(idx)} | "
                  f"{f(p.get('embed'))} | {f(p.get('phase2'))} | {f(p.get('phase3'))} | {f(p.get('analysis+structure'),0)} | "
                  f"{f(p.get('pages'),0)} | {f(p.get('publish'))} | {r.get('pages')}/{r.get('errors')} | "
                  f"{m['completion_tokens']:,} ({m['reasoning_tokens']:,}) |")

    print("\n### Memory, cold start, ask latency\n")
    print("| Corpus | Engine | Cold start (s) | Peak RSS generate (MB, process tree) | Peak RSS ask (MB) | `time -l` max RSS (MB) | ask p50 / p95 wall (s) | ask p50 / p95 excl. model (s) | ask ok / empty |")
    print("|---|---|---|---|---|---|---|---|---|")
    for c, cname in CORPORA:
        for e, ename in ENGINES:
            r = s.get(c, {}).get(e)
            if not r:
                continue
            rss = r.get("rss_peak_tree_mb") or {}
            a = r.get("ask") or {}
            print(f"| {cname} | {ename} | {f(r.get('cold_start_s'),2)} | {f(rss.get('generate'),0)} | {f(rss.get('ask'),0)} | "
                  f"{f(r.get('time_l_max_rss_mb'),0)} | {f(a.get('wall_p50_s'))} / {f(a.get('wall_p95_s'))} | "
                  f"{f(a.get('engine_p50_s'),2)} / {f(a.get('engine_p95_s'),2)} | {a.get('success','–')}/{a.get('empty_answers','–')} of {a.get('n','–')} |")

    print("\n### Retrieval (fixed question sets, top 10)\n")
    print("| Corpus | Engine | n | Vectors in index | node recall@10 | node MRR@10 | file recall@10 | file MRR@10 | search p50 (ms, excl. embedding) |")
    print("|---|---|---|---|---|---|---|---|---|")
    for c, cname in CORPORA:
        for e, ename in ENGINES + [("python-patched-textemb", "Python (dim patched, TEXT embeddings — diagnostic)")]:
            r = (s.get(c, {}).get(e) or {}).get("retrieval")
            if not r:
                continue
            print(f"| {cname} | {ename} | {r['n']} | {f(r.get('index_vectors'))} | {f(r['node_recall@10'],2)} | {f(r['node_mrr@10'],2)} | "
                  f"{f(r['file_recall@10'],2)} | {f(r['file_mrr@10'],2)} | {f(r.get('search_ms_p50'),1)} |")

    print("\n### Structure\n")
    print("| Corpus | Engine | Sections | Pages | Words | Arch. symbols named in pages | Arch. files cited | Mermaid blocks |")
    print("|---|---|---|---|---|---|---|---|")
    for c, cname in CORPORA:
        for e, ename in ENGINES:
            r = (s.get(c, {}).get(e) or {}).get("coverage")
            if not r:
                continue
            print(f"| {cname} | {ename} | {f(r.get('sections'))} | {r['pages']} | {r['words']:,} | "
                  f"{r['symbols_covered']}/{r['arch_symbols']} ({f(100*(r['symbol_coverage'] or 0),0)}%) | "
                  f"{r['files_cited']}/{r['arch_files']} ({f(100*(r['file_coverage'] or 0),0)}%) | {r['mermaid_blocks']} |")

    print("\n### Answer quality (blinded pairwise judge, both orders)\n")
    print("| Corpus | Pair | Questions | Rust wins | Ties | Python wins | Order-swap agreement | Mean faithfulness (Rust / Python) | Mean helpfulness (Rust / Python) |")
    print("|---|---|---|---|---|---|---|---|---|")
    for c, cname in CORPORA:
        for pair, j in sorted((s.get(c, {}).get("_judge") or {}).items()):
            ms = j.get("mean_scores") or {}
            a, b = ms.get(j["first"]) or {}, ms.get(j["second"]) or {}
            print(f"| {cname} | {j['first']} vs {j['second']} | {j['questions']} | {j['first_wins']} | {j['ties']} | {j['second_wins']} | "
                  f"{f(100*(j['order_swap_agreement'] or 0),0)}% of {j['pairs_with_both_verdicts']} | "
                  f"{f(a.get('faithfulness'),2)} / {f(b.get('faithfulness'),2)} | {f(a.get('helpfulness'),2)} / {f(b.get('helpfulness'),2)} |")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
