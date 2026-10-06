#!/usr/bin/env python3
"""Build the Mermaid sanitizer corpus the Rust port is held to.

Runs ``diagram_sanitizer.sanitize_content`` (the Python engine's, with the
default ``SanitizerConfig``) over:

* every fenced ```` ```mermaid ```` block in the Markdown files given on the
  command line (real diagrams: design documents, generated wiki pages),
  each as its own page;
* the hand cases below, which break diagrams the way models do (missing
  headers, smart quotes, reserved ids, unquoted labels, escaped quotes,
  generics, sequence aliases, inline fence closers, …), so every repair
  pass runs at least once.

One JSON line per case: ``name``, ``input`` (the page), ``output`` (the
page ``sanitize_content`` returns, or ``null`` when it raised — the page
then keeps its text), ``error`` and per diagram ``status`` and ``fixes``.

    PYTHONPATH=services/elitea-deepwiki/src python parity/python_sanitizer_corpus.py \\
        <out.jsonl> [file.md ...]
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

FENCE = re.compile(r"^```mermaid[^\n]*\n.*?^\s*```", re.DOTALL | re.MULTILINE | re.IGNORECASE)

HAND_CASES: list[tuple[str, str]] = [
    ("valid_flowchart", "```mermaid\nflowchart LR\n  api --> store\n  api --> auth\n```\n"),
    ("no_header_flow", "```mermaid\nA[Start here] --> B[Do (things)]\nB --> C[Done.]\n```\n"),
    ("no_header_sequence", "```mermaid\nClient->>Server: GET /x\nServer-->>Client: 200\n```\n"),
    ("missing_direction", "```mermaid\nflowchart\n  A --> B\n```\n"),
    ("bad_direction", "```mermaid\ngraph XY extra\n  A --> B\n```\n"),
    ("smart_quotes_arrows", "```mermaid\nflowchart TD\n  A[“Quoted” label] → B[‘x’]\n  B ⇒ C ⟶ D\n```\n"),
    ("dotted_arrows", "```mermaid\nflowchart TD\n  A -.-> B\n  B ==> C\n  C --o D\n  D --x E\n  E ---> F\n  F --#> G\n```\n"),
    ("end_node", "```mermaid\nflowchart TD\n  start[\"Begin\"] --> end[\"Finish\"]\n  end_[\"taken\"] --> x\n```\n"),
    ("end_inline", "```mermaid\nflowchart TD\n  A --> end[\"Stop\"]\n```\n"),
    ("arrow_labels", "```mermaid\nflowchart TD\n  A -->|  yes please  | B\n  A --> |\"no\"| C\n  A -->| \"\"double\"\" | D\n```\n"),
    ("orphan_label", "```mermaid\nflowchart TD\n  A --> |\"Yes\"| \"Create Café ()\"\n  A --> |\"No\"| \"!!!\"\n  B[\"x\"] --> |\"go\"| \"Create Café ()\"\n```\n"),
    ("decision_quotes", "```mermaid\nflowchart TD\n  q{\"Is it valid?\"} --> A\n  r{ \"Other\" } --> B\n```\n"),
    ("label_newline", "```mermaid\nflowchart TD\n  A[\"line one\\nline two\"] --> B\n```\n"),
    ("dupe_quotes", "```mermaid\nflowchart TD\n  A[\"\"Label\"\"] -->|\"\"go\"\"| B\n  C -->|\"\"x\"| D\n  E -->|\"y\"\"| F\n```\n"),
    ("return_array", "```mermaid\nflowchart TD\n  A[\"\"Return [\\\"]\"] --> B[\"Return [\\\"]\"]\n  C[\"Return [\"] --> D\n```\n"),
    ("dict_index", "```mermaid\nflowchart TD\n  A[\"config[\\\"key\\\"] value\"] --> B[\"data[\"name\"]\"]\n  C[\"[\\\"'x'\\\"]\"] --> D\n```\n"),
    ("generics", "```mermaid\nflowchart TD\n  A[\"BaseStore[\\\"str\\\", Document]\"] --> B[\"Dict[\\\"k\\\", List[int]]\"]\n  C[\"Map\\\"[K, V]\"] --> D\n```\n"),
    ("escaped_quotes", "```mermaid\nflowchart TD\n  A[\"call \\\"run\\\" now\"] -->|\"say \\\"hi\\\"\"| B\n  C[\"fn(\\\"x\\\")\"] --> D\n```\n"),
    ("dict_value_quotes", "```mermaid\nflowchart TD\n  A[\"{'key': \\\"value\\\"} and Map[\\\"a\\\", b]\"] --> B[\"{'k': \"v\"}\"]\n```\n"),
    ("late_quote", "```mermaid\nflowchart TD\n  A[Load config.yaml] --> B[Parse: tokens]\n  C[plain] --> D[\"ok\"]\n```\n"),
    ("fragments", "```mermaid\nflowchart TD\n  A[\"part1\"<br/>\"part2\"] -->|\"a\"<br/>\"b\"| B\n```\n"),
    ("subgraphs", "```mermaid\nflowchart LR\n  subgraph Core[\"Core\"]\n    api[\"API\"]\n    db[\"DB\"]\n  end\n  subgraph Empty\n  end\n  Core --> Empty\n  x -->|\"y\"| Core[\n```\n"),
    ("sequence_participants", "```mermaid\nsequenceDiagram\n  participant API_\n  API->>DB: query\n  DB-->>API_: rows\n  User->>API_: call\n```\n"),
    ("sequence_reserved", "```mermaid\nsequenceDiagram\n  participant end\n  participant loop\n  end->>loop: tick\n  loop-->>end: tock\n```\n"),
    ("sequence_deactivate", "```mermaid\nsequenceDiagram\n  A->>B: x\n  activate B\n  deactivate B, C\n  deactivate D\n  alt ok\n    B-->>A: y\n    deactivate B\n  else bad\n    deactivate B\n  end\n```\n"),
    ("sequence_return_break", "```mermaid\nsequenceDiagram\n  A->>B: x\n  return\n  break\n  B-->>A: y; z; \"a;b\"\n```\n"),
    ("sequence_note", "```mermaid\nsequenceDiagram\n  A->>B: x\n  Note over A,B: first; second; \"q;r\"\n  note right of B: one\n```\n"),
    ("sequence_opt_else", "```mermaid\nsequenceDiagram\n  A->>B: x\n  opt maybe\n    B-->>A: y\n  else never\n    B-->>A: z\n  end\n  opt alone\n    A->>B: w\n  end\n```\n"),
    ("sequence_continuation", "```mermaid\nsequenceDiagram\n  A->>B: call(a ,  b \\\n    , c )\n  A ->> B: open( x,\n  y )\n  B-->>A:  ok ( 1 ,2 )\n```\n"),
    ("inline_closer", "Text\n```mermaid\nflowchart TD\n  A --> B```\nAfter\n"),
    ("inline_closer_quoted", "```mermaid\nflowchart TD\n  A[\"has ``` inside\"] --> B ``` tail\n```\n"),
    ("unclosed", "Intro\n```mermaid\nflowchart TD\n  A --> B\n"),
    ("two_blocks", "```mermaid\nflowchart TD\nA-->B\n```\nmiddle\n```Mermaid title\ngraph\nC-->D\n```"),
    ("crlf_tabs", "```mermaid\r\nflowchart TD\r\n\tA --> B\r\n\r\n```\r\n"),
    ("blank_edges", "```mermaid\n\n\n  flowchart TD\n  A --> B\n\n\n```\n\n"),
    ("too_large", "```mermaid\nflowchart TD\n" + "  A --> B\n" * 1000 + "```\n"),
    ("class_diagram", "```mermaid\nclassDiagram\n  class Animal {\n    +String name\n  }\n  Animal <|-- Dog\n```\n"),
    ("pie", "```mermaid\npie title Pets\n  \"Dogs\" : 386\n  \"Cats\" : 85\n```\n"),
    ("unknown_header", "```mermaid\nstuff here\n  more\n```\n"),
    ("unicode_labels", "```mermaid\nflowchart TD\n  A[Ünïcödé label] --> B[日本語]\n  B --> C[\"ok\"]\n```\n"),
    ("trailing_after_fence", "```mermaid\nflowchart TD\n  A --> B\n```   \nnext"),
    ("no_newline_end", "```mermaid\nflowchart TD\n  A --> B\n```"),
    ("double_newline_after", "```mermaid\nflowchart TD\n  A --> B\n```\n\nText"),
    ("nested_brackets", "```mermaid\nflowchart TD\n  A[\"list[str] of [\"x\", 'y']\"] --> B[\"f(\\\"a\\\", [1, 2])\"]\n```\n"),
    ("quote_before_indexer", "```mermaid\nflowchart TD\n  A[\"obj\\\"['k']\"] --> B[\"x\" ['y']]\n```\n"),
    ("pipe_spaces", "```mermaid\nflowchart TD\n  A -- text --> B\n  B -->|  spaced   | C\n```\n"),
    ("subgraph_digit_rep", "```mermaid\nflowchart TD\n  subgraph S1\n    2abc[\"x\"]\n  end\n  S1 --> Z\n```\n"),
    # The template `\1<digits>\3` fails before the search: no edge names S1.
    ("subgraph_digit_rep_no_edge", "```mermaid\nflowchart TD\n  subgraph S1\n    2abc[\"x\"]\n  end\n  A --> Z\n```\n"),
    # re.IGNORECASE: `i` also matches U+0130 and U+0131.
    ("ignorecase_dotted_i", "```mermaİd\nsequenceDiagram\n  A->>B: x\n  Note rİght of B: one; two\n```\n"),
    ("ignorecase_dotless_i", "```MERMAıD\nflowchart TD\n  A[x y] --> B\n```\n"),
    ("ignorecase_dotless_i_indented", "Intro\n  ```mermaıd\nsequenceDiagram\n  A->>B: x\n  note rıght of B: one; two\n```\n"),
    ("ignorecase_note_mixed", "```mermaid\nsequenceDiagram\n  A->>B: x\n  NOTE LEFT OF B: one; two\n  Note rİght of A: three; four\n```\n"),
]


def main() -> int:
    if len(sys.argv) < 2:
        print(__doc__, file=sys.stderr)
        return 2
    from elitea_deepwiki.engine.diagram_sanitizer import SanitizerConfig, sanitize_content  # noqa: PLC0415

    cases = list(HAND_CASES)
    for path in sys.argv[2:]:
        text = Path(path).read_text(encoding="utf-8", errors="replace")
        for index, match in enumerate(FENCE.finditer(text)):
            cases.append((f"{Path(path).name}#{index}", match.group(0) + "\n"))
        # A whole generated page too, when it is a wiki page.
        if path.endswith(".md") and "wiki_pages" in path:
            cases.append((f"{Path(path).name}#page", text))

    with open(sys.argv[1], "w", encoding="utf-8") as out:
        for name, page in cases:
            try:
                output, summary = sanitize_content(page, SanitizerConfig())
                record = {
                    "name": name,
                    "input": page,
                    "output": output,
                    "error": None,
                    "diagrams": [{"status": r.status, "fixes": r.fixes, "hash": r.hash} for r in summary.records],
                }
            except Exception as exc:  # noqa: BLE001 - recorded
                record = {"name": name, "input": page, "output": None, "error": repr(exc), "diagrams": []}
            out.write(json.dumps(record, ensure_ascii=False) + "\n")
    print(f"{len(cases)} cases")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
