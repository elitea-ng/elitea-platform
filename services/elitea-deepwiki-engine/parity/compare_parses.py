#!/usr/bin/env python3
"""Compare one Rust parser's ParseResults with the Python parser's, file by file.

    python parity/compare_parses.py files  <reference.jsonl> <files.txt>
    python parity/compare_parses.py compare <reference.jsonl> <candidate.jsonl> [--examples 10] [--json out.json]

``files`` writes the reference's file list (what ``deepwiki-parity
parse-dump`` should parse). ``compare`` reports, per field:

* symbols matched by (name, symbol_type, start line, parent_symbol): Jaccard,
  and on matched symbols the agreement of every other field;
* relationships matched by (source, target, type, source line): Jaccard, and
  counts by type on each side;
* the worst files, so a port can be fixed file by file.

A Python ``metadata`` or ``annotations`` of ``None`` reads as ``{}``: the
Rust model has no ``None`` there, and the graph builder reads both as ``{}``.

The parser gate is a symbol Jaccard and a relationship Jaccard of at least
0.95 (``--min``), exit 1 below it.
"""

from __future__ import annotations

import argparse
import collections
import json
import sys


def load(path):
    rows = {}
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            row = json.loads(line)
            for symbol in row["symbols"]:
                if symbol.get("metadata") is None:
                    symbol["metadata"] = {}
            for relationship in row["relationships"]:
                if relationship.get("annotations") is None:
                    relationship["annotations"] = {}
            rows[row["file_path"]] = row
    return rows


def sym_key(s):
    start = (s.get("range") or {}).get("start") or {}
    return (s["name"], s["symbol_type"], start.get("line"), s.get("parent_symbol"))


def rel_key(r):
    start = ((r.get("source_range") or {}).get("start") or {}).get("line")
    return (r["source_symbol"], r["target_symbol"], r["relationship_type"], start)


SYMBOL_FIELDS = ("full_name", "scope", "visibility", "is_static", "is_abstract", "is_async", "docstring",
                 "return_type", "parameter_types", "signature", "source_text", "range", "metadata")
REL_FIELDS = ("target_file", "confidence", "weight", "is_direct", "annotations", "source_file")


def jaccard(a, b):
    return 1.0 if not a and not b else len(a & b) / len(a | b)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("files", "compare"))
    parser.add_argument("reference")
    parser.add_argument("other")
    parser.add_argument("--examples", type=int, default=10)
    parser.add_argument("--min", type=float, default=0.95)
    parser.add_argument("--json")
    args = parser.parse_args()

    reference = load(args.reference)
    if args.mode == "files":
        with open(args.other, "w", encoding="utf-8") as handle:
            for path in sorted(reference):
                handle.write(path + "\n")
        print(f"{len(reference)} files")
        return 0

    candidate = load(args.other)
    missing_files = sorted(set(reference) - set(candidate))
    sym_ref, sym_cand, rel_ref, rel_cand = set(), set(), collections.Counter(), collections.Counter()
    sym_rows = {}
    rel_rows = {}
    field_same = collections.Counter()
    field_total = collections.Counter()
    rel_field_same = collections.Counter()
    rel_field_total = collections.Counter()
    per_file = []
    types_ref, types_cand = collections.Counter(), collections.Counter()
    sym_types_ref, sym_types_cand = collections.Counter(), collections.Counter()
    for path, ref in reference.items():
        cand = candidate.get(path, {"symbols": [], "relationships": []})
        rs = {(path,) + sym_key(s): s for s in ref["symbols"]}
        cs = {(path,) + sym_key(s): s for s in cand["symbols"]}
        rr = {(path,) + rel_key(r): r for r in ref["relationships"]}
        cr = {(path,) + rel_key(r): r for r in cand["relationships"]}
        for s in ref["symbols"]:
            sym_types_ref[s["symbol_type"]] += 1
        for s in cand["symbols"]:
            sym_types_cand[s["symbol_type"]] += 1
        for r in ref["relationships"]:
            types_ref[r["relationship_type"]] += 1
        for r in cand["relationships"]:
            types_cand[r["relationship_type"]] += 1
        sym_ref |= set(rs)
        sym_cand |= set(cs)
        rel_ref.update(rr.keys())
        rel_cand.update(cr.keys())
        for key in set(rs) & set(cs):
            for field in SYMBOL_FIELDS:
                field_total[field] += 1
                if rs[key].get(field) == cs[key].get(field):
                    field_same[field] += 1
                else:
                    sym_rows.setdefault(field, (key, rs[key].get(field), cs[key].get(field)))
        for key in set(rr) & set(cr):
            for field in REL_FIELDS:
                rel_field_total[field] += 1
                if rr[key].get(field) == cr[key].get(field):
                    rel_field_same[field] += 1
                else:
                    rel_rows.setdefault(field, (key, rr[key].get(field), cr[key].get(field)))
        score = jaccard(set(rs), set(cs)) + jaccard(set(rr), set(cr))
        per_file.append((score / 2, path, len(set(rs) ^ set(cs)), len(set(rr) ^ set(cr))))

    sj = jaccard(sym_ref, sym_cand)
    rj = jaccard(set(rel_ref), set(rel_cand))
    report = {
        "files": {"reference": len(reference), "candidate": len(candidate), "missing": missing_files[: args.examples]},
        "symbols": {"reference": len(sym_ref), "candidate": len(sym_cand), "jaccard": round(sj, 4)},
        "relationships": {"reference": len(rel_ref), "candidate": len(rel_cand), "jaccard": round(rj, 4)},
        "symbol_types": {t: [sym_types_ref[t], sym_types_cand[t]] for t in sorted(set(sym_types_ref) | set(sym_types_cand))},
        "relationship_types": {t: [types_ref[t], types_cand[t]] for t in sorted(set(types_ref) | set(types_cand))},
        "symbol_field_agreement": {f: round(field_same[f] / field_total[f], 4) for f in field_total},
        "relationship_field_agreement": {f: round(rel_field_same[f] / rel_field_total[f], 4) for f in rel_field_total},
        "first_symbol_field_mismatch": {f: [list(v[0]), v[1], v[2]] for f, v in sym_rows.items()},
        "first_relationship_field_mismatch": {f: [list(v[0]), v[1], v[2]] for f, v in rel_rows.items()},
        "symbols_only_reference": [list(k) for k in sorted(sym_ref - sym_cand, key=str)[: args.examples]],
        "symbols_only_candidate": [list(k) for k in sorted(sym_cand - sym_ref, key=str)[: args.examples]],
        "relationships_only_reference": [list(k) for k in sorted(set(rel_ref) - set(rel_cand), key=str)[: args.examples]],
        "relationships_only_candidate": [list(k) for k in sorted(set(rel_cand) - set(rel_ref), key=str)[: args.examples]],
        "worst_files": [[round(s, 3), p, ds, dr] for s, p, ds, dr in sorted(per_file)[: args.examples]],
    }
    text = json.dumps(report, indent=1, ensure_ascii=False, default=str)
    if args.json:
        with open(args.json, "w", encoding="utf-8") as handle:
            handle.write(text + "\n")
    print(text[:20000])
    ok = sj >= args.min and rj >= args.min and not missing_files
    print(f"\nparser gate: {'PASS' if ok else 'FAIL'} (symbols {sj:.4f}, relationships {rj:.4f}, min {args.min})")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
