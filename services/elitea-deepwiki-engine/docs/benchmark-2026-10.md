# DeepWiki engine benchmark, 2026-10: Python vs Rust-native (ADR-0026)

The ADR-0026 benchmark, measured on 2026-10-06. It is a **partial** run: the
owner stopped it after spring-petclinic, because one generation with a
reasoning model takes 80 to 120 minutes. What was and was not measured is
stated in each section; nothing below is extrapolated.

## Set-up

- **Engines.**
  - `rust`: this crate, release build, the native runner.
  - `python-shipped`: `services/elitea-deepwiki` as shipped.
  - `python-patched`: the same, with `parity/bench/pypatch` lifting the fixed
    1536-dimension vector table, so it can store this model's 2560-dimension
    vectors.

  Each engine served the sidecar socket and was driven through it by
  `parity/bench/run_engine.py`, the same requests the Go host sends.
- **Models.** On one LAN box, behind `parity/model_router.py`, which logs
  every request so model time can be separated from engine time:
  - chat `RadixArk/Qwen3.8-27B-NVFP4` (vLLM, reasoning on);
  - embeddings `Qwen/Qwen3-Embedding-4B` (vLLM, 2560 dimensions, 8192-token
    context).
- **Host.** Apple Silicon (arm64), macOS, PostgreSQL 16 with pgvector 0.8.5
  in podman.
- **Corpora.**
  - spring-projects/spring-petclinic at `500158f7`: Java.
  - elitea-platform: the graph and the analysis phases only, no model.

  express, cleanarch and leveldb were planned; see *Not measured*.
- **Questions.** `parity/questions/<corpus>.jsonl`, built by
  `parity/bench/make_questions.py` from each corpus's own code graph. Each
  question names the symbols and files that answer it.

Reproduce with `parity/bench/run_all.sh`, then run `analyze.py` and
`report_tables.py`.

## Results

### Generation, spring-petclinic

| Engine | Wall time | Engine time | Model time | Pages (ok / failed) | Chat tokens out (reasoning) |
|---|---|---|---|---|---|
| Rust native | 4,955 s (82.6 min) | 11 s | 4,944 s | 17 / 1 | 498,562 (323,147) |
| Python, patched | 6,862 s (114.4 min) | 9 s | 6,853 s | 18 / 0 | 474,017 (244,816) |
| Python, as shipped | 6,976 s (116.3 min) | 11 s | 6,965 s | 26 / 0 | 506,527 (353,822) |

A generation is model-bound: the engine's own time is about 10 s of 80 to
116 minutes on every engine. The wall-time difference comes from how many
model calls each run makes, and how long the model takes over them. Both
planners are not deterministic, so the page counts differ between runs too.

The one failed Rust page hit the 30-minute total stream limit. A reasoning
model writing a 48k-token page streams for longer than that. The limit is
2 h since `7b7757965`, and `ELITEA_DEEPWIKI_MODEL_STREAM_TOTAL_SECONDS`
sets it.

### Memory, cold start and `ask`, spring-petclinic

| Engine | Cold start | Peak RSS, generate | Peak RSS, ask | `ask` p50 / p95 | `ask` p50 / p95 excl. model | `ask` answered |
|---|---|---|---|---|---|---|
| Rust native (phase 7 build) | 0.30 s | 126 MB | 45 MB | 17.5 / 30.4 s | 0.06 / 0.09 s | 13 of 15, 2 blank |
| Python, patched | 0.28 s | 667 MB | 623 MB | 54.0 / 69.7 s | 6.80 / 12.17 s | 15 of 15 |
| Python, as shipped | 1.25 s | 715 MB | 524 MB | 4.3 / 7.0 s | — | 0 of 15 |

The memory figures are the peak RSS of the whole process tree, which for
Rust includes the generation worker.

- **The two blank Rust answers** (`petclinic-04`, `petclinic-06`) came from
  the forced last turn: told to answer, the reasoning model sent only
  `"\n\n"`. Since `7f9c02214` this fails the `ask` with a `RuntimeError`
  instead of succeeding with an empty answer. The captured exchange is
  `capture/0049-0053.json` in the run directory.
- **Python as shipped** answered nothing. Every `ask` returned "No wiki index
  found", because the run lacked `DEEPWIKI_ASK_AGENTIC=1`. The engine image
  sets that, but the source default does not. The `ask` latency of that row
  is therefore the latency of a refusal.

### Retrieval, spring-petclinic

36 questions. The top 10 come from the read path `ask` uses: full-text and
dense search fused by weighted RRF.

