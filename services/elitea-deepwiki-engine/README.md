# elitea-deepwiki-engine

The Rust-native DeepWiki engine ([ADR-0026](https://github.com/elitea-ng/elitea-docs/blob/main/docs/internal/03-architecture/adrs/adr-0026-native-deepwiki-engine.mdx)).
It replaces the Python engine sidecar (`services/elitea-deepwiki`) behind the
same Unix socket. The Go sub-application host (`services/elitea-subapp-host`)
keeps the provider SPI, admission, the parameter merge, the egress check,
composition and upload. This crate runs the tools.

**Status: ADR-0026 phase 3.** Phase 1 delivered the sidecar protocol, the
`unavailable` and `fixture` runners, and the container probe. Phase 2 adds
the front half of the engine: repository ingest, the eight language parsers,
and the code graph (Phase 1 build plus the Phase 1c passes), each proven equal
to the Python engine (see [Parity](#parity-with-the-python-engine)). Phase 3
adds the [index storage](#index-storage-srcstorage): PostgreSQL only, with a
build space, a transactional publish and the read path. Phase 5 adds the
structure planner, the pages and the export, and wires `generate_wiki` end to
end as the [`native` runner](#the-native-runner-generate_wiki): a worker
child process per generation. `ask`, `deep_research` and `resolve_wiki` are
still refused by it (phase 6).

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
| `native` | The engine: `generate_wiki` in a worker child process (see [below](#the-native-runner-generate_wiki)); the other three tools are refused (`RuntimeError`, phase 6). Needs `ELITEA_DEEPWIKI_DATABASE_URL`, or the start is refused. |
| `legacy` | Refused: that is the Python engine image. |

The fixture's JSON artifacts are written as Python's `json.dumps(…,
indent=2)` writes them (insertion order, `ensure_ascii`), because the real
engine's artifacts are bytes a reader downloads.

## The native runner (`generate_wiki`)

`ELITEA_DEEPWIKI_RUNNER=native` (`src/runner/native.rs`, `src/worker.rs`,
`src/generate/`). The start is refused without
`ELITEA_DEEPWIKI_DATABASE_URL`: the engine has no index storage but
PostgreSQL. Model settings are per invocation (`llm_settings`,
`embedding_model`); a request without them is refused with the Python
messages (`llm_settings.api_base is required`). `GET /engine/health` reports
`runner: native`.

### The pipeline

One generation, in the worker child, in order (Python's
`tool_operations.generate_wiki` → `wiki_subprocess_worker` →
`HybridWikiToolkitWrapper` → `_write_unified_db` → the agent graph → the
composition → `publish_generation`):

1. the arguments (`generate::arguments`) and the environment flags
   (`DEEPWIKI_TEST_LINKER`, `DEEPWIKI_WEIGHT_CALIBRATION_PROFILE`,
   `DEEPWIKI_EXCLUDE_TESTS`, `DEEPWIKI_STRUCTURE_PLANNER`), refused up front
   when invalid;
2. ingest: the clone target from `repo_config`, the git allowlist again, the
   shallow clone into the job's scratch directory;
3. Phase 1 + 1c on a blocking thread;
4. a build: the graph staged (`COPY`), every node whose `source_text` is not
   blank (SQLite's `trim`) embedded through the gateway in rounds of
   `WIKI_EMBED_BATCH_SIZE × ELITEA_DEEPWIKI_EMBED_CONCURRENCY` and its
   vector staged;
5. Phase 2 against the staged rows (`storage::topology::PgTopologyStore`),
   the model as the orphan fallback embedder; the staged edges replaced;
6. Phase 3 with the first 20 hubs in id order (what Python passed), the
   cluster columns written to the staged nodes;
7. repository analysis and the structure planner over the rows as the index
   stores them (types before Phase 2's re-typing, the cluster columns);
8. pages, export and the worker's composition;
9. the publish, one transaction, before the result line. A failure is
   reported in band in `errors` (as `publishing.py` did) and the build is
   abandoned. The publish is last on purpose: a run that fails at the model
   never replaces a wiki's live index.

Progress is `thinking` lines worded like the Python worker's log lines
(`[worker] Clone config built: …`, `Phase 2 complete: …`, `Phase 3
complete: …`, `Publishing the index for query replicas`, `Published N nodes
and M vectors`). Errors keep the frozen categories: ingest refusals and
argument errors are `ValueError`, index failures `RuntimeError`
("Repository indexing failed: …"), model failures the client's own
(`timeout_error`, `service_busy`, …). Deliberate difference: a failed
embedding request fails the run (Python skipped the batch and published a
wiki with part of its vectors). That includes an orphan's fallback
embedding in Phase 2: a gateway refusal or timeout there fails the run with
the gateway's own error type and category ("Embedding an orphan node with
<model> failed: …"), not as a generic index failure (Python went on without
the vector).

**What the engine trusts.** The wiki is named by the clone, never by the
caller: `wiki_id` is `normalize_wiki_id(repo:branch:sha8)` of the repository
`repo_config` names and the branch and commit actually checked out; a
`wiki_id` or `path_prefix` argument is ignored. Phase 2's path prefixes are
directories of the clone, bound as parameters. `llm_settings.api_base` is
taken as given: elitea-main's facade replaces the whole block
(`material.CallbackSettings`, the platform's `/llm/v1` and a short-lived
bearer) and lifts only `max_tokens` / `temperature` from a client's block, so
a caller cannot redirect the model calls or the bearer.

### Phase 2 on PostgreSQL

`PgTopologyStore` answers every `TopologyStore` call from the build's staged
rows, with the semantics `parity/python_reference.py --search-dsn` measured:
the folded `plainto_tsquery` over the staged `fts` column, optionally under
`<prefix>/` (a C-collated range), ranked by BM25 (k1 1.2, b 0.75) over the
build's own statistics (document length = the sum of position counts, the
document frequency of each query lexeme counted once per build and cached),
negated to FTS5's sign, ties by node id; `phraseto_tsquery` for the phrase
counts; exact pgvector L2 for dense search, ranked over ALL vectors and
filtered by the prefix AFTER the limit (sqlite-vec's order, the owner's
decision), so fewer than `k` hits can come back. The hub flags and the meta
entries have no column in the ADR-0022 schema; the store returns them to the
caller. Each call checks the stop first.

### The worker child

`elitea-deepwiki-engine worker` (ADR-0026 decision 10). The parent:

- makes `{ELITEA_DEEPWIKI_SCRATCH_PATH}/jobs/job-…` (mode 0700) and removes
  it after the child ends, also when the request's reader went away and
  the supervising task was dropped (a drop guard removes the directory and
  schedules the build's delete). At `serve` startup it removes every
  `jobs/*` entry an earlier process left: on Linux their workers died with
  it (`PR_SET_PDEATHSIG`); on macOS, which has no such signal, an orphaned
  worker only stops at its next checkpoint (its stdin closed), so the
  startup clean-up can remove a directory under a worker that is still
  ending;
- starts the child with `RAYON_NUM_THREADS` and `MALLOC_ARENA_MAX=2` and
  sends the request on its STDIN (the arguments carry credentials, so never
  `argv` or the environment); stdin stays open and its end stops the child;
- relays the child's NDJSON `thinking` / `token` lines, keeps its
  `{"build": id}` line, takes the last line as the result or the error, and
  copies its stderr (the logs) to its own. A line is at most 64 MiB, the Go
  host's own line limit (`MAX_RESULT_LINE`, tested against `engine.go`);
  the child refuses a larger result BEFORE it publishes (`RuntimeError`
  "The wiki result is too large: …; nothing was published"), so a wiki the
  host could never receive does not replace the live index;
- on a stop, or a reader that went away, sends SIGTERM, then SIGKILL after
  3 s (the child stops at its next checkpoint, abandons its build and writes
  the stop line);
- treats the publish as a critical section: the child writes a
  `{"publishing": true}` control line (kept, never relayed) before it
  publishes and `{"publishing": false}` after. A stop meanwhile is deferred:
  the child does not interrupt the publish, and the parent moves its SIGKILL
  out to the publish `statement_timeout` plus 30 s. A publish that
  committed reports its result even though a stop came (never "cancelled"
  for a wiki that is live); one that did not commit reports the stop. A
  child killed inside the publish after the commit is reported as a
  `RuntimeError` saying the index is live but the result was lost. The
  child's connections set `client_connection_check_interval` (5 s), so the
  backend of a killed child aborts its statement and releases its locks;
- after the child ended, deletes its build if it reported one, so a killed
  child leaves no staging rows (a no-op after a publish or an abandon). The
  delete has a 15 s `lock_timeout` and a 2 min `statement_timeout`; on a
  timeout the sweep removes the build.

The child limits itself before it reads the request (`rustix`'s safe
`setrlimit`, so the crate stays `unsafe_code = "forbid"`), never above the
hard limits it inherited, and on Linux asks for SIGKILL when its parent dies.
A parser pool that cannot start its threads fails the run
("Repository indexing failed: Parse error: the … parser could not start its
worker threads"); it does not index the repository with every file of that
language marked as failed. A child that dies without a last line is
reported by its cause: an
allocation failure (`MemoryError`, `out_of_memory`), SIGXCPU
(`timeout_error`), a SIGKILL the parent did not send (`MemoryError`: on
Linux that is the kernel's OOM killer; the message says so for certain when
the cgroup v2 `memory.events` `oom_kill` count rose during the run), anything
else `RuntimeError`. A last line cut off by the child's death does not hide
the exit status: it is reported ("output is unreadable") only when the exit
explains nothing.

| Setting | Default | |
| --- | --- | --- |
| `ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES` | 85 % of the container's cgroup v2 `memory.max` when it is set, else 16 GiB (at least 1 GiB) | `RLIMIT_AS`. It counts reserved address space (each parser thread reserves its stack, up to 256 MiB), not resident memory; it stops a runaway, the pod limit sizes the job. macOS does not enforce it (a warning is logged). |
| `ELITEA_DEEPWIKI_WORKER_CPU_SECONDS` | 14400 (at least 60) | `RLIMIT_CPU`, hard limit 10 s above |
| `ELITEA_DEEPWIKI_WORKER_THREADS` | available parallelism, at most 8 (at most 256), lowered until it fits the memory cap | the child's runtime and parser threads. With the native runner, `threads × 256 MiB + 1 GiB` (each parser thread's stack reservation plus headroom) must fit `WORKER_MEMORY_BYTES`, or the start is refused. |
| `ELITEA_DEEPWIKI_SCRATCH_PATH` | `/tmp/deepwiki` | root of the job directories |

All are strict-parsed. Still refused or not ported: `ask`, `deep_research`,
`resolve_wiki` (phase 6), the deepagents planner (5d), artifact-folder
sources.

`tests/native_generate.rs` runs the whole runner: the sidecar on a Unix
socket, the real worker child, a repository served by `git http-backend`,
a mock gateway (the e2e stub's answers, stand-in embeddings) and
PostgreSQL. It checks the result against the frozen generation contract, the
published rows and the artifacts, and that a stop during the embedding
phase kills the child and leaves no staging rows. It needs
`DEEPWIKI_TEST_DSN` and the test-only `loopback-git-http` feature
(`--all-features`; a release build ignores it), because the child derives an
`https://` clone URL from `repo_config` otherwise. `tests/storage_topology.rs`
holds the store's contract. The Go host's `native_engine_test.go` runs the
native runner through its own client up to the engine's allowlist re-check;
a whole native run is not part of the Go job (it needs a gateway, a git host
and PostgreSQL), the crate test covers it.

## Repository ingest (`src/ingest/`)

ADR-0026 decision 7: gitoxide (`gix` 0.88) in process, no `git` binary
(the runtime image is distroless). The native runner calls `ingest::ingest(repo_config, settings, job_scratch, cancel)`.

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

## Phase 2: graph topology (`src/graph/topology/`)

A port of `graph_topology.run_phase2` and what it reaches
(`graph_orphan_cascade_v2`, `graph_orphan_hybrid`, `graph_lexical_v2`).
The native runner calls
`graph::topology::run_phase2(graph, store, embedder, config)` after Phase 1c.

1. Orphan resolution (`cascade.rs`), Python's Mode A: explicit references
   (markdown links, backtick names, imports), hybrid lexical + vector RRF
   (k 60, threshold 0.02, top 20 — only a node BOTH searches find can
   pass), tiered lexical T1–T4 behind the IDF gate, directory proximity.
2. Doc edges (`docs.rs`): hyperlinks, and directory proximity with the
   md5-picked anchors for repository-root docs.
3. Component bridging (`bridge.rs`).
4. Weights (`weights.rs`): `1 / ln(structural_in_degree + 2)`; synthetic
   edges floored per class (`DEEPWIKI_WEIGHT_CALIBRATION_PROFILE`,
   `calibrated` by default, or `legacy`).
5. Hubs: in-degree z-score above 3.0 (numpy's mean and std, bit for bit).
6. The index's edges are replaced by the graph's.

Every index access is a method of the `TopologyStore` trait (`store.rs`):
`get_nodes`, `node_count`, `count_phrase_matches`, `search_lexical`,
`get_embeddings`, `search_dense`, `set_hubs`, `replace_edges`,
`set_meta`. The PostgreSQL build space implements it; the parity gate uses
`ReplayStore`, which answers from a recording of the Python run. The model
fallback for an orphan without a stored vector is a `TextEmbedder`.

Phase 3 consumes the graph (weights, edge classes, the synthetic edges, and
the `rest_endpoint` re-typing the lexical pass makes in the GRAPH only) and
the hub list. Python's indexer passes Phase 3 only the first 20 hubs in id
order (`stats["hubs"]["node_ids"]`); `Phase2Outcome::hubs_for_phase3` is
that list, `Phase2Outcome::hubs` all of them.

Not ported, on purpose: Modes B and C of `resolve_orphans` (Python
hard-codes `orphan_cascade_v2` on; they are its kill switch), and with them
`DEEPWIKI_VEC_PREFIX_DEPTH` and `DEEPWIKI_VEC_CONCURRENCY`, which only they
read. Deliberate differences: a storage or embedding failure fails the phase
(Python logged it at debug level and published a poorer graph); a
component's representative among equal degrees is the first in id order
(Python's `max()` over a `set` followed the hash seed); an unknown
calibration profile is an error (Python fell back to `calibrated`).

## Model client (`src/llm/`)

ADR-0026 decision 8: one small OpenAI-compatible client on `reqwest` 0.13
(the copy `gix` pulls) over rustls. Indexing and generation (the native runner) and `ask` / deep research (phase 6) build on it.

- `ModelSettings::from_llm_settings` reads the block the facade writes:
  `api_base` | `openai_api_base`, `api_key` | `openai_api_key`,
  `organization` (sent as `OpenAI-Organization`), `model_name`, and
  `max_tokens` (default 64000), `max_retries` (2), `streaming` (true),
  `provider` (`openai` | `anthropic`; both go through the gateway's
  OpenAI-compatible surface). `temperature` is ignored, as in Python.
  Missing transport fails with the Python messages
  (`llm_settings.api_base is required`). Unlike Python, a missing
  `model_name` or `embedding_model` is refused at once instead of
  defaulting to `gpt-4o-mini` / `text-embedding-3-large`.
- `EmbeddingClient`: batches of `WIKI_EMBED_BATCH_SIZE` (64), at most
  300 000 tokens per request, `ELITEA_DEEPWIKI_EMBED_CONCURRENCY` (4) in
  flight. Texts above 8191 `cl100k_base` tokens are embedded in windows
  and averaged, as LangChain did. The dimension comes from the first
  response and is enforced for the rest of the run.
- `ChatClient`: blocking and SSE-streamed completions with tool calls
  (streamed deltas assembled by `index`; a new id, or a new name once a
  call's arguments began, on a used index starts another call; a skipped
  index leaves no slot; a slot with arguments and no name is refused),
  usage, `max_completion_tokens`,
  temperature 0.1 / 0.0 (`Sampling::Deterministic`) / 1.0 for `o*`
  models. System messages take only `'static` prompts; repository text
  goes in user messages.
- Retries: the `openai` SDK's policy (408/409/429/5xx and connection
  failures, `retry-after`, 0.5 s doubling to 8 s with jitter). A stop
  aborts a request or a wait at once.
- Timeouts: connect 10 s, blocking call 600 s, stream silence 300 s,
  stream total 30 min. SSE caps: 1 MiB per line and per event, 64 MiB
  per stream. Lines end in `\n`, `\r\n` or a lone `\r`; a leading UTF-8
  BOM is skipped.
- `ELITEA_DEEPWIKI_TLS_CA_FILE` is trusted in addition to the platform
  roots. Redirects are refused (the bearer key must not follow one).
- Errors: timeouts → `timeout_error`; 429/503 after the retries →
  `service_busy`; 402 → `invalid_input`; 404 → `resource_not_found`;
  other refusals and malformed replies → `inference_failed`. The key is
  never in an error or a log line; upstream text is redacted and cut.
- `count_tokens` is `token_counter.py` over the embedded `o200k_base`
  BPE, with no `chars/4` fallback.

`tests/llm_client.rs` runs the client against a mock gateway on loopback
(and over TLS with a throwaway CA). `ELITEA_DEEPWIKI_LIVE_LLM=1` adds a
chat, stream and tool-call round trip against the LAN vLLM
(`ELITEA_DEEPWIKI_LIVE_LLM_BASE`, `ELITEA_DEEPWIKI_LIVE_LLM_MODEL`).

## Index storage (`src/storage/`)

ADR-0026 decision 5, with the owner's rule: **no SQLite anywhere**. There is
no `.wiki.db`, no sqlite-vec, no FAISS and no docstore. The index is the
ADR-0022 schema in the `deepwiki` database, unchanged.

**Migrations.** The SQL files stay in
`services/elitea-deepwiki/src/elitea_deepwiki/migrations/`; the binary
embeds them (`include_str!`). `elitea-deepwiki-engine migrate` reads
`ELITEA_DEEPWIKI_DATABASE_URL` and writes the `schema_migrations` ledger as
`python -m elitea_deepwiki.storage` does: the same versions, names and
SHA-256 of the text Python reads (`\r\n` translated), the same refusal of
an applied migration whose file changed. Either runner continues where the
other stopped. `tests/storage_migrate.rs` runs `migrate.discover()` with
python3 and compares every checksum; a file in the directory that is not
embedded fails it. The URL form only (`postgresql://…`): sqlx does not read
psycopg's `key=value` form.

Migration 0003 (additive, applied by both runners) adds schema
`deepwiki_build`: `builds (build_id, wiki_id, owner, started_at,
heartbeat_at)` and UNLOGGED copies of `wiki_nodes` (with the folded
`deepwiki_porter` tsvector and its GIN index), `wiki_edges` and
`wiki_node_embeddings` keyed by `build_id`, plus `bm25_docs` /
`bm25_postings` (see below). Every staging row cascades from its `builds`
row. Migration 0004 (additive) adds the nullable `builds.boot_id`; 0003 is
never edited, because both runners checksum it.

**Database privileges.** The role in `ELITEA_DEEPWIKI_DATABASE_URL` that
runs the migrations needs `CREATE` on the database itself, not only on
schema `public`: 0003 runs `CREATE SCHEMA deepwiki_build` (ADR-0026 keeps
the build space in its own schema). A role that owns the database has it; a
role that was only granted rights on `public` fails 0003 with `permission
denied for database`. Grant it once (`GRANT CREATE ON DATABASE deepwiki TO
<role>`), or pre-create the schema as an administrator
(`CREATE SCHEMA deepwiki_build AUTHORIZATION <role>`; the migration's
`IF NOT EXISTS` then needs no database privilege). The serving role needs
`USAGE` on the schema and read/write on its tables, and must own the live
and staging tables to `ANALYZE` them (a skipped `ANALYZE` is logged, not
an error).

**Build and publish** (`storage::build`). A build stages the code graph with
`COPY` in the text format, in rounds of 2,000 nodes. Rows map as
`storage/publish.py` maps the `.wiki.db` rows: NULL text becomes `""`,
flags become booleans, parallel edges collapse onto `(source, target,
rel_type)` with the first position and the last `edge_class` / `weight`, a
weight of 0 becomes 1.0, `metadata` stays `{}`. Vectors are written as the
shortest decimal text of each `f64`, the text Python's `repr` gave, so
pgvector rounds them to the same `float4`.

`Build::publish` is one transaction: lock the build row; queue without a
timeout behind a publish of the same wiki, then for one of
`ELITEA_DEEPWIKI_PUBLISH_SLOTS` (default 2) publish slots per database
(transaction-scoped advisory locks: a publish waits, it does not fail; every
replica must use the same number); set `statement_timeout`
(`ELITEA_DEEPWIKI_PUBLISH_STATEMENT_TIMEOUT_SECONDS`, default 1800),
`lock_timeout` (`…_PUBLISH_LOCK_TIMEOUT_SECONDS`, default 30) and `work_mem`
(`…_PUBLISH_WORK_MEM_MB`, default 64, so the slots bound the memory the
publishes take together); upsert the `wikis` row (`registry_from_result`'s
fields; an absent field keeps the stored value), refuse an empty build,
delete the wiki's live rows, `INSERT … SELECT` nodes, edges and vectors,
write both `wiki_bm25_*` branches, delete the build, commit. A reader sees
the old index or the new one. After the commit the live tables are
`ANALYZE`d one by one, best effort, each with a short `lock_timeout`
(`…_PUBLISH_ANALYZE_LOCK_TIMEOUT_SECONDS`, default 5): a table that another
`ANALYZE` or a vacuum holds is skipped and logged
(`PublishCounts::statistics_refreshed` is then false). All five settings are
strict-parsed.

`publish` takes `&mut self`: on an error the transaction rolled back, the
build keeps its rows and its heartbeat, and the caller retries the publish
(a timeout, a lost connection), stages more and retries, or calls
`abandon`. After a success the build is gone. The BM25 statistics are `publish.py`'s: the `'bm25'` branch from
Python `str.split()` tokens of the document text (tokenised in Rust while
staging, because a PostgreSQL regular expression is not Python's
whitespace), k1 1.5, b 0.75; the `'fts'` branch from the lexemes and
position counts of the published tsvectors, k1 1.2, b 0.75; a document
without tokens takes no `doc_idx`.

**Reconciliation.** `ELITEA_DEEPWIKI_BUILD_OWNER` (default `HOSTNAME`, the
pod name) is the owner a build is recorded under, together with the boot id
of the process run (random, drawn at start). With
`ELITEA_DEEPWIKI_DATABASE_URL` set, one of the two must be set: the
fallback owner `elitea-deepwiki-engine` would be shared by every engine
without them, and the start is refused with an error that says so. `serve`
deletes this owner's builds of EARLIER runs (`owner = $1 AND boot_id IS
DISTINCT FROM $run`; a build from before 0004 has no boot id and goes too),
never this run's and never another replica's, so a reconciliation that
succeeds late (the database was down at start) cannot remove builds this run
already opened. It sweeps builds whose heartbeat is older than
`ELITEA_DEEPWIKI_BUILD_STALE_SECONDS` (default 7200, at least 300) every
quarter of that (10 s to 10 min), skipping a build a publish holds locked.
An open `Build` beats its heartbeat from a background task every tenth of
the limit (100 ms to 1 min), so a long model call after staging does not get
it swept; the task stops when the build is dropped, published or abandoned.
A database that is not up yet is retried; it never stops the sidecar. A
swept build cannot heartbeat, stage or publish.

**Read path** (`storage::search`, `storage::adapter`). Ports of
`PostgresBackend`'s searches with the same SQL — dense exact `<->` (no HNSW
index), the folded `plainto_tsquery` FTS ranked by the `'fts'` statistics
and negated, BM25 from the `'bm25'` statistics — and of `base.rrf_fuse`
(weights 0.4 / 0.6, k 60, pools of 30, stable on ties). `UnifiedDb` is
`PostgresUnifiedDB`: `search_hybrid`, `get_node`, `get_nodes_by_ids`,
`get_edges_from`, `get_edges_to`, `vec_available`, `get_meta`, with the
legacy row shapes. Every multi-statement read runs in one `REPEATABLE READ
READ ONLY` transaction.

### Deliberate differences from the Python storage

- **The publish is atomic.** `publish.py` committed every batch of 500 nodes
  and upserted over the old rows, so a reader could see a mix and a node the
  new graph dropped stayed. The publish now replaces the wiki's rows in one
  transaction.
- **A multi-statement search reads one snapshot** (above). Python ran each
  statement in its own transaction.
- **Planner statistics.** The publish `ANALYZE`s the staged tables before
  it and the live tables right after its commit (best effort, see above),
  and turns nested loops off for its bulk statements. Without that, freshly
  replaced rows were planned with the old row counts: one `INSERT … SELECT`
  took 23 s and a BM25 search did not finish in 10 minutes on a 5,000-node
  wiki. The live `ANALYZE` is not inside the transaction: there it held its
  lock on the shared tables until the commit and cancelled autovacuum.
- **The `path_prefix` filter escapes `_` and `\`** as well as `%`.
- **A NUL in text** is stored as U+FFFD. Python's publish failed on it in
  psycopg; the elitea-platform corpus has such a node (a PDF fixture).
- **A `'bm25'` term over 1 kB gets no posting** (it still counts in its
  document's length, so lengths and `avgdl` stay exact). The term is in
  0001's B-tree keys, which cannot hold it; the corpus has a 108 kB token.
- **The migrator holds an advisory lock** while it runs, so two replicas
  cannot apply one file twice. The ledger is unchanged.

### Retrieval parity

`tests/storage_parity.rs` ports `tests/storage/test_retrieval_parity.py`
over `conformance/provider/fixtures/deepwiki/retrieval/sample-repo` (20
nodes, 11 recorded queries), through the build space and the publish rather
than direct inserts:

| Branch | Result |
| --- | --- |
| dense | exact: order (up to recorded ties) and L2 distances within 1e-6, 11/11 queries |
| bm25 | exact: order and scores within 1e-6, 11/11; `doc_count` 20, `avgdl` 36.25, 279 terms, k1 1.5, b 0.75 |
| fts | match set 11/11, no recorded ordering crossed, 0 inversions over the 4 discriminating queries |
| fused | equals the frozen RRF over the components for 11/11; equals the recording for the 8 queries without a dense tie in the top 10 |

`tests/storage_reconcile.rs` covers the owner and boot-id reconciliation,
the sweep and the background heartbeat; `tests/storage_publish_control.rs`
the retry and abandon after a failed publish, the statement and lock
timeouts, the slot queue, the `ANALYZE` after the commit and two concurrent
publishes (of one wiki, of two wikis).

The tests need PostgreSQL with pgvector: they skip, with a message, when
`DEEPWIKI_TEST_DSN` is unset, and fail when `DEEPWIKI_REQUIRE_POSTGRES=1`
is set as well (CI sets both). Each test creates its own database.

### End to end on elitea-platform

`deepwiki-parity index-dump <repo> <wiki-id> [--embeddings <dim>]` builds
the graph, stages it, publishes it into `ELITEA_DEEPWIKI_DATABASE_URL` and
times a few searches. Measured 2026-10-05 (Apple M4 Pro under load, podman
VM, pgvector 0.8.5 / PostgreSQL 16, 384-dimension pseudo-vectors, before
the postings were inserted in key order):

| Step | Rows | Time |
| --- | --- | --- |
| graph build | 150,942 nodes, 373,281 edges | 5.8 s |
| stage (`COPY`) | 150,942 nodes, 282,861 edges after collapse, 150,924 BM25 documents, 5,071,704 postings | 81 s |
| stage vectors | 150,942 × 384 | 11 s |
| publish (one transaction) | the above, plus 150,852 FTS documents | 288 s |

Peak RSS 1.30 GB (the graph; staging holds one round of 2,000 nodes).
Searches on the published index: FTS 64–488 ms, BM25 21–419 ms, exact
dense scan 880 ms. Most of the publish was the two postings inserts (137 s
and 58 s, random B-tree inserts); they now insert in key order with
`work_mem` (64 MB by default, `ELITEA_DEEPWIKI_PUBLISH_WORK_MEM_MB`), not
yet re-measured. The run needs about 10 GB of database disk
(WAL included); a full host disk stopped the second run. Run `index-dump`
on a small corpus (spring-petclinic, CleanArchitecture, leveldb) on a
shared machine.

## Wiki pages and export (`src/wiki/`)

Phase 5b of `generate_wiki`: the planner's structure in, the engine result
out. The native runner calls
`wiki::run::generate_wiki_pages(generator, structure, repo_context,
identity, clock, started)` after the structure planner, with a
`wiki::index::PageIndex` built from the Phase 3 graph
(`PageIndex::from_graph` + `GraphFacts::from_graph`).

The live Python path (`agents/wiki_graph_optimized.py`, cluster planner):
`dispatch_page_generation` (with `_split_overloaded_pages`) →
`generate_page_content` per page (LangGraph `Send`, at most 4 in flight)
→ `_try_cluster_expansion` (`cluster_expansion.expand_for_page` with
`shared_expansion.expand_symbol_smart` and the language hints — both
hard-coded on) or, for a page without symbols, the documentation path
(`_fetch_explicit_target_docs` strategies 2–3,
`_get_doc_nodes_from_graph`'s scan) → the 80,000-token budget and ranked
truncation (`document_ranker`, graph-based, no embeddings) →
`_format_simple_context` → `_generate_simple`
(`ENHANCED_CONTENT_GENERATION_PROMPT_V3_TONE_ADJUSTED`, streamed,
temperature 0.1 / 1.0 for `o*`) → `diagram_sanitizer.sanitize_content`
→ `finalize_wiki` → `ArtifactExporter` → the hybrid wrapper's result →
`wiki_subprocess_worker`'s composition (manifest, `wiki_id`,
`analysis_key`, the analysis records).

Dead in that path and not ported: the hierarchical and agentic modes
(`AGENTIC_MODE_THRESHOLD` is 999,999), quality assessment and enhancement
(their graph nodes are commented out), `_collect_expansion_neighbors`
(smart expansion is on), the FTS5 graph text index (never built since the
`.code_graph.gz` writes were removed), semantic doc retrieval (flags off).

| Module | Ports |
| --- | --- |
| `spec` | `PageSpec`, `SectionSpec`, `WikiStructureSpec`, `WikiPage` (MERGE NOTE: the planner port has its own copy; one survives the merge) |
| `index`, `search` | the `.wiki.db` queries, answered in SQLite's row order from memory; the two FTS5 lookups behind `PageSearch` |
| `expansion`, `retrieve`, `ranker`, `context`, `repo_files` | the page context |
| `prompts` | the prompt files and `PROMPTS_MANIFEST.json` (MERGE NOTE: the planner keeps its own manifest; they merge into one list) |
| `pages` | the split, the fan-out, `_generate_simple`, the sanitizer call |
| `pyregex`, `sanitizer` | Python `re` on `fancy-regex`; `diagram_sanitizer.py` line for line |
| `export`, `compose`, `run` | the artifacts, the worker's composition, end to end |

### Deliberate differences

- **Planner-chosen paths are contained.** `_fetch_explicit_target_docs`
  joined `target_docs` (model output) to the clone root: an absolute path
  or `..` read any file, symlinks were followed, and the bare-name walk
  entered `.git`. Here a path must be plain relative components, no
  symlink on the way, a regular file; the walk skips `.git` and visits in
  sorted order.
- **Colliding page slugs** in one directory get `-2`, `-3` (README links
  and the structure JSON follow). Python wrote both pages to one file. An
  empty slug is `page` / `section` (Python lost the page). A slug is cut
  to 100 characters before the suffix (a title is model output; a name
  over 255 bytes failed Python's export).
- **Artifact order** is README, then the pages in structure order
  (Python: the temp directory's `rglob` order). The manifest's `pages`
  follow it.
- **The manifest has no `faiss_cache_key`, `graph_cache_key`,
  `docstore_cache_key`/`docstore_files`, `bm25_cache_key`/`bm25_files`,
  `unified_db_key`/`unified_db_files`.** No live consumer reads them: the
  Go host and `apps/elitea-web` never mention them; in the Python sidecar
  only `publishing.py` reads `unified_db_key` (with a newest-file
  fallback) on the legacy-runner path, and the legacy engine's
  `artifact_manager` / dead `wiki_loader` read the rest.
  `analysis_cache_key` stays.
- **Full-text search.** FTS5 is gone (no SQLite); production uses
  `SubstringSearch`: Python's own non-FTS fallback of
  `_search_framework_fts` for framework references, a token-phrase match
  for the symbol fallback. The parity gate replays Python's FTS5 answers.
- **Symbols cluster expansion cannot resolve** go to the documentation
  path; Python ran its legacy networkx expansion there (not ported, ~450
  lines). **An empty retrieval** uses the repository context, the fallback
  Python used when its (removed) vector search failed.
- **Orphan seeds are walked in insertion order** (Python: a `set`, hash
  seed order; the reference is patched to match).
- `DEEPWIKI_MAX_SYMBOLS_PER_PAGE` below 1 is ignored (Python looped
  forever); the structure timestamp is UTC. A split part's `page_order`
  (`page_order * 100 + part`, from the model's number) saturates at the
  `i64` bounds (Python's integers have none).
- **The sanitizer is time-bounded.** One page gets 5 s of wall clock
  (`SANITIZE_BUDGET`) and one search at most 1,000,000 backtracking steps;
  past either, the page keeps its unsanitised text with a warning (the
  path Python took when the sanitizer raised). Python had no bound. The
  per-character `match` loops are anchored and pre-tested in O(1), and the
  identifier-led substitutions carry a leading `(?<![A-Za-z0-9_\-])` (same
  matches, proved by `leading_lookbehind_keeps_every_substitution`), so a
  normal page is byte-identical and a 7,900-character line takes
  milliseconds (it took minutes).

Quirks kept: the context and the related-files list are joined with the
two characters `\n`; the sanitizer's literal `'\1'` template, its
"duplicate deactivate" pass on any diagram with `alt `, and the blank
line it drops after a fence; failed pages are exported as empty files.

### Parity

```bash
# Python: index (Phase 1–3, stand-in embedding), then the page path against
# the e2e stub over a FIXED structure; every chat request recorded
PYTHONHASHSEED=0 PYTHONPATH=services/elitea-deepwiki/src python parity/python_pages_dump.py <repo> <ref> [--structure s.json] [--mermaid-pages]
cargo run --release --bin deepwiki-parity -- pages <ref> <out>
python3 parity/compare_pages.py <ref> <out>
# Sanitizer corpus (tests/fixtures/wiki/sanitizer_corpus.jsonl)
PYTHONPATH=services/elitea-deepwiki/src python parity/python_sanitizer_corpus.py <out.jsonl> <file.md>...
```

Measured 2026-10-05 (Apple M4 Pro, release build; the structures are the
cluster planner's against the stub; `--mermaid-pages` answers every page
with broken diagrams so the sanitizer runs on every page). Every page
prompt (context assembly, truncation, ordering), every page, every
artifact (time/uuid names normalised) and every result field is
byte-identical on all four corpora, in both modes:

| Corpus | Pages (doc-only) | Prompts | Artifacts | Pages phase py / rs | Context building py / rs |
| --- | --- | --- | --- | --- | --- |
| spring-petclinic | 23 (4) | 23/23 | 26/26 | 0.27 s / 0.06 s | 21 ms / 6 ms |
| CleanArchitecture | 34 (8) | 34/34 | 37/37 | 0.45 s / 0.06 s | 52 ms / 9 ms |
| leveldb | 54 (2) | 54/54 | 57/57 | 0.81 s / 0.06 s | 434 ms / 27 ms |
| express | 30 (1) | 30/30 | 33/33 | 0.46 s / 0.09 s | 195 ms / 94 ms |

The pages phase includes the stub model (Python drafts sequentially for
the recording; Rust four at a time). A leveldb variant with two
60-symbol pages of the same name (the split, the symbol fallback, the
collision rule) is identical except the three artifacts the collision
suffix changes. The Mermaid corpus — 94 cases: every hand-made repair
case and the real diagrams of the design documents and the Mermaid
README — is byte-identical, fixes and statuses included.

`tests/wiki_pages_golden.rs` runs the same gate without Python over
`tests/fixtures/wiki/golden` (a repository with Python, C++, a Pylon API,
Markdown and YAML; a split page; a page whose targets are only on disk or
outside the checkout). `tests/wiki_contract.rs` holds the export to the
frozen `conformance/provider/fixtures/deepwiki/generation` fixtures, and
`tests/wiki_prompts.rs` re-derives every prompt hash from the Python source.

## Repository analysis and structure planning (`src/structure/`)

ADR-0026 phase 5a: the first two nodes of `generate_wiki`'s agent graph
(`agents/wiki_graph_optimized.py`), `analyze_repository` →
`generate_wiki_structure`. The native runner calls `structure::analysis::analyze_repository` and then
`structure::plan_wiki_structure` with the index rows of Phase 3
(`structure::index::PlannerIndex`: `repo_nodes` with the cluster columns,
`repo_edges` as Phase 2 persisted them, both in row order).

1. **Repository analysis** — one model call (`ENHANCED_REPO_ANALYSIS_PROMPT`;
   the JSON variant under `DEEPWIKI_USE_STRUCTURED_REPO_ANALYSIS=1`). Its
   inputs: the tree of the agent's own file list (an `os.walk` with the
   indexer's default `FilterManager`, not graph discovery), the README as the
   graph builder chunked it (the file-based reader of the live path always
   says "No README file found"), code samples and file statistics. The answer
   is the `repository_context` page generation, ask and deep research read.
   Python's fallbacks over the graph builder's documents are ported: no
   document fails with `ValueError` "No documents found in indexer…"; an
   empty file walk takes the documents' `source` paths as the file list
   (sorted; Python's `set` order reached only file-statistics ties); no
   code sample from the files takes the documents' samples
   (`_extract_representative_code_samples`, whose config test is a
   substring test: `cmd/init.go` holds `ini`).
2. **Planner choice** — `planner_mode` / `planner_type` (the web app always
   sends `cluster`): `cluster`; `agent` / `agentic` / `deepagents`; anything
   else `auto`. `auto` takes deepagents at 2,000 files or a context of 8,000
   tokens (`DEEPWIKI_DEEPAGENTS_FILE_THRESHOLD`,
   `DEEPWIKI_DEEPAGENTS_REPOCTX_TOKENS`), else the classic planner. The
   deepagents planner is a later unit (5d): a deepagents choice falls back
   to the classic planner with Python's warning ("Deepagents structure
   planner failed, falling back to LLM: …"), the path Python took whenever
   deepagents failed, so no job fails for it.
3. **Cluster planner** (`ClusterStructurePlanner.plan_structure`) — the
   architectural cluster map; the candidate validator (demote, split by file
   into at most 5 pages, merge into the sibling with the most edges); the
   coverage ledger (logged only, as in Python); one batched naming call per
   section (`BATCHED_NAMING_*`, on unless `DEEPWIKI_NAMING_BATCHED` is
   false), falling back to one call per page and a section name from the
   pages (or, with `DEEPWIKI_NAMING_ORDER` other than `pages_first`, from
   the top symbols); the fallback section when naming fails outright; the
   `target_symbols` of a page by `PageRank` (`select_central_symbols`).
4. **Classic planner** — one call (`ENHANCED_WIKI_STRUCTURE_PROMPT`), the
   answer's JSON validated as pydantic's lax mode validates it; an answer
   that does not validate becomes the one-page fallback structure.

Every call is the worker's `ChatOpenAI`: the request settings' model and
`max_tokens`, temperature 0.1, streamed by default. Repository text goes in
user messages only.

**Prompts.** Every prompt text is the Python value, byte for byte, in
`src/structure/prompts/*.txt` (compiled in). `PROMPTS_MANIFEST.json` names
the source file, the symbol and the SHA-256 of each Python value;
`tests/structure_prompts.rs` derives the hashes again from the Python
source with python3 (`ast`, no engine import) and from the embedded texts.

**`PageRank`.** Pages are full of exact rank ties, and a tie keeps the
subgraph's node order, so `centrality.rs` reproduces networkx 3.6 / scipy
1.18 / numpy 2.5 operation by operation: the subgraph view's node and
neighbour order, `to_undirected`'s edge merging, the sparse matrix's
duplicate sums, `reduceat` and pairwise sums, and the power iteration.

### Deliberate differences

- **Set order.** Python held a section's nodes, and the nodes a page's
  centrality starts from, in `set`s; networkx's subgraph view iterates such
  a set. Their order follows the hash seed, and it reaches `target_symbols`
  (order and members) and the file count in the naming prompt. Here the
  order is insertion order. Measured on the four small corpora, Python run
  twice with `PYTHONHASHSEED` 0 and 1 against the pinned order:
  `target_symbols` change on 7–20 pages of 17–49, with different MEMBERS on
  2–18 of them; names and every other field are unchanged. A Python wiki is
  not reproducible run to run; this one is.
- **Walk order.** `os.walk` lists a directory in file-system order; here
  each listing is sorted. Only ties of the file-statistics sort see it.
- **Links** are listed as `os.walk` lists them but never read for a code
  sample (Python opened the target).
- **README.** A code file whose path contains `readme`, in a language
  sorted before `documentation`, is not considered; a large README cut into
  generic chunks keeps its chunk order (Python's sort raised `TypeError` on
  them and failed the analysis).
- **Failures.** A failed analysis call or classic structure call is
  returned as the call's error (Python logged it and returned an empty
  wiki). A classic answer whose JSON candidate does not parse is
  `RuntimeError` "Structure generation failed". A stop ends planning;
  every other failed naming call is replaced, as in Python.
- `json.loads` takes `NaN` / `Infinity`; `serde_json` does not (the answer
  then takes the parse-failure path). Integer fields hold `i64` only.
- **Page cap.** A classic answer keeps its first 500 pages
  (`MAX_CLASSIC_PAGES`) in structure order; the rest, and the sections left
  empty, are dropped with a warning. Python drafted every page listed.

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

### Phase 3 clustering (`src/graph/clustering/`)

The live Python path (`hierarchical_leiden` is hard-coded on): hubs out,
sections by Leiden on the file-contracted graph (γ = `max(0.3, 1 −
0.2·log10(files))`), pages by Leiden per section (γ = 1), consolidation to
`clamp(5..20, ⌈1.2·log2 files⌉)` sections and `clamp(8..200, ⌈√(nodes/7)⌉)`
pages, hub re-integration, the `macro_cluster` / `micro_cluster` / `is_hub` /
`hub_assignment` columns. Leiden is vendored `leiden-rs` 0.8.1
(`vendor/README.md`) behind the `Partitioner` trait, one thread, seed 42.
Note: the live caller hands Phase 3 only the first 20 sorted hub ids
(`run_phase2` caps `node_ids`); the port keeps that contract.

```bash
# Python: Phase 1 + 1c + Phase 2 (stand-in SHA-256 embeddings) + Phase 3, every leidenalg call recorded
PYTHONHASHSEED=0 PYTHONPATH=services/elitea-deepwiki/src python services/elitea-deepwiki-engine/parity/python_phase3_dump.py <repo> <dump>
cargo run --release --bin deepwiki-cluster-parity -- replay <dump>          # recorded memberships: must be identical
cargo run --release --bin deepwiki-cluster-parity -- leiden <dump> <out>    # vendored Leiden
python parity/compare_phase3.py <dump> <out>                                # modularity, ARI/NMI, seed spread, targets
```

Replay (non-Leiden logic): identical cluster columns and stats on all five
corpora and the three synthetic fixtures in `tests/fixtures/phase3`
(`parity/python_phase3_fixture.py`, checked by `cargo test`). Vendored
Leiden vs leidenalg on the same inputs, modularity by igraph for both
(gate: ≥ leidenalg − 0.01), with leidenalg's seed 1–5 ARI as the noise
floor (1–3 on elitea-platform):

| Corpus | Sections Q py / rs | ARI (floor) | Pages Q py / rs | ARI (floor) | Final sections / pages py, rs (target) | Python / Rust Phase 3 |
| --- | --- | --- | --- | --- | --- | --- |
| elitea-platform | 0.8869 / 0.8877 | 0.98 (0.96–0.98) | 0.8019 / 0.8031 | 0.67 (0.68–0.71) | 17/147, 17/147 (17/147) | 479 s / 0.99 s |
| spring-petclinic | 0.7400 / 0.7348 | 0.92 (0.64–1.0) | 0.6683 / 0.6667 | 0.96 (0.93–0.96) | 5/9, 5/9 (9/9) | 0.04 s / 0.9 ms |
| CleanArchitecture | 0.8930 / 0.8923 | 0.91 (0.78–1.0) | 0.4095 / 0.4093 | 0.85 (0.89–0.93) | 10/15, 10/15 (10/15) | 0.66 s / 5.9 ms |
| leveldb | 0.6153 / 0.6174 | 0.69 (0.32–0.84) | 0.7414 / 0.7422 | 0.74 (0.69–0.80) | 4/20, 5/20 (9/20) | 1.5 s / 9.5 ms |
| express | 0.6968 / 0.6952 | 0.85 (0.83–0.96) | 0.7566 / 0.7552 | 0.85 (0.93–0.96) | 10/13, 10/13 (10/13) | 0.38 s / 2.8 ms |

Python's time includes its SQLite writes (395 s without them on
elitea-platform; the page-merge loop re-sorts every page per merge). End to
end on elitea-platform, Rust vs Python section ARI is 0.97 against a
Python seed-to-seed 0.95–0.97, page ARI 0.47 against 0.47–0.49.

### Phase 2

```bash
# Python: Phase 2 over its own .wiki.db, stand-in embedding, every index read recorded
PYTHONPATH=services/elitea-deepwiki/src python services/elitea-deepwiki-engine/parity/python_reference.py <repo> <ref-dir> --through phase2
# Rust: the same phase, the index answering from the recording
cargo run --release --bin deepwiki-parity -- graph-dump <repo> <out-dir> --through phase2 --replay <ref-dir>/recording.jsonl
cmp <ref-dir>/edges.jsonl <out-dir>/edges.jsonl && cmp <ref-dir>/stats.json <out-dir>/stats.json
# The accepted difference: Phase 2 reads on a PostgreSQL build-space emulation
python_reference.py <repo> <pg-dir> --through phase2 --search-dsn <dsn> [--dense-postfilter]
python3 parity/compare_phase2.py <ref-dir> <pg-out-dir>
```

Replay, measured 2026-10-05: edges (weights, classes, raw similarity, the
new edges), `is_hub` and the stats dict are byte-identical on all five
corpora. On elitea-platform, 11 node rows differ in `signature` only; these
are the known Go parser rows above, which Phase 2 does not write. Phase 2
on elitea-platform (83,038 orphans, 97,262 new edges, 313 hubs): Python
585 s (FTS5 + sqlite-vec), Rust 2.5 s against the replay store.

PostgreSQL search path (`--search-dsn`), against the FTS5 run, edges
reference → PostgreSQL (Jaccard of the edge sets). elitea-platform is not
measured.

| Corpus | lexical | semantic | directory | bridge | doc | hubs J |
| --- | --- | --- | --- | --- | --- | --- |
| spring-petclinic | 15 → 4 (0.27) | 6 → 53 (0.11) | 98 → 79 (0.39) | 6 → 10 (0.00) | 59 → 60 (0.98) | 1.0 |
| CleanArchitecture | 69 → 23 (0.33) | 9 → 372 (0.02) | 834 → 635 (0.10) | 182 → 206 (0.00) | 396 → 397 (0.99) | 0.0 |
| leveldb | 472 → 480 (0.73) | 104 → 1345 (0.07) | 1158 → 771 (0.65) | 22 → 138 (0.11) | 550 → 550 (1.00) | 1.0 |
| express | 54 → 47 (0.87) | 307 → 787 (0.17) | 581 → 521 (0.15) | 76 → 70 (0.01) | 202 → 202 (1.00) | 0.5 |

The main cause is the vector search with a path prefix. sqlite-vec took
the 20 nearest of ALL vectors and then dropped the ones outside the
directory; pgvector filters first. More same-directory nodes then appear in
both searches, which the RRF threshold needs, so more hybrid edges are
made. With `--dense-postfilter` (sqlite-vec's order) spring-petclinic is
identical. The rest (CleanArchitecture semantic 9 → 82, leveldb 104 → 1257,
express 307 → 647) is lexical: FTS5 read a symbol name as a query
expression and returned nothing for `a::b`, `a.b` or `a-b`;
`plainto_tsquery` folds such a name and matches it. Directory, bridge and
hub changes follow from those edges: anchors and components move.
Structural edges and every weight of an edge both sides have are equal.

### Structure planning

```bash
# Python: Phase 1–3 (recorded), then analyze_repository + generate_wiki_structure
# against the LLM stub (started in process; it records every request body)
PYTHONHASHSEED=0 PYTHONPATH=services/elitea-deepwiki/src python services/elitea-deepwiki-engine/parity/python_structure_dump.py <repo> <py-dump> [--planner cluster|auto]
# Rust: Phase 1 + 1c, Phase 2 on the recording, Phase 3 with leidenalg's memberships replayed,
# then the same two nodes against the stub (LLM_STUB_RECORD=<rs-out>/requests.jsonl LLM_STUB_PORT=…)
cargo run --release --bin deepwiki-parity -- structure-dump <repo> <py-dump> <rs-out> --llm-base http://127.0.0.1:<port>/v1 [--planner …]
python3 parity/compare_structure.py <py-dump> <rs-out>
```

The stub (`services/elitea-deepwiki/e2e/llm_stub.py`) answers the naming
prompts with names made from the listed symbols, and drops the last page of
one batched answer in five so the multi-call path runs too. The gate:
the same requests in the same order (messages byte for byte, model,
temperature, tools, token budget), `structure.json` byte-identical, the
analysis state equal. Rust Phase 3 replays Python's partitions (Leiden
differs by design), and its cluster columns are checked against Python's.
Measured 2026-10-05 (Apple M4 Pro):

| Corpus | Planner | Requests | Sections / pages | Requests / structure | Python analysis + structure | Rust |
| --- | --- | --- | --- | --- | --- | --- |
| spring-petclinic | cluster | 7 | 6 / 17 | identical / identical | 0.11 s | 0.008 s |
| CleanArchitecture | cluster | 11 | 10 / 34 | identical / identical | 0.26 s | 0.011 s |
| leveldb | cluster | 9 (multi-call naming in one section) | 5 / 49 | identical / identical | 0.18 s | 0.010 s |
| express | cluster | 6 | 5 / 23 | identical / identical | 0.17 s | 0.010 s |
| leveldb | cluster, `DEEPWIKI_NAMING_BATCHED=0`, `DEEPWIKI_NAMING_ORDER=sections_first` | 55 | 5 / 49 | identical / identical | 0.29 s | 0.025 s |
| spring-petclinic | auto (classic) | 2 | 2 / 3 | identical / identical | 0.16 s | 0.053 s |
| express | auto (classic) | 2 | 2 / 3 | identical / identical | 0.15 s | 0.052 s |
| elitea-platform (150,942 nodes) | cluster | 17 | 16 / 192 | identical / identical | 15.3 s | 0.40 s |

The times are dominated by the stub's answers; the Rust planner itself
takes milliseconds. On elitea-platform the whole run (Phase 1 to structure) took 55 min
in the reproducible Python reference (inline thread pools; Phase 1 alone
39 min) and 10.1 s in Rust (3.2 GB peak RSS, most of it the Phase 2
recording); the Phase 3 cluster columns were equal to Python's. The only key on one side only is `stream_options` (the
Rust client asks for streamed usage; `LangChain` did not). The golden tests
(`tests/structure_golden.rs`, fixtures from
`parity/python_structure_fixture.py`) replay a scripted model over a
synthetic index that reaches every planner branch, and over a small
repository for the analysis and the classic planner, with no Python and
no network.

## Running

Build with the pinned toolchain (`rust-toolchain.toml`, 1.97.1):

```bash
cd services/elitea-deepwiki-engine
cargo fmt --all -- --check
cargo clippy --locked --all-targets --all-features -- -D warnings
cargo test --locked --all-targets --all-features
```

The tests run the sidecar on a real Unix socket. Two of them read the
shared fixtures in `conformance/provider/fixtures/deepwiki/`. The storage
tests need PostgreSQL with pgvector (they skip without it):

```bash
podman run -d --name dwpg -e POSTGRES_USER=deepwiki -e POSTGRES_PASSWORD=deepwiki \
    -e POSTGRES_DB=deepwiki -p 15436:5432 docker.io/pgvector/pgvector:0.8.5-pg16
DEEPWIKI_TEST_DSN=postgresql://deepwiki:deepwiki@127.0.0.1:15436/deepwiki \
DEEPWIKI_REQUIRE_POSTGRES=1 cargo test --locked --all-targets
```

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
