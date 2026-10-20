# libs/rust — shared Rust crates

Crates the platform's Rust services share (ADR-0027). The first consumers are
the engine sidecars behind `elitea-subapp-host`: the DeepWiki engine and the
native Inventory engine (`services/elitea-inventory-engine`).

| Crate | What it holds |
|---|---|
| `engine-core` | The sidecar error contract, the NDJSON stream and stop flag, zeroizing secrets, Python-compatible JSON/string/value semantics |
| `engine-sidecar` | The Unix-socket NDJSON server the host's engine client speaks, over any `Engine` (tools + run); the distroless container probe; the cgroup reader; process tracing with OTLP span export under the worker's switches |
| `llm-wire` | The `/llm` caller contract without a transport: routes, header names and value rules, refusal codes and retry hints (budget scope from a 402 body, bounded or omitted upstream detail), a bounded SSE splitter, `OpenAI` chunk primitives (usage, finish reason, reasoning) and tool-call delta assembly, each with a lenient (engines) and a strict (worker) profile. serde_json with default features only, so the agent worker can depend on it without changing its canonical hashes |
| `model-client` | The OpenAI-compatible gateway client: chat (blocking, streamed, tool calls, reasoning), batched embeddings, SSE, token counting; follows the `/llm` caller contract the worker follows (`model-client/docs/llm-caller-contract.md`) |
| `adk-gateway` | An adk-rust model (`adk_core::Llm`) over `model-client`: an engine's agent runs on adk's `LlmAgent` and `Runner` while every model call follows the `/llm` caller contract; `adk-core` is pinned `~2.2.0` (adk breaks its API between minors, and the worker pins `=2.2.0`) |
| `agent-runtime` | The agent runtime the cloud worker and the desktop host share (ADR-0029), moved out of `services/elitea-worker-rust` in stages (`agent-runtime/EXTRACTION.md`): the host interface (definitions, models, tools, events, state, memory, approvals, execution guard), the pipeline graph nodes without cloud coupling, and `canonical`, order-explicit JSON so no digest depends on `preserve_order`. serde_json with default features only, like `llm-wire`; adk-core/adk-graph/adk-session pinned `~2.2.0`. Behind the `toolkit-data-analysis` feature (the cloud worker enables it, the desktop does not) it carries the `data_analysis` internal tool (ADR-0033): ten tools over CSV, TSV, JSON and xlsx files on an in-house typed table engine, bounded while a file is read, plus `run_analysis_code`, which hands a program to the platform code sandbox through the `AnalysisCodeRunner` seam |
| `local-embed` | The desktop's optional embeddings for the local index: the `Embedder` trait, the model catalogue the person chooses from (multilingual-e5-small, Qwen3-Embedding-0.6B; every file pinned by SHA-256), int8/Matryoshka vector blobs, and (feature `candle`) the on-device backend — candle with fused-attention BERT and Qwen3 forwards, Metal on macOS. `examples/embed_bench.rs` measures throughput; `tests/candle_models.rs` checks the vectors against candle-transformers' reference models when `ELITEA_EMBED_MODELS` points at downloaded checkpoints |
| `local-tools` | The desktop host's local tool family (ADR-0029 decision 4), handed to `agent-runtime` through its `ToolProvider` and `ApprovalChannel`: a workspace confined by descriptor walks (`openat` + `O_NOFOLLOW`), file/search/tree/document/shell/git tools, read-before-write, the OS sandbox for commands (Seatbelt on macOS, bubblewrap or a partial Landlock helper on Linux; `.git` read-only, credentials unreadable), layered approval rules over parsed argv (reads, in-workspace writes, commits and routine sandboxed commands run without a question; destructive commands, network and full access are asked: `local-tools/README.md`; `command_deny` is advisory, not a boundary: the sandbox and the approval confine commands), host git hardened against repository-controlled code (unsafe repositories refused, overrides, sandboxed git, timeouts), and turn checkpoints (private git refs, or copies outside git). Feature `handoff` (ADR-0034): the send snapshot in a temporary index (exclusions on tracked files too, the secret scan with rules shipped as data, the caps, orphan base and upstream, a shadow git directory for a plain folder) and take-back (a preflight with no side effects, then the fast path with a checkpoint, a worktree, or discard) over `workspace-store`. Unix only; its Seatbelt tests run on macOS in `ci-local-tools-macos.yml` |
| `code-parsers` | tree-sitter parsers for Python, Go, TypeScript, JavaScript, Java, C#, C++, Rust, Kotlin and Swift → symbols and relationships; `grammar_for` gives a language's grammar to a syntax-aware chunker |
| `graph-algos` | Seeded two-pass Leiden (RB-configuration) over the vendored `leiden-rs`, communities numbered by size |
| `repo-ingest` | Admitted shallow git clones (gix, egress allowlist, limits), artifact-folder downloads, file discovery; refusals name the consumer's settings (`names::SettingNames`) |
| `content-source` | The source-agnostic content layer (ADR-0028): `ContentSource` (list documents with a version, mime type and ACL; fetch their bytes), the document model, `Acl`/`Caller`; git (a checked-out tree) is the first connector |
| `connectors` | One HTTP client per source provider (ADR-0027 decision 8, ADR-0030 decision 3), shared by the agent-runtime toolkit families and the indexing connectors: a TLS-agnostic `Transport` (reqwest 0.12 and 0.13 adapters behind `reqwest-012` / `reqwest-013`; the binary picks the TLS stack), the strict base-URL rule, the shared fail-closed egress host allowlist (`repo-ingest`'s `EgressPolicy` wraps it), and per provider its configuration, wire layer, request building and authentication. `content-source` adds `ContentSource` connectors (list with pagination and bounded 429 backoff, fetch under a separate size cap) |
| `conversation` | An agent's conversation (`Msg`, `Call`) and its summarisation (`LangChain`'s `SummarizationMiddleware`: model-profile thresholds, a cut that keeps calls with their results, `compact`); the `DeepWiki` agents and Inventory's `investigate` |
| `doc-extract` | Document bytes to text: text decoded as UTF-8, else by the encoding `chardetng` guesses (binary refused); HTML converted to markdown (`html-to-markdown-rs`); PDF, Office, spreadsheets, e-mail and e-books through xberg (pinned) behind the `documents` feature, on its own large-stack thread with page/size/time caps |
| `doc-chunk` | Document text to index chunks with the SDK's defaults (ADR-0030 decision 4): markdown (H1-H4, merge, token split), text, JSON (`RecursiveJsonSplitter`), code (tree-sitter per method via `code-parsers`), and the extension router; the `index_data` `chunking_config` form. Model-calling steps (statistical and proposal chunkers, the image-file vision step `plan_image`) are planned here and fulfilled by the caller; `chunk` returns `RequiresModel` for them |
| `pg-migrate` | The forward-only, checksummed Postgres migration runner: each consumer passes its own ledger table and advisory-lock name, so two engines on one database never share a ledger |
| `workspace-runner` | The cloud executor's workspace runner (ADR-0034 decision 9): the binary inside the turn's sandbox container (`Containerfile` here: Debian 13, git, a build toolchain, no credential). `unpack` reads the worker's workspace archive into `/workspace` and records a baseline commit, `serve` answers tool calls as JSON lines with local-tools at the `container` enforcement level (`git_commit` refused: the worker commits), `pack` writes the tree back without `.git`. Its contract test holds `agent-runtime`'s `workspace_tools::WORKSPACE_TOOLS` equal to local-tools' `TOOLS` minus `git_commit` |
| `workspace-store` | The workspace store client of the desktop ⇄ cloud handoff (ADR-0034 decision 2), shared by the desktop host and the cloud worker: git bundle v2 `COMPACT` segments written from an object database (whole-object packs) and read back into a quarantine, verified (SHA-256 content address, pack checksum, connectivity from every ref) before any ref or file is touched; sha1 only (sha256 refused); the ref allowlist; the store transport with retries (an idempotent publish after a lost ack, a dropped upload re-sent, a stale plan fetched again), an in-process store with Main's rules and a fault-injecting transport for tests; feature `rest`: the desktop's REST routes over a host-built reqwest 0.12 client. gix crates only (`gix-odb`, `gix-pack`, `gix-object`), never the git CLI and never a repository's config; code adapted from floe is listed in its `NOTICE` (`tests/notice.rs` enforces the list) |
| `vector-index` | The caller side of `elitea-vector` (ADR-0030 decisions 2 and 5, ADR-0031): a typed `elitea.vector.v1` gRPC client (mTLS, `authorization: Bearer` from the claim token, retryable vs non-retryable status classes), the SDK's index tools on it (`search_index` with over-fetch, `extended_search`, rerank and cut-off, `full_text_search` as hybrid search, `stepback_*`, `list_indexes`, `remove_index`; the three ADR fixes), and the SDK filter translation (refuses what Qdrant cannot express). No model or HTTP stack: queries and step-back prompts go through traits the worker implements. Feature `test-server` adds an in-process fake of `elitea-vector` for tests |
| `office-core` | `elitea-office-core` (ADR-0032 decisions 2 and 3): the only crate that names a `zavora-*` type. `Deck` (pptx), `Document` (docx) and `Workbook` (xlsx) with our own types, bytes in and out. Caller bytes are validated and re-packed with `zip` 8.6 before any library sees them (entry, XML, media, depth, DTD, span and raster-pixel limits in one `Limits`); parse, save and render run on a dedicated 16 MiB thread under a process-wide permit held until the thread exits (`health()`); renders get a copy with embedded fonts emptied and SVG neutralised (and the vendored renderers use the bundled fonts only); `Deck::facts()` reads tables, charts, geometry, font sizes and colours back from a saved deck; `facts_of_docx_package` and `facts_of_xlsx_package` read paragraphs, tables, pictures, charts, named cells and per-sheet charts from saved bytes without the library (P4), `Document::replace_all` replaces in one leftmost-longest pass with line breaks as break runs, and `Workbook::{write_cells,clear_range,add_chart}` write all or nothing, counting distinct cells. The hostile corpus is built in `tests/hostile.rs`; `fuzz/` holds the cargo-fuzz targets and `tests/corpus.rs` replays a bounded corpus on every CI run. `svg_to_png` (ADR-0033) rasterises an SVG the caller built, with `resvg` and the bundled Liberation Sans only, following no image reference; `data_analysis` draws its charts with it. |

