#!/usr/bin/env python3
"""The page gate: compare a Python dump with ``deepwiki-parity pages``.

    python3 parity/compare_pages.py <python-dump-dir> <rust-out-dir>

Checks, and prints one line each:

* every chat request: the same number, and per request the ``messages``
  byte-identical (the page prompt: context assembly, truncation,
  ordering), plus ``model``, ``temperature``, ``max_completion_tokens``
  and ``stream``;
* the structure after the split and every page (id, title, content,
  status) and the error lines;
* every artifact byte-identical after normalising what is time- or
  uuid-derived: the structure file's timestamp, the manifest's version id,
  ``created_at``, ``analysis_key`` and ``analysis_cache_key``. The
  manifest's ``pages`` list is compared as a set: Python listed files in
  the temp directory's own order (a recorded deliberate difference);
* the top-level result fields (``execution_time`` and the timing inside
  ``result`` normalised).

Exit status 0 when everything is equal.
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

VERSION = re.compile(r"\d{8}T\d{6}Z-[0-9a-f]{8}")
STAMP = re.compile(r"wiki_structure_\d{8}_\d{6}\.json")
EXEC = re.compile(r"Execution Time: [0-9.]+s")


def load_jsonl(path: Path) -> list:
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]


def norm(text: str) -> str:
    return STAMP.sub("wiki_structure_TS.json", VERSION.sub("VERSION", text))


def manifest_body(data: str) -> dict:
    body = json.loads(data)
    body["created_at"] = "CREATED"
    body["analysis_cache_key"] = "MD5"
    body["wiki_version_id"] = "VERSION"
    body["analysis_key"] = VERSION.sub("VERSION", body["analysis_key"])
    body["pages"] = sorted(body["pages"])
    return body


def main() -> int:
    ref, out = Path(sys.argv[1]), Path(sys.argv[2])
    failures = 0

    def report(name: str, ok: bool, detail: str = "") -> None:
        nonlocal failures
        print(f"{'OK  ' if ok else 'DIFF'} {name}{(' — ' + detail) if detail else ''}")
        failures += 0 if ok else 1

    py_req, rs_req = load_jsonl(ref / "requests.jsonl"), load_jsonl(out / "requests.jsonl")
    # Rust drafts four pages at a time, so its requests arrive in any
    # order: pair them by the page each one is for.
    page_of = re.compile(r"^- Page: (.*)$", re.MULTILINE)

    def by_page(requests: list) -> list:
        return sorted(requests, key=lambda r: page_of.search(r["messages"][-1]["content"]).group(1))

    py_req, rs_req = by_page(py_req), by_page(rs_req)
    report("request count", len(py_req) == len(rs_req), f"{len(py_req)} / {len(rs_req)}")
    differing = []
    for i, (a, b) in enumerate(zip(py_req, rs_req)):
        if a["messages"] != b["messages"]:
            differing.append(i)
        for key in ("model", "temperature", "stream"):
            if a.get(key) != b.get(key):
                differing.append(i)
        if a.get("max_completion_tokens", a.get("max_tokens")) != b.get("max_completion_tokens"):
            differing.append(i)
    report("page prompts byte-identical", not differing, f"{len(py_req) - len(set(differing))}/{len(py_req)} equal")
    if differing:
        i = differing[0]
        a = py_req[i]["messages"][-1]["content"]
        b = rs_req[i]["messages"][-1]["content"]
        at = next((k for k in range(min(len(a), len(b))) if a[k] != b[k]), min(len(a), len(b)))
        print(f"     first difference in request {i} at char {at}:\n     py: {a[max(0, at - 200):at + 200]!r}\n     rs: {b[max(0, at - 200):at + 200]!r}")

    py_pages = json.loads((ref / "pages.json").read_text(encoding="utf-8"))
    rs_pages = json.loads((out / "pages.json").read_text(encoding="utf-8"))
    report("structure after split", py_pages["structure_after_split"] == rs_pages["structure_after_split"])
    report("pages", py_pages["pages"] == rs_pages["pages"], f"{len(py_pages['pages'])} pages")
    report("errors", py_pages["errors"] == rs_pages["errors"])

    py_res = json.loads((ref / "result.json").read_text(encoding="utf-8"))
    rs_res = json.loads((out / "result.json").read_text(encoding="utf-8"))
    py_art = {norm(a["name"]): a for a in py_res["artifacts"]}
    rs_art = {norm(a["name"]): a for a in rs_res["artifacts"]}
    report("artifact names", set(py_art) == set(rs_art), f"{len(py_art)} / {len(rs_art)}")
    equal = 0
    for name in sorted(set(py_art) & set(rs_art)):
        a, b = py_art[name], rs_art[name]
        if "wiki_manifest_" in name:
            same = manifest_body(a["data"]) == manifest_body(b["data"]) and a.get("object_type") == b.get("object_type")
            # The bytes too, once the volatile values are put back.
            text = b["data"]
            for key in ("created_at", "wiki_version_id", "analysis_key", "analysis_cache_key"):
                text = text.replace(json.loads(b["data"])[key], json.loads(a["data"])[key])
            bytes_equal_but_pages = json.loads(text)["pages"] and (
                re.sub(r'"pages": \[[^\]]*\]', "", text) == re.sub(r'"pages": \[[^\]]*\]', "", a["data"])
            )
            same = same and bool(bytes_equal_but_pages)
        else:
            same = a["data"] == b["data"] and a.get("type") == b.get("type")
        equal += same
        if not same:
            print(f"     artifact differs: {name}")
    report("artifacts byte-identical (normalised)", equal == len(set(py_art) & set(rs_art)), f"{equal}/{len(py_art)}")

    def top(result: dict) -> dict:
        keep = {k: v for k, v in result.items() if k != "artifacts"}
        keep["execution_time"] = 0
        keep["result"] = EXEC.sub("Execution Time: Ts", keep["result"])
        for key in ("wiki_version_id", "analysis_key"):
            keep[key] = VERSION.sub("VERSION", keep[key])
        return keep

    report("result fields", top(py_res) == top(rs_res))
    if top(py_res) != top(rs_res):
        for key in sorted(set(top(py_res)) | set(top(rs_res))):
            if top(py_res).get(key) != top(rs_res).get(key):
                print(f"     {key}: py={top(py_res).get(key)!r:.200} rs={top(rs_res).get(key)!r:.200}")
    report("result key order", list(py_res) == list(rs_res))

    py_s = json.loads((ref / "summary.json").read_text())
    rs_s = json.loads((out / "summary.json").read_text())
    print(
        f"TIME pages python {py_s['page_seconds']:.3f}s rust {rs_s['page_seconds']:.3f}s; "
        f"context python {py_s.get('context_seconds', float('nan')):.3f}s rust {rs_s['context_seconds']:.3f}s"
    )
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(main())
