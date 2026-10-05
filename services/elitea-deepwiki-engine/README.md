# elitea-deepwiki-engine

The Rust-native DeepWiki engine ([ADR-0026](https://github.com/elitea-ng/elitea-docs/blob/main/docs/internal/03-architecture/adrs/adr-0026-native-deepwiki-engine.mdx)).
It replaces the Python engine sidecar (`services/elitea-deepwiki`) behind the
same Unix socket. The Go sub-application host (`services/elitea-subapp-host`)
keeps the provider SPI, admission, the parameter merge, the egress check,
composition and upload. This crate runs the tools.

**Status: ADR-0026 phase 1.** The sidecar protocol, the `unavailable` and
`fixture` runners, and the container probe. The analysis engine (`native`)
arrives in phases 2–6. Until then `ELITEA_DEEPWIKI_RUNNER=native` refuses to
start, so a deployment cannot ask for the engine and get a refusal at
invocation time instead.

## The socket protocol

```
POST /engine/invoke                  {invocation_id, tool, arguments}
  → application/x-ndjson: {"thinking": …} and {"token": …} interleaved,
    then {"result": {…}} | {"error": {message, error_type, error_category}}
POST /engine/invocations/{id}/stop   a cooperative stop → 202 {"stopped": bool}
GET  /engine/health                  {"status": "UP", "runner": …, "active": n}
```

Tools: `generate_wiki`, `ask`, `deep_research`, `resolve_wiki`.

The wire is the Python sidecar's wherever the host can see it:

- the routes and status codes: 400 for a missing id, an unknown tool or
  non-object arguments; 409 for an id that is already running; 422 for a
  body that is not a JSON object;
- an empty token is dropped, never sent;
- a stop, or a reader that goes away, ends the run at its next checkpoint
  with `{"error": {"message": "Invocation cancelled", "error_type":
  "RuntimeError", "error_category": "runtime_error"}}`;
- `error_type` is spelled as the Python exception class name, because the
  host's `engine.KindOf` maps those names. `error_category` is a port of
  `elitea_deepwiki.errors.classify`, with its precedence.

The socket is created world-writable. The host runs as another non-root
user and needs write permission to connect. The shared directory (a pod
`emptyDir`, a compose volume) is the only scope. A file at the socket path
that is not a socket is refused, never deleted.

`context_paths` and `extra_context` are resolved by the host, which removes
both keys. A request that still carries a non-empty one is refused with a
`ValueError`, never answered without its attachments. The native `ask`
resolves them itself in phase 6.

## Runners

| `ELITEA_DEEPWIKI_RUNNER` | What it does |
| --- | --- |
| `unavailable` (default) | Refuses every tool with `FileNotFoundError` / `resource_not_found`. |
| `fixture` | Canned results with paced progress (`ELITEA_DEEPWIKI_FIXTURE_STEP_SECONDS`, default 1). A port of the Python `fixture_runner.py`; the Go host's own fixture is a third copy. |
| `native` | Refused at start until the engine lands. |
| `legacy` | Refused: that is the Python engine image. |

The fixture's JSON artifacts are written as Python's `json.dumps(…,
indent=2)` writes them (insertion order, `ensure_ascii`), because the real
engine's artifacts are bytes a reader downloads.

## Running

Build with the pinned toolchain (`rust-toolchain.toml`, 1.97.1):

```bash
cd services/elitea-deepwiki-engine
cargo fmt --all -- --check
cargo clippy --locked --all-targets --all-features -- -D warnings
cargo test --locked --all-targets
```

The tests run the sidecar on a real Unix socket. Two of them read the
shared fixtures in `conformance/provider/fixtures/deepwiki/`.

The Go host's own client, composition and upload against this binary:

```bash
cargo build --locked --release
cd ../elitea-subapp-host
ELITEA_REQUIRE_NATIVE_ENGINE=1 \
ELITEA_DEEPWIKI_NATIVE_ENGINE_BIN="$PWD/../elitea-deepwiki-engine/target/release/elitea-deepwiki-engine" \
  go test -race -run Native ./internal/apps/deepwiki/run
```

`.github/workflows/ci-deepwiki-engine.yml` runs all of the above.

The image (50 MB, distroless):

```bash
podman build -f services/elitea-deepwiki-engine/Containerfile -t elitea-deepwiki-engine-native .
```

On the standalone stack, in place of the Python sidecar:

```bash
STANDALONE_OVERLAY=deploy/docker-compose.deepwiki-native.yml deploy/scripts/standalone-stack.sh up
```

The Go host then runs `ELITEA_DEEPWIKI_RUNNER=native` and `GET /health`
reports `runner: native`.