## Layout rules

- **A workspace of libraries, not of services.** Each service keeps its own
  crate, `Cargo.lock` and image, and takes these crates as path dependencies
  (`../../libs/rust/<crate>`). An image build copies `libs/rust` and its own
  tree, nothing else.
- **Caret ranges here, exact pins in services.** A shared crate states the
  lowest version it needs; each service's lock decides the exact one. The
  exceptions are the tree-sitter grammars in `code-parsers` and xberg in
  `doc-extract`: their output is a function of their revision, so they stay
  exactly pinned.
- **Lints match the services:** `unsafe_code = "forbid"`, Clippy `all` and
  `pedantic` denied.
- **No engine-specific code.** A crate here must not know which engine it
  serves. DeepWiki keeps its old module paths by re-exporting
  (`pub use elitea_engine_core::errors;`), so moving code here is a move, not
  a rewrite of every caller.

Vendored third-party crates live in `vendor/` (provenance and review in
`vendor/README.md`). They are path dependencies, never workspace members,
and `vendor/rustfmt.toml` keeps `cargo fmt` from rewriting them.

## Checks

CI runs them inside `ci-deepwiki-engine.yml` (one job, one dependency build):

```bash
cd libs/rust
cargo fmt --all -- --check
cargo clippy --locked --workspace --all-targets --all-features -- -D warnings
RUSTDOCFLAGS="-D warnings" cargo doc --locked --workspace --no-deps --all-features
cargo test --locked --workspace --all-targets --all-features
```

A change here also runs every consumer's CI: each workflow that watches
`services/elitea-deepwiki-engine/**` watches `libs/rust/**` too.
