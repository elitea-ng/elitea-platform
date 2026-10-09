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
| `agent-runtime` | The agent runtime the cloud worker and the desktop host share (ADR-0029), moved out of `services/elitea-worker-rust` in stages (`agent-runtime/EXTRACTION.md`): the host interface (definitions, models, tools, events, state, memory, approvals, execution guard), the pipeline graph nodes without cloud coupling, and `canonical`, order-explicit JSON so no digest depends on `preserve_order`. serde_json with default features only, like `llm-wire`; adk-core/adk-graph/adk-session pinned `~2.2.0` |
| `local-tools` | The desktop host's local tool family (ADR-0029 decision 4), handed to `agent-runtime` through its `ToolProvider` and `ApprovalChannel`: a workspace confined by descriptor walks (`openat` + `O_NOFOLLOW`), file/search/tree/document/shell/git tools, read-before-write, the OS sandbox for commands (Seatbelt on macOS, Landlock helper on Linux), layered approval rules over parsed argv, and turn checkpoints (private git refs, or copies outside git). Unix only; its Seatbelt tests run on macOS in `ci-local-tools-macos.yml` |
| `code-parsers` | tree-sitter parsers for Python, Go, TypeScript, JavaScript, Java, C#, C++, Rust, Kotlin and Swift → symbols and relationships; `grammar_for` gives a language's grammar to a syntax-aware chunker |
| `graph-algos` | Seeded two-pass Leiden (RB-configuration) over the vendored `leiden-rs`, communities numbered by size |
| `repo-ingest` | Admitted shallow git clones (gix, egress allowlist, limits), artifact-folder downloads, file discovery; refusals name the consumer's settings (`names::SettingNames`) |
| `content-source` | The source-agnostic content layer (ADR-0028): `ContentSource` (list documents with a version, mime type and ACL; fetch their bytes), the document model, `Acl`/`Caller`; git (a checked-out tree) is the first connector |
| `conversation` | An agent's conversation (`Msg`, `Call`) and its summarisation (`LangChain`'s `SummarizationMiddleware`: model-profile thresholds, a cut that keeps calls with their results, `compact`); the `DeepWiki` agents and Inventory's `investigate` |
| `doc-extract` | Document bytes to text: text decoded strictly; PDF, Office, spreadsheets, e-mail and HTML through xberg (pinned) behind the `documents` feature, on its own large-stack thread with page/size/time caps |
| `pg-migrate` | The forward-only, checksummed Postgres migration runner: each consumer passes its own ledger table and advisory-lock name, so two engines on one database never share a ledger |

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
