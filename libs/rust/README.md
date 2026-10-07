# libs/rust — shared Rust crates

Crates the platform's Rust services share (ADR-0027). The first consumers are
the engine sidecars behind `elitea-subapp-host`: the DeepWiki engine today, the
native Inventory engine next.

| Crate | What it holds |
|---|---|
| `engine-core` | The sidecar error contract, the NDJSON stream and stop flag, zeroizing secrets, Python-compatible JSON/string/value semantics |
| `model-client` | The OpenAI-compatible gateway client: chat (blocking, streamed, tool calls), batched embeddings, SSE, token counting |
| `code-parsers` | tree-sitter parsers for Python, Go, TypeScript, JavaScript, Java, C#, C++ and Rust → symbols and relationships |

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