| Engine | Vectors stored | Node recall@10 | Node MRR@10 | File recall@10 | File MRR@10 | Search p50 (excl. embedding) |
|---|---|---|---|---|---|---|
| Rust native | yes (2560-dim) | **0.92** | 0.65 | **0.94** | 0.84 | 4.5 ms |
| Python, patched | 422 | 0.03 | 0.00 | 0.17 | 0.05 | 0.8 ms |
| Python, as shipped | 0 | 0.00 | 0.00 | 0.00 | 0.00 | 0.3 ms |
| Python, patched, text embeddings (diagnostic) | 422 | 0.92 | 0.65 | 0.94 | 0.84 | 0.8 ms |

The diagnostic row explains the gap. The two engines retrieve identically
once Python's vectors are made from the text. They were not, for two
reasons, both Python defects:

- **As shipped,** the 1536-dimension table cannot hold this model's vectors.
  The index has none, and nothing reports it.
- **Patched,** Python sends the cl100k token IDS of each text to the
  embedding model, and a non-OpenAI model reads them as its own IDs. The
  vectors fit the table, but they mean nothing.

### Structure coverage, spring-petclinic

The 25 architectural symbols are the classes, interfaces, records and
functions of the code graph.

| Engine | Sections | Pages | Words | Arch. symbols named | Arch. files cited | Mermaid blocks |
|---|---|---|---|---|---|---|
| Rust native | 7 | 16 | 68,981 | 25/25 (100%) | 25/25 (100%) | 68 |
| Python, patched | 8 | 17 | 74,715 | 25/25 (100%) | 25/25 (100%) | 87 |
| Python, as shipped | 5 | 25 | 45,138 | 19/25 (76%) | 16/25 (64%) | 102 |

### The analysis phases, elitea-platform, no model

This is a large polyglot repository (Go, TypeScript, Python, Rust). Each
row is one measured run (`/usr/bin/time -l`).

| Step | Rust | Python | Rust is |
|---|---|---|---|
| Code graph build | 12.5 s, 1.63 GB max RSS | 104.7 s, 3.12 GB | 8.4× faster, 1.9× less memory |
| Graph + phase 2 (orphan resolution, 87,614 orphans) | 15.1 s, 2.36 GB | 854.4 s, 3.53 GB | 57× faster, 1.5× less memory |

Phase 3 on this corpus could not be re-measured: the Python run stalled at
100% CPU for over two hours without output and was stopped. The earlier
parity replay, same corpus, measured Phase 3 at about 1 s (Rust) against
479 s (Python).

### Image

| Image | Size | Contents |
|---|---|---|
| `elitea-deepwiki-engine-native` | 156 MB (arm64, measured before the Debian 13 base change) | one binary on distroless; no Python, no CUDA, no shell; since #1089 on `base-nossl-debian13` with no system OpenSSL; HIGH/CRITICAL scan blocking and clean |
| `elitea-deepwiki:*-engine` (Python) | about 2 GB (as recorded in ADR-0022 and the workflow comments; not re-measured) | the Python engine and its ML closure (torch, transformers, faiss-cpu, tree-sitter) |

## Defects found by the benchmark

Fixed in this programme's PRs:

| Engine | Defect | Fix |
|---|---|---|
| Rust | A model stream had a 30-minute total limit, which failed a page that a reasoning model was still writing. | `7b7757965`: 2 h, configurable |
| Rust | A forced last turn of only whitespace made `ask` succeed with a blank answer. | `7f9c02214`: a `RuntimeError` |
| Rust | Embedding windows are sized in cl100k tokens. A model whose tokenizer counts more (Qwen3, 8192 context) refused a full window, and the run failed (express, after 5.8 s). | `4e3da47d9`: a context-length refusal splits the window and retries; `ELITEA_DEEPWIKI_EMBED_CTX_TOKENS` |

Python defects, recorded and not fixed (the Python engine is being
replaced):

- the fixed 1536-dimension vector table stores no vectors for other models,
  silently;
- it sends cl100k token IDs to non-OpenAI embedding models;
- its exclude globs match absolute paths, so a clone path that contains
  `.cache` indexes nothing (the harness clones elsewhere);
- `ask` depends on `DEEPWIKI_ASK_AGENTIC=1`, which only the image sets.

## Not measured

- **express** (JavaScript).
  - Rust failed on the embedding window defect above, before its fix. It
    was not re-run.
  - The Python as-shipped run was stopped partway.
  - Note that express's default branch moved from `7ef98448` to
    `9efc29e2` after the question set was built.
- **cleanarch and leveldb:** not run; their question sets are in
  `parity/questions/`.
- **The blinded LLM judge** (`parity/bench/judge.py`) of the `ask` answers:
  not run. Answer quality is evidenced here only by retrieval, structure
  coverage and the answered/blank counts.
- **The image size of the Debian 13 native build,** and the Python image's
  size on this host.
- **Linux numbers.** Every figure is from one arm64 macOS host, where the
  worker's `RLIMIT_AS` is not enforced.
