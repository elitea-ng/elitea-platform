#!/usr/bin/env python3
"""Generate cases.json: pipeline YAML documents at and just past the expansion
budget a stored pipeline is parsed under.

The Worker enforces the budget at run (libs/rust/agent-runtime/src/graph/mod.rs
PIPELINE_YAML_BUDGET via src/bounded_yaml.rs) and Main enforces the same budget
at save (services/elitea-main/internal/domain/pipelinelimits/expansion.go). Both
test suites read this file, so the two cannot disagree on a boundary.

Counting rules (after anchor and alias expansion): every scalar, sequence and
mapping is one node, mapping keys included; string scalars add their UTF-8
bytes; each sequence or mapping nests one level; the parser replays at most
100 aliases per event the document holds.

Run from this directory: python3 generate.py
"""
import json

NODES = 131_072
SCALAR_BYTES = 1024 * 1024
DEPTH = 64


def flow(items):
    return "[" + ", ".join(items) + "]"


def nodes_case(extra):
    # root(1) + a(1) + x(1+k) + b(1) + b-seq(1) + m*(1+k) + c(1) + c-seq(1) + p
    k, m = 1000, 129
    fixed = 7 + k + m * (1 + k)
    p = NODES - fixed + extra
    return (f"a: &x {flow(['0'] * k)}\n"
            f"b: {flow(['*x'] * m)}\n"
            f"c: {flow(['0'] * p)}\n")


def bytes_case(extra):
    # keys s, t, u (3 bytes) + (m + 1) * L + pad
    length, m = 4096, 254
    pad = SCALAR_BYTES - 3 - (m + 1) * length + extra
    return (f"s: &s {'a' * length}\n"
            f"t: {flow(['*s'] * m)}\n"
            f"u: {'b' * pad}\n")


def depth_case(levels):
    # the root mapping is level 1
    return "d: " + "[" * (levels - 1) + "]" * (levels - 1) + "\n"


def replay_case(n):
    # x1..x3 each hold ten aliases to the previous level; x4 holds n aliases to
    # x3. Expansion stays far under the node budget; alias replays do not.
    lines = ["x0: &x0 [0]"]
    for level in range(1, 4):
        lines.append(f"x{level}: &x{level} {flow([f'*x{level - 1}'] * 10)}")
    lines.append(f"x4: {flow(['*x3'] * n)}")
    return "\n".join(lines) + "\n"


CASES = [
    ("nodes at the limit", "accept", None, nodes_case(0)),
    ("nodes one past the limit", "refuse", "nodes", nodes_case(1)),
    ("scalar bytes at the limit", "accept", None, bytes_case(0)),
    ("scalar bytes one past the limit", "refuse", "scalar_bytes", bytes_case(1)),
    ("depth at the limit", "accept", None, depth_case(DEPTH)),
    ("depth one past the limit", "refuse", "depth", depth_case(DEPTH + 1)),
    ("depth reached through an alias", "refuse", "depth",
     "a: &d " + "[" * 40 + "]" * 40 + "\nb: " + "[" * 30 + "*d" + "]" * 30 + "\n"),
    ("self-referencing anchor", "refuse", "depth", "a: &a [*a]\n"),
    ("alias replays under the parser guard", "accept", None, replay_case(3)),
    ("alias replays past the parser guard", "refuse", "nodes", replay_case(4)),
    ("alias bomb", "refuse", "nodes", f"a: &x {flow(['0'] * 1000)}\nb: {flow(['*x'] * 2000)}\n"),
]

with open("cases.json", "w", encoding="utf-8") as out:
    json.dump({
        "budget": {"nodes": NODES, "scalar_bytes": SCALAR_BYTES, "depth": DEPTH},
        "cases": [{"name": n, "verdict": v, "limit": l, "yaml": y} for n, v, l, y in CASES],
    }, out, indent=1)
    out.write("\n")
