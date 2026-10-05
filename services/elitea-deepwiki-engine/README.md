# elitea-deepwiki-engine

The Rust-native DeepWiki engine ([ADR-0026](https://github.com/elitea-ng/elitea-docs/blob/main/docs/internal/03-architecture/adrs/adr-0026-native-deepwiki-engine.mdx)).
It replaces the Python engine sidecar (`services/elitea-deepwiki`) behind the
same Unix socket. The Go sub-application host (`services/elitea-subapp-host`)
keeps the provider SPI, admission, the parameter merge, the egress check,
composition and upload. This crate runs the tools.

**Status: ADR-0026 phase 2.** Phase 1 delivered the sidecar protocol, the
`unavailable` and `fixture` runners, and the container probe. Phase 2 adds
the front half of the engine: repository ingest, the eight language parsers,
and the code graph (Phase 1 build plus the Phase 1c passes), each proven equal
to the Python engine (see [Parity](#parity-with-the-python-engine)). Nothing
calls them from the socket yet: the `native` runner arrives with
`generate_wiki` (phase 5). Until then `ELITEA_DEEPWIKI_RUNNER=native` refuses
to start, so a deployment cannot ask for the engine and get a refusal at
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

## Repository ingest (`src/ingest/`)

ADR-0026 decision 7: gitoxide (`gix` 0.88) in process, no `git` binary
(the runtime image is distroless). Not wired to a runner yet; the native
runner calls `ingest::ingest(repo_config, settings, job_scratch, cancel)`.

1. An artifact-folder source (`provider_type: artifact`, `artifact://…`)
   is refused for now (`RuntimeError`); a later phase ports it.
2. `providers::clone_target` ports `engine/repo_providers` (GitHub incl.
   Enterprise, GitLab, Bitbucket Cloud/Server, Azure DevOps) over the
   host's `repo_config`. It builds a credential-free `https://` URL and the
   `Authorization` value git sends for the userinfo Python put in the URL
   (`Basic base64("user:password")`, an absent password empty: GitHub
   `token:`, GitLab `oauth2:token`, Bitbucket `user:password`, ADO `pat:`).
   `tests/fixtures/ingest/providers.json` is the Python factory's output
   for 44 configurations (`gen_providers.py` regenerates it).
3. `egress` re-checks the URL's own host against
   `ELITEA_DEEPWIKI_GIT_ALLOWLIST` (Python/Go rules: fail-closed, `*`,
   `*.x` = direct subdomains, ports ignored) before the credential is
   used. The engine checks the host it will CONNECT to; the Go host checks
   its prediction (`api.github.com` for the public API URL), so list
   `github.com,*.github.com`.
4. `clone` resolves the branch (`ls-remote`; a missing branch is
   `resource_not_found` before any download), clones depth 1, single
   branch, no tags into `{scratch}/{owner_repo}_{branch}_{sha8}`, and
   reports the identity `{repo}:{branch}:{sha8}` of the commit checked out.

The credential is an in-memory extra header for one connection: never in a
URL, `argv`, `.git/config` or an error message. No HTTP redirect is
followed, with or without a credential, so a clone only ever connects to the
host the allowlist admitted (a repository that moved must be given by its new
URL). The
repository is opened isolated, so no host git configuration (credential
helpers, `insteadOf`, `extraHeader`) applies. Paths are validated by
gitoxide (no `..`, no `.git`), a file is never written below a symlink,
and `verify_containment` re-checks every checked-out path from outside.
Symlinks are checked out as links and never followed later. Submodules and
Git LFS are not supported, on purpose.

| Setting | Default | |
| --- | --- | --- |
| `ELITEA_DEEPWIKI_GIT_ALLOWLIST` | unset = refuse all | hosts a clone may reach |
| `ELITEA_DEEPWIKI_MAX_CLONE_BYTES` | 2 GiB | pack bytes received (watched during the fetch) + blobs to check out |
| `ELITEA_DEEPWIKI_MAX_FILE_COUNT` | 100 000 | files (and, separately, directories) in the tree, before checkout; the tree walk stops at the first limit passed |
| `ELITEA_DEEPWIKI_MAX_FILE_BYTES` | 100 MiB | one blob, before checkout; also the largest allocation of the pack resolution (at least 16 MiB), so a decompression bomb fails while fetching |
| `ELITEA_DEEPWIKI_MAX_PARSED_BYTES` | 512 MiB | blobs discovery would parse, before checkout |
| `ELITEA_DEEPWIKI_CLONE_TIMEOUT_SECONDS` | 600 | ls-remote + fetch + checkout |
| `ELITEA_DEEPWIKI_SCRATCH_PATH` | `/tmp/deepwiki` | root of the per-job scratch directories |

A limit is a `ValueError` (`invalid_input`) naming the setting; the
timeout is `timeout_error`. The tree admission walks the fetched tree entry
by entry (a few objects can describe billions of files) and also stops at the
deadline and on cancellation.

Residual risk: the pack resolution decodes objects in memory on at most 4
threads, each holding a few buffers up to the allocation limit, so a fetch
can still peak at about `3 × 4 × MAX_FILE_BYTES` plus the pack's delta tree.
gitoxide offers a per-allocation limit and a thread count, not a total; the
deployment control is the job's memory limit (container or pod), which
should stay above that figure. `tests/ingest_clone.rs` runs every path against
`git http-backend` on loopback (git is a TEST dependency only);
`ELITEA_DEEPWIKI_LIVE_CLONE=1` adds a clone of this repository from GitHub.

## Parsers and the code graph (`src/parsers/`, `src/graph/`)

Eight parsers, one per language the Python engine parses richly: Python, Go,
TypeScript/TSX, JavaScript/JSX, Java, C#, C++ and Rust. Each is a port of the
Python visitor for that language, over `tree-sitter` 0.27. The Python parser
used the standard-library `ast` module; its port builds the same tree from
`tree-sitter-python` and ports `ast.unparse` for signatures.

The grammars are the ones the Python engine's `tree_sitter_language_pack`
1.16.1 builds. A test per language asserts the grammar's ABI, parse-state,
field and node-kind counts. The C# and C++ crates on crates.io are older than
those grammars, so both are pinned to a git revision.

`src/graph/builder.rs` turns the parse results into the code graph exactly as
`EnhancedUnifiedGraphBuilder.analyze_repository` does: file discovery,
documentation chunking, node ids and collision suffixes, relationship
resolution, contraction, the SQL subgraph and ORM linking. `src/graph/phase1c.rs`
then runs the passes `filesystem_indexer` runs before writing the index: API
surfaces and contract nodes, the cross-language linker, markdown structure,
and the test linker (off by default, `DEEPWIKI_TEST_LINKER`). The graph
iterates nodes and edges in networkx insertion order, because contraction and
every later pass depend on that order.

Output is deterministic. Files are parsed in parallel, but every cross-file
registry is filled and read in sorted path order.

### Deliberate differences from the Python engine

- **Exclude patterns match the repository-relative path.** Python matched the
  ABSOLUTE path, so a clone under a directory named `build`, `target` or
  `dist` excluded every file.
- **Symlinks are never followed** by discovery or by any pass that reads a
  file. Python followed them, so a link to a secret file was read, sent to the
  model and published.
- **JavaScript relative imports stay inside the repository.** Python's
  `_resolve_import_path` asked the file system about any `./` or `../` path
  (`is_file`, `realpath`), following symlinks and leaving the repository. The
  Rust resolver looks a path up only inside the directory of the parsed files,
  one name at a time, never through a symlink, and collapses `..` by name. A
  path outside, or through a link, is never a parsed file, so the import
  resolves through the global export index as before; on the parity corpora
  nothing changes.
- **Deep nesting fails a file later, and never the process.** Python's
  recursive visitors fail a file at its 1,000-frame recursion limit (about
  450–600 nesting levels for Java, C#, C++ and JavaScript). The exact depth
  depends on the caller's stack, so it cannot be reproduced; the Rust parsers
  run on large worker stacks and parse such a file. A file whose syntax tree is
  deeper than 4,000 levels (measured without recursion before any walk) fails
  alone with `maximum recursion depth exceeded`, the text Python gives, instead
  of overflowing the stack and aborting the engine. The Python parser port
  reproduces `ast`'s own limits and refuses the same depth. When the
  large-stack worker pool cannot start, every file fails with that error; the
  parse never falls back to a small stack.
- **A file whose output passes 256 MiB fails.** Every symbol keeps its node's
  whole text, so nested declarations repeat the same bytes once per level and
  the output can grow with the square of the file (1,100 nested functions
  around 300 KB of text give 330 MB). Python keeps it all; the Rust parsers
  count the symbol text (and JavaScript's body-reference edges) per file and
  fail the file with `output limit exceeded` at 256 MiB. No file of the parity
  corpora comes near either limit.
- **Hash-seeded iteration order is sorted.** Where Python iterates a `set`
  (some `imports` lists, some edge-insertion loops), the order changes with
  the hash seed between runs; the Rust order is sorted. Rows do not change.

Every other quirk of the Python parsers is reproduced and documented in the
module that reproduces it: text sliced by code point at byte offsets, files
dropped on one bad UTF-8 byte, doubled visits, and names that collide across
packages with the last file winning.

## Parity with the Python engine

The parity tools are in `parity/`. They run the Python engine itself (the
`engine` extra of `services/elitea-deepwiki`), made reproducible for the
reference only: inline thread pools, submission-order `as_completed`, sorted
discovery. Two Python runs on the same commit are byte-identical.

```bash
# Python reference: the graph as the index stores it (repo_nodes / repo_edges rows)
PYTHONPATH=services/elitea-deepwiki/src python services/elitea-deepwiki-engine/parity/python_reference.py <repo> <ref-dir>
# One parser's ParseResults, file by file
PYTHONPATH=services/elitea-deepwiki/src python services/elitea-deepwiki-engine/parity/python_parse_dump.py <language> <repo> <ref.jsonl>

# Rust side
cargo run --release --bin deepwiki-parity -- graph-dump <repo> <out-dir>
python3 parity/compare_parses.py files <ref.jsonl> files.txt
cargo run --release --bin deepwiki-parity -- parse-dump <language> <repo> files.txt <out.jsonl>

# The gates
python3 parity/compare_graphs.py <ref-dir> <out-dir>        # node and edge Jaccard per language
python3 parity/compare_parses.py compare <ref.jsonl> <out.jsonl>
```

The CI suite carries Python-generated golden fixtures for every parser and for
Phase 1c, so a regression fails `cargo test` without the Python closure. The
full-corpus runs below are local: their references are hundreds of MB.

Measured 2026-10-05 (Apple M4 Pro; real parsers, builder and Phase 1c against
the Python engine's own graph). Every language on every corpus has node and
edge Jaccard 1.0:

| Corpus | Files | Nodes / edges | Python | Rust |
| --- | --- | --- | --- | --- |
| elitea-platform | 11,401 | 150,942 / 373,281 | 83 s (Phase 1 only), 3.07 GB peak RSS | 7.6 s, 1.48 GB peak RSS |
| spring-petclinic (Java) | 132 | 531 / 1,010 | | 0.31 s |
| CleanArchitecture (C#) | 258 | 1,500 / 1,037 | | 0.09 s |
| leveldb (C++) | 152 | 2,582 / 14,798 | | 0.28 s |
| express (JavaScript) | 214 | 1,109 / 1,034 | | 0.08 s |

Memory: the build parses one language at a time and drops its parse results
before the next, keeps only the symbol fields later passes read, and writes
rows one at a time. The live heap peaks near 0.7 GB on elitea-platform; the
RSS figure also counts pages macOS malloc keeps after the parse results are
freed. `DEEPWIKI_PARITY_RSS=1` makes `deepwiki-parity` print RSS at each phase.

Per parser, on the parser gate (symbols and relationships, every field, in
order): all eight are exact on their corpora — elitea-platform for Go,
TypeScript, Python, Rust and JavaScript; plus gson (Java), Newtonsoft.Json and
CleanArchitecture (C#), leveldb (C++), express (JavaScript), elitea-sdk and the
legacy plugins (Python), and a tricky-construct fixture set per language. Two
known single-file differences: one Newtonsoft.Json file where tree-sitter
0.27's error recovery inside an `#if`-split `switch` differs, and eleven Go
signatures where the language pack's grammar misreads unnamed parameters.

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
