# libs/rust — shared Rust crates

Crates the platform's Rust services share (ADR-0027). The first consumers are
the engine sidecars behind `elitea-subapp-host`: the DeepWiki engine and the
native Inventory engine (`services/elitea-inventory-engine`).

| Crate | What it holds |
|---|---|
| `engine-core` | The sidecar error contract, the NDJSON stream and stop flag, zeroizing secrets, Python-compatible JSON/string/value semantics |
| `engine-sidecar` | The Unix-socket NDJSON server the host's engine client speaks, over any `Engine` (tools + run); the distroless container probe; the cgroup reader; process tracing with OTLP span export under the worker's switches |
| `model-client` | The OpenAI-compatible gateway client: chat (blocking, streamed, tool calls, reasoning), batched embeddings, SSE, token counting; follows the `/llm` caller contract the worker follows (`model-client/docs/llm-caller-contract.md`) |
| `code-parsers` | tree-sitter parsers for Python, Go, TypeScript, JavaScript, Java, C#, C++ and Rust → symbols and relationships |
| `graph-algos` | Seeded two-pass Leiden (RB-configuration) over the vendored `leiden-rs`, communities numbered by size |
| `repo-ingest` | Admitted shallow git clones (gix, egress allowlist, limits), artifact-folder downloads, file discovery; refusals name the consumer's settings (`names::SettingNames`) |
| `pg-migrate` | The forward-only, checksummed Postgres migration runner: each consumer passes its own ledger table and advisory-lock name, so two engines on one database never share a ledger |

## Layout rules

- **A workspace of libraries, not of services.** Each service keeps its own
  crate, `Cargo.lock` and image, and takes these crates as path dependencies
  (`../../libs/rust/<crate>`). An image build copies `libs/rust` and its own
  tree, nothing else.
- **Caret ranges here, exact pins in services.** A shared crate states the
  lowest version it needs; each service's lock decides the exact one. The
  one exception is the tree-sitter grammars in `code-parsers`: parser output
  is a function of the grammar revision, so they stay exactly pinned.
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
