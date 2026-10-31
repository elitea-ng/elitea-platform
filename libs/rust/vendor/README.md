# Vendored crates

## `leiden-rs/` — leiden-rs 0.8.1

Leiden community detection (ADR-0026 decision 6), shared since ADR-0027.
Nothing calls it directly: `elitea-graph-algos` (`leiden::partition`) is
its one caller, and the engines reach it through that crate — DeepWiki's
Phase 3 through `graph::clustering::LeidenPartitioner`.

**Why vendored.** It is a single-author crate whose repository is on
gitcode, not GitHub. A copy in this tree cannot change or disappear under
us, and every change to it is a reviewed diff.

| | |
| --- | --- |
| Source | `https://static.crates.io/crates/leiden-rs/leiden-rs-0.8.1.crate` |
| Archive SHA-256 | `c7bbf4703fdbd25078c1842083c8f4e5551ed118a9d00342e309fee0bea46d42` (equals the crates.io index `cksum`) |
| Upstream commit | `fad950eea78feab2c9a839c93986edc3ac8d716f` (`.cargo_vcs_info.json`), `https://gitcode.com/lileeei/leiden-rs` |
| Tree digest | `7091c5ac7ad29c0012244bfc4de7a145dd52f3f22306744ed3356035be15dec6` (79 files; command below) |
| Licence | `MIT OR Apache-2.0`; both texts are in the directory (`LICENSE-MIT`, `LICENSE-APACHE`) |
| Changes | One: the archive's `Cargo.lock` is removed. Every other file is the archive's, byte for byte. The repository's root `.gitignore` ignores `bin/`, so `src/bin/leiden-cli.rs` is added with `git add -f`; keep it, or `diff -r` and the tree digest fail. |

**Verify** (from `libs/rust`).

```bash
curl -sL https://static.crates.io/crates/leiden-rs/leiden-rs-0.8.1.crate -o /tmp/l.crate
shasum -a 256 /tmp/l.crate                       # the archive SHA-256 above
tar xzf /tmp/l.crate -C /tmp && rm /tmp/leiden-rs-0.8.1/Cargo.lock
diff -r /tmp/leiden-rs-0.8.1 vendor/leiden-rs    # no output
(cd vendor/leiden-rs && find . -type f | LC_ALL=C sort | xargs shasum -a 256) | shasum -a 256
```

**Why no `Cargo.lock`.** The archive's lockfile pins 139 packages, most of
them dev-dependencies and optional features this build never compiles.
GitHub's dependency graph reads every `Cargo.lock` in the repository, so it
raised alerts for code that does not ship. Cargo ignores the lockfile of a
path dependency (each consuming service's `Cargo.lock` resolves the
crate's dependencies), so removing it changes nothing in the build.

**How it is built.** A path dependency with `default-features = false`:
no `cli` (clap), no `rayon` (the algorithm runs on one thread, so a seed
gives one result on every machine), no `gryf`. It pulls `rand` 0.9,
`rustc-hash` 2 and `thiserror` 2; `Cargo.lock` pins them. The crate is not
a workspace member (`libs/rust/Cargo.toml` excludes `vendor`), so its own tests, benches and dev-dependencies are not
built or resolved. `vendor/rustfmt.toml` stops `cargo fmt --all` (which
also formats path dependencies) from rewriting it.

**Review (2026-10-05).**

- `unsafe`: none. `src/lib.rs` has `#![deny(unsafe_code)]` and no file
  contains an `unsafe` block. The engine's own `unsafe_code = "forbid"`
  is unaffected: the lint table applies per crate.
- Used: `GraphDataBuilder` (CSR build, undirected; a self-loop counts twice
  in the degree, as in igraph), `Leiden::run`,
  `Leiden::run_with_initial_partition`, `QualityType::RBConfiguration`
  (`Σ_c [e_c − γ·K_c²/(4m)]`, the same objective as leidenalg's
  `RBConfigurationVertexPartition`). Read: `leiden.rs`, `algorithm.rs`
  (local moving, refinement, aggregation), `quality.rs`, `partition.rs`,
  `graph/`.
- Randomness: `StdRng::seed_from_u64(seed)`; the OS generator is used only
  when no seed is given, and the engine always gives one.
- I/O, network, environment, threads: none in the paths used. The unused
  modules (Infomap, LFR and other generators, metrics, multiplex, label
  propagation, fluid communities, WASM, CLI) compile but are never called.

Updating: replace the directory with the new archive's contents, remove
its `Cargo.lock`, update
the table and the review, and re-run the Phase 3 gate
(`parity/compare_phase3.py`).

## `adk-agent/`, `adk-runner/`, `adk-sandbox/` — ADK 2.3.0 runtime extensions

Patched copies of three published adk-rust 2.3.0 crates, moved here from
`services/elitea-worker-rust/vendor/` (ADR-0029 decision 2, stage 2) so every
consumer of `elitea-agent-runtime` builds the same patched adk: the worker and
this workspace both apply them through `[patch.crates-io]`, and a future
desktop host must do the same (a library cannot carry a patch for its
consumers). Keep the exact version (`=2.3.0`) in every manifest that patches
them; `cargo tree -i adk-agent` must show the path source, never the registry.

**The compile-time guard.** `elitea-agent-runtime` names three patched-only
builders (`LlmAgentBuilder::retain_prepared_history`,
`LlmAgentBuilder::steering_source`, `Runner::with_session_event_refresh`) in
three `const _` items in `src/lib.rs`
that are part of every build, not a test. A host that depends on the runtime
but forgets the `[patch.crates-io]` lines resolves the registry 2.3.0 crates,
which have none of them, so its build fails with "no function or
associated item named ..." in `elitea-agent-runtime` rather than running the
unpatched agent loop. Keep those constants when the patches change; remove
them only with the patches.

These packages preserve the published ADK 2.3.0 dependency graph.
History extensions change `adk-agent/src/llm_agent.rs`, `adk-runner/src/runner.rs` and `adk-runner/src/context.rs`.
The optional sandbox supervisor extension changes `adk-sandbox/src/workspace/docker.rs` and adds
`docker_code_jobs.rs` with opt-in real-container tests in `docker_live_tests.rs`.
Each package includes the upstream Apache 2.0 license.

Upstream repository: https://github.com/zavora-ai/adk-rust

Published source commit: `7dd31e2571e0e50ee5039b3edcec1150c106bcc6`.

| Package | Published archive SHA-256 |
| --- | --- |
| adk-agent 2.3.0 | `24e216ce9361811c6f9737aaca0a71bc84bd5f78f6ea855c8ea0f3a561181f8d` |
| adk-sandbox 2.3.0 | `670b384da20cb6e0adcc0f3acd3b93d57fdfdeb24264dd8f7ed1a361794d58a5` |
| adk-runner 2.3.0 | `f359e6760f53e27d35d70a15b469eb4401341e6a96897d564262f59524b597ad` |

`retain_prepared_history` lets the agent retain history prepared by successful model callbacks.
Its default is false. Elitea enables it when its durable compaction callbacks own prepared history.

`tool_call_confirmation` (ADR-0034, the cloud executor) adds an argument-aware
confirmation check to `LlmAgentBuilder`: a `ToolCallConfirmation` asked, in the
pre-dispatch confirmation pass, for a call the name-based
`ToolConfirmationPolicy` does not already confirm. A call it confirms interrupts
exactly as a policy-confirmed one does, and a recorded decision for that call id
and argument fingerprint answers it. 2.3.0 added a live `ToolConfirmationHandler`
inside the tool executor; the check is asked there too, so a run with a handler
cannot skip it. Unset, nothing changes. The worker uses it
to ask before a destructive `run_command` of a cloud workspace.

`steering_source` (steering during execution) adds a `SteeringSource` to
`LlmAgentBuilder`. The loop asks it at each safe point: after the tool results
of a step are in the history and before the next model request. Each returned
`Content` (role `user`) is appended to the history, so the next request, its
`before_model` callbacks (the model checkpoint among them) and every later step
see it. The source never fails the turn. Unset, nothing changes. The cloud
worker takes messages through `ClaimSteeringMessages`; the desktop takes them
from the turn's local queue.

`ToolExecutionStrategy::Auto` keeps the model's order across writes (#1213):
only a run of adjacent read-only, concurrency-safe calls overlaps, and any other
call runs alone in its place. The published crate partitions the whole step and
runs every safe call first (2.3.0 still does), so a read listed after a write
ran before it and read the old file. The desktop runs local tools under `Auto`; the regression is
`d0::tests::a_read_after_a_write_in_one_step_runs_after_it`.

`with_session_event_refresh` reloads the session view after each persisted event.
Its default is false. Elitea enables it to use its durable active-history projection.
All stream fragments still reach the caller. Partial fragments do not accumulate in the refreshed session.
Storage failures stop execution instead of silently using an obsolete session view.

`with_unanswered_tool_calls_kept` (`adk-runner/src/runner.rs`, `context.rs`)
keeps every function call in the conversation history, answered or not, as
2.2.0 did. 2.3.0 drops a call with no result from the model history once a
later user turn exists (`close_interrupted_tool_calls`). The worker resumes a
paused call (a confirmation decision, an `ask_user` answer) by starting a run
whose user event is the decision marker, and its replay adapters need the
paused call in that request to replay it exactly; `replay_history` closes
abandoned calls itself at the provider boundary. Its default is false;
`NativeAgentInvocation::start` enables it for every worker run.

These extensions do not replace the ADK execution loop or implement another summarizer.
The initial session snapshot remains allocated by the native mutable session.
The retention tests do not prove a process RSS bound.

For an ADK upgrade, compare both modified files with these published sources.
Run the worker history, recovery, interruption, and stream delivery tests.
Remove these extensions when upstream supplies equivalent verified behavior.


The sandbox extension adds an explicit non-root offline Code-job policy with finite
CPU/memory limits, no extra swap, a PID limit, read-only root, restricted temporary
mounts, and no added capabilities. It bounds combined decoded output to 1 MiB,
fails on stream/input errors, terminates containers on command failure/timeout,
attempts cleanup after preparation failure, and retains session handles on removal
failure. Shell file-write destinations are arguments, not interpolated commands.
Code-job image snapshots are rejected because tmpfs contents are not captured.

This dependency is optional under `sandbox-supervisor`; the default worker does
not gain Docker access. The upstream in-memory session map is not a durable job
registry. Caller cancellation, lost create acknowledgements, supervisor restart,
persistent receipts, and Kubernetes execution still require supervisor ownership
and integration tests before production admission.


Named Code jobs carry a validated opaque identity and request fingerprint as
Docker metadata. Concurrent creation is arbitrated by Docker's unique name
constraint. A recreated client can observe an existing workload without
repopulating a session or rerunning code. Existing names require reconciliation;
fingerprint mismatch fails. Failed named preparation retains the container/name
for reconciliation after termination. Durable terminal receipts and supervisor
authorization remain required; absence of a container alone never proves that
execution did not happen.


Compiled Code profiles explicitly opt into executable workspace tmpfs through
`with_code_compilation`, which requires the existing finite resource policy.
Default Code workspaces now explicitly specify `noexec`; `/tmp` remains `noexec`
for both profiles. The supervisor binds allowed languages to the image/policy
and keeps Rust-only compilation profiles separate from interpreted profiles.
This is deployment-owned configuration, never a flag supplied by user code.

The Docker extension exposes its configured Code timeout through
`code_job_timeout`. Supervisor admission rejects prepared requests that exceed
this deployment bound before ledger reservation or runtime creation.

The Code job identity also supports an immutable persisted runtime binding.
Docker observations reject a replacement container ID, even when its name and labels match.
The supervisor stores this binding before dispatch in agentstate migration 0008.

## `zavora-slide/`, `zavora-slide-render/`, `zavora-slide-pdf/` — zavora-slide 0.1.1 with three patches

Patched copies of three published crates (ADR-0032 Risks and decision 5), applied
through `[patch.crates-io]` in `libs/rust/Cargo.toml` and
`services/elitea-worker-rust/Cargo.toml`. Only `elitea-office-core` names them
(through `zavora-slide`); the exact `=0.1.1` pins and the isolation test that
reads the lock are unchanged, because the patched crates keep their version.
The desktop host does not link them (no `toolkit-office`).

| Crate | Archive SHA-256 (crates.io) | Upstream commit |
| --- | --- | --- |
| zavora-slide 0.1.1 | `16dd3c2289cc7b520b53f4f7158fedf58cf242371145841686401fd6ee3fa7a8` | `82e407a9bc10f33f7e41b73b188092fa33b62973` (`crates/zavora-slide`) |
| zavora-slide-render 0.1.1 | `897be0dd207ce289a43f3e9497a084f9bdc6f30cad1672b22fa18e25359647a6` | see its `.cargo_vcs_info.json` |
| zavora-slide-pdf 0.1.1 | `25c5d8cd4ff4c3e74815b22dee18cbe308e081eeafe5bc045a5eb277a6b8847d` | see its `.cargo_vcs_info.json` |

Licence: `MIT OR Apache-2.0` (their `Cargo.toml`). Each archive's `Cargo.lock` and
`.cargo-ok` are removed (as for leiden-rs: cargo ignores a path dependency's
lockfile, and GitHub's dependency graph would raise alerts for dependencies that
do not ship); every other file is the archive's, byte for byte, except the files
below.

| File | Change | Why |
| --- | --- | --- |
| `zavora-slide-render/src/lib.rs`, `zavora-slide-render/src/text_shaper.rs`, `zavora-slide-pdf/src/lib.rs` | The three `load_system_fonts()` calls are removed; the bundled Liberation Sans face is the only font. | A render or PDF depended on the fonts of the host it ran on, so previews differed between machines and a deck that names a font the host has was measured with it. Decks that name fonts we do not bundle (Calibri, Arial, others) render with the bundled substitute. `office-core/tests/vendor_patches.rs` fails if any vendored crate calls `load_system_fonts` or `load_fonts_dir`. |
| `zavora-slide/src/presentation.rs` | In `overlay_package` (the save of a deck opened from bytes): tables and chart frames from the build model are spliced into the slide DOM before `</p:spTree>` (renumbered above the file's own shape ids, as the model numbers from 2 on every slide), and pictures inserted into the DOM get their media part and relationship written, instead of re-authoring the slide. The same splice is applied to a slide added in the session. | Upstream dropped a table added to an opened slide on save, and re-authored any slide that gained a chart or a picture from the build model, which does not hold the tables, charts and pictures the file already had: they were lost silently. `tests/vendor_patches.rs` adds a table, a chart, a picture, text and a second table to one slide across four sessions and checks that nothing is lost and that shape ids stay unique. |
| `zavora-slide/src/slide.rs` | `Table::graphic_frame_xml` is `pub(crate)`; `Slide::add_bullets` puts a text box in the file (one paragraph per item) when the slide has no body placeholder. | The first is used by the splice above. The second: a slide added to an opened deck starts blank, and upstream fell through to the build model, which an opened deck never writes, so bullets were reported as added and were not (`set_title` already did the equivalent). |

**What the patches do not fix** (the pptx toolkit guards it instead, see
`families/pptx/ops.rs::save_guarded`): adding speaker notes to a slide that has no
notes part, and replacing the theme, make the library rewrite the whole file
from its build model, which drops what an opened file held that the model does
not (tables, charts, pictures) and re-authors Title-layout slides. The toolkit
reopens the saved file, compares what it holds with what the file held before,
and refuses the edit (nothing is written) if anything was lost. Also not
fixed: a file with no slide list at all drops a slide added to it on save
(`create_presentation` always creates one slide; `add_slide` verifies the
saved file holds the slide).

**Updating.** Replace the directories with the new archives, re-apply the
changes (each is marked by an "Elitea patch" comment),
update the table, and rerun `cargo test -p elitea-office-core` (the hostile
corpus, the isolation test and `vendor_patches`). Drop a patch when the
upstream release carries an equivalent, verified by the same tests.

## `zavora-docx-opc/` — zavora-docx-opc 0.1.4 on zip 9

The OPC reader and writer under `zavora-docx`. `elitea-office-core` reaches it
through `zavora-docx`; nothing names it directly.

**Why vendored.** The published 0.1.4 requires `zip ^8.1`, and it resolves
`zip` 8.6.0. The nightly image scan fails that version: CVE-2026-115905
(CRITICAL, fixed in 9.0.5) and CVE-2026-115907, CVE-2026-115908 and
CVE-2026-115909 (HIGH, fixed in 9.0.0). 0.1.4 is the latest release, and
upstream publishes no fix. A copy here moves the crate to `zip` 9.1.3, the
version every direct declaration in this repository already pins.

| | |
| --- | --- |
| Source | `https://static.crates.io/crates/zavora-docx-opc/zavora-docx-opc-0.1.4.crate` |
| Archive SHA-256 | `5aa20057a69df5fb2c19af1aa58a57112fdafb46385583f6d906de34e5baf96f` (equals the crates.io index `cksum`) |
| Upstream commit | `2be0ad47f5454c16936664c60d625fffe2bb2d3f` (`.cargo_vcs_info.json`), `crates/zavora-docx-opc` |
| Licence | `MIT OR Apache-2.0` (its `Cargo.toml`) |

Keep the version at 0.1.4. `office-core/tests/isolation.rs` asserts that the
lock holds `zavora-docx-opc` 0.1.4. A different version fails that test.

**Changes.** Three. Every other file is the archive's, byte for byte.

| File | Change | Why |
| --- | --- | --- |
| `Cargo.toml` | `zip` moves from `8.1` to `9.1.3`. | zip 8.6.0 carries the four CVEs above. |
| `Cargo.toml` | `zip` takes `default-features = false` and `features = ["deflate"]`. | This is the feature set `office-core` already declares, so the lock gains no package. The published crate took the `zip` defaults, which also add `aes-crypto`, `bzip2`, `deflate64`, `lzma`, `ppmd`, `time`, `zstd` and `xz`. An OOXML package stores each part with Store or Deflate, so the reader still opens every conforming file. A part that uses another method now fails with an unsupported-method error. |
| `src/package.rs` | `OpcPackage::from_reader` calls `entry.name()?` in place of `entry.name()`. | zip 9 returns `ZipResult<Cow<'_, str>>` from `ZipFile::name`. zip 8 returned `&str`. `?` maps the error through `OpcError::Zip`. The name value does not change. |

The archive's `Cargo.lock` is removed (see "Why no `Cargo.lock`" above).
`Cargo.toml.orig` keeps the upstream `zip = { workspace = true }` line; cargo
reads `Cargo.toml`, not that file.

**Verify** (from `libs/rust`).

```bash
curl -sL https://static.crates.io/crates/zavora-docx-opc/zavora-docx-opc-0.1.4.crate -o /tmp/d.crate
shasum -a 256 /tmp/d.crate                          # the archive SHA-256 above
tar xzf /tmp/d.crate -C /tmp && rm /tmp/zavora-docx-opc-0.1.4/Cargo.lock
diff -r /tmp/zavora-docx-opc-0.1.4 vendor/zavora-docx-opc   # only Cargo.toml and src/package.rs
(cd vendor/zavora-docx-opc && cargo check --all-features && cargo test && rm -f Cargo.lock)
```

**Review (2026-10-31).**

- `unsafe`: none. No file in `src/` contains the word `unsafe`. The
  workspace's own `unsafe_code = "forbid"` is unaffected: the lint table
  applies per crate.
- `zip` call sites: `ZipArchive::new`, `len`, `by_index`, `is_dir`, `name`,
  `ZipWriter::new`, `start_file`, `finish`, and `CompressionMethod::Deflated`
  through `SimpleFileOptions`. The writer API is the same in zip 8 and zip 9.
- Round trip: `cargo test` passes 8 tests, `package::tests::round_trip_package`
  among them. A scratch harness also read, wrote and re-read two real `.docx`
  fixtures and compared every part, content type and relationship with the
  unpatched 0.1.4 on zip 8.6.0. The results match.
- I/O, network, environment, threads: none. The crate reads and writes the
  reader and writer that the caller gives it.

Updating: replace the directory with the new archive's contents, remove its
`Cargo.lock`, re-apply the changes (each is marked by an "Elitea patch"
comment), update this table, and rerun `cargo test -p elitea-office-core`.
Drop the copy when an upstream release requires `zip` 9 or later.

## `zavora-slide-opc/` — zavora-slide-opc 0.1.1 on zip 9

The OPC reader and writer under `zavora-slide`. `elitea-office-core` reaches it
through `zavora-slide`; nothing names it directly.

**Why vendored.** The same four `zip` 8.6.0 CVEs as `zavora-docx-opc` above.
0.1.1 is the latest release, and upstream publishes no fix.

| | |
| --- | --- |
| Source | `https://static.crates.io/crates/zavora-slide-opc/zavora-slide-opc-0.1.1.crate` |
| Archive SHA-256 | `127bd9f6cab49a8169e586f4d0fa35add68b909ec8087829d84ff5807d92da88` (equals the crates.io index `cksum`) |
| Upstream commit | `82e407a9bc10f33f7e41b73b188092fa33b62973` (`.cargo_vcs_info.json`), `crates/zavora-slide-opc`; the same commit as the patched `zavora-slide/` above |
| Licence | `MIT OR Apache-2.0` (its `Cargo.toml`) |

Keep the version at 0.1.1. `office-core/tests/isolation.rs` asserts that the
lock holds `zavora-slide-opc` 0.1.1. A different version fails that test.

**Changes.** Two. Every other file is the archive's, byte for byte.

| File | Change | Why |
| --- | --- | --- |
| `Cargo.toml` | `zip` moves from `8.1` to `9.1.3`. | zip 8.6.0 carries the four CVEs above. |
| `src/package.rs` | `OpcPackage::from_reader` calls `entry.name()?` in place of `entry.name()`. | zip 9 returns `ZipResult<Cow<'_, str>>` from `ZipFile::name`. zip 8 returned `&str`. `?` maps the error through `OpcError::Zip`. The name value does not change. |

The published crate already declares `default-features = false` and
`features = ["deflate"]`, so the feature set does not change. The archive's
`Cargo.lock` is removed (see "Why no `Cargo.lock`" above).

**Verify** (from `libs/rust`).

```bash
curl -sL https://static.crates.io/crates/zavora-slide-opc/zavora-slide-opc-0.1.1.crate -o /tmp/s.crate
shasum -a 256 /tmp/s.crate                          # the archive SHA-256 above
tar xzf /tmp/s.crate -C /tmp && rm /tmp/zavora-slide-opc-0.1.1/Cargo.lock
diff -r /tmp/zavora-slide-opc-0.1.1 vendor/zavora-slide-opc  # only Cargo.toml and src/package.rs
(cd vendor/zavora-slide-opc && cargo check --all-features && cargo test && rm -f Cargo.lock)
```

**Review (2026-10-31).**

- `unsafe`: none. No file in `src/` contains the word `unsafe`.
- `zip` call sites: the same set as `zavora-docx-opc` above. The writer API is
  the same in zip 8 and zip 9.
- Round trip: `cargo test` passes 8 tests,
  `package::tests::round_trip_package` among them. A scratch harness also read,
  wrote and re-read four real `.pptx` fixtures. Two of them are
  `zavora-slide/tests/corpus/powerpoint_sample.pptx` and
  `zavora-slide/tests/corpus/deck_with_a_chart.pptx`. The harness compared
  every part, content type and relationship with the unpatched 0.1.1 on
  zip 8.6.0. The results match.
- I/O, network, environment, threads: none. The crate reads and writes the
  reader and writer that the caller gives it.

Updating: as for `zavora-docx-opc` above.

## `zavora-slide-xlsx/` — zavora-slide-xlsx 0.1.1 on zip 9

The embedded-workbook builder that writes chart data for a `.pptx` chart part.
`elitea-office-core` reaches it through `zavora-slide`; nothing names it
directly. It writes a workbook; it never reads one.

**Why vendored.** The same four `zip` 8.6.0 CVEs as `zavora-docx-opc` above.
0.1.1 is the latest release, and upstream publishes no fix.

| | |
| --- | --- |
| Source | `https://static.crates.io/crates/zavora-slide-xlsx/zavora-slide-xlsx-0.1.1.crate` |
| Archive SHA-256 | `27f1acbaab66f4f49d2fc8bf2d4a2ba24c5abc5d62026afa7ab76998f6bf3eb1` (equals the crates.io index `cksum`) |
| Upstream commit | `82e407a9bc10f33f7e41b73b188092fa33b62973` (`.cargo_vcs_info.json`), `crates/zavora-slide-xlsx` |
| Licence | `MIT OR Apache-2.0` (its `Cargo.toml`) |

Keep the version at 0.1.1. `office-core/tests/isolation.rs` asserts that the
lock holds `zavora-slide-xlsx` 0.1.1. A different version fails that test.

**Changes.** One. Every other file is the archive's, byte for byte.

| File | Change | Why |
| --- | --- | --- |
| `Cargo.toml` | `zip` moves from `8.1` to `9.1.3`. | zip 8.6.0 carries the four CVEs above. |

No source file changes. The crate uses the writer API only
(`ZipWriter::new`, `start_file`, `finish`, `SimpleFileOptions`), and that API is
the same in zip 8 and zip 9. The published crate already declares
`default-features = false` and `features = ["deflate"]`, so the feature set
does not change. The archive's `Cargo.lock` is removed (see "Why no
`Cargo.lock`" above).

**Verify** (from `libs/rust`).

```bash
curl -sL https://static.crates.io/crates/zavora-slide-xlsx/zavora-slide-xlsx-0.1.1.crate -o /tmp/x.crate
shasum -a 256 /tmp/x.crate                          # the archive SHA-256 above
tar xzf /tmp/x.crate -C /tmp && rm /tmp/zavora-slide-xlsx-0.1.1/Cargo.lock
diff -r /tmp/zavora-slide-xlsx-0.1.1 vendor/zavora-slide-xlsx  # only Cargo.toml
(cd vendor/zavora-slide-xlsx && cargo check --all-features && cargo test && rm -f Cargo.lock)
```

**Review (2026-10-31).**

- `unsafe`: none. No file in `src/` contains the word `unsafe`.
- `zip` call sites: `ZipWriter::new`, `start_file`, `finish`,
  `SimpleFileOptions::default().compression_method(CompressionMethod::Deflated)`
  in `build`, and `ZipArchive` in the crate's own tests.
- Round trip: `cargo test` passes 11 tests and 1 doc test.
  `output_is_valid_zip` and `output_contains_required_parts` read the built
  workbook back. A scratch harness also built one workbook with both the
  patched and the unpatched crate and compared every entry name and every
  decompressed entry. The results match.
- I/O, network, environment, threads: none. `build` returns a `Vec<u8>`.

Updating: as for `zavora-docx-opc` above.

## `calamine/` — calamine 0.36.2 (upstream 0.36.1 + zip 9)

The XLSX, XLSB, XLS and ODS reader. `xberg` 1.3.10 pulls it in as an optional
dependency; `libs/rust/doc-extract` pins `xberg` at `=1.3.10`. Nothing in this
repository names `calamine` directly.

**Why vendored.** The published 0.36.1 requires `zip ^8.6`, and it resolves
`zip` 8.6.0. The nightly image scan fails that version: CVE-2026-115905
(CRITICAL, fixed in 9.0.5) and CVE-2026-115907, CVE-2026-115908 and
CVE-2026-115909 (HIGH, fixed in 9.0.0). Upstream 0.37.0 moves to `zip` 9, but
`xberg` requires `calamine ^0.36.1`, which excludes 0.37.0. 0.36.1 is the last
release of the 0.36 line. A copy here moves the crate to `zip` 9.1.3, the
version every direct declaration in this repository already pins.

| | |
| --- | --- |
| Source | `https://static.crates.io/crates/calamine/calamine-0.36.1.crate` |
| Archive SHA-256 | `5fa68281b1a76b54a62156474adb06bb380a67e07dd60656e3217152b42183f3` (equals the crates.io index `cksum`) |
| Upstream commit | `0a24c2a9f1e38c0932c1299e633270dc730db505` (`.cargo_vcs_info.json`), `https://github.com/tafia/calamine` |
| Tree digest | `05c46f86ddb64d171f6164977aab2d6df2ba244e165f7b1441dbe7a730e07f5c` (44 files; command below) |
| Licence | `MIT`; the text is in the directory (`LICENSE-MIT.md`) |

**The version is 0.36.2.** The patch must satisfy `xberg`'s `^0.36.1` range,
which is `>=0.36.1, <0.37.0`. Cargo refuses a `[patch.crates-io]` entry whose
version no dependant accepts. Keep the version inside that range. Do not set
0.37.0.

**The migration source.** Upstream 0.37.0 carries the same `zip` 8 to 9 change
(its changelog, PR #741). Each source change below copies the 0.37.0 code for
that call site. The 0.37.0 release also holds XLS, CFB, formula and number
format fixes, a new `workbook_properties()` API and `quick-xml` 0.42. None of
those are here.

**Changes.** Six files, plus the removed lockfile. Every other file is the
archive's, byte for byte.

| File | Change | Why |
| --- | --- | --- |
| `Cargo.toml`, `Cargo.toml.orig` | `zip` moves from `8.6` to `9.1.3`. | zip 8.6.0 carries the four CVEs above. The feature set is unchanged: `default-features = false`, `features = ["deflate"]`. |
| `Cargo.toml`, `Cargo.toml.orig` | `version` moves from `0.36.1` to `0.36.2`. | The patch must stay inside `^0.36.1`. The new patch level marks the copy in `cargo tree` and in each lock. |
| `src/utils.rs` | `build_zip_path_cache` returns `ZipResult<HashMap<String, String>>`. It applies `?` to each `file_names()` item and returns `Ok(cache)`. A `use zip::result::ZipResult;` line is added. | zip 9 returns `ZipResult<Cow<'_, str>>` for each entry name. zip 8 returned `&str`. |
| `src/xlsx/mod.rs` | `Reader::new` calls `build_zip_path_cache(&zip)?`. The shared-strings unit test calls `build_zip_path_cache(&zip).unwrap()`. `read_pictures` calls `zfile.name()?`. | The cache builder is now fallible. `ZipFile::name` is now fallible. `?` maps the error through `XlsxError::Zip`. |
| `src/xlsb/mod.rs` | `Reader::new` calls `build_zip_path_cache(&zip)?`. `read_pictures` calls `zfile.name()?`. | The same two changes. `?` maps the error through `XlsbError::Zip`. |
| `src/ods.rs` | `read_pictures` calls `zfile.name()?`. | `ZipFile::name` is now fallible. `?` maps the error through `OdsError::Zip`. |

The archive's `Cargo.lock` is removed (see "Why no `Cargo.lock`" above).

**Verify** (from `libs/rust`).

```bash
curl -sL https://static.crates.io/crates/calamine/calamine-0.36.1.crate -o /tmp/c.crate
shasum -a 256 /tmp/c.crate                       # the archive SHA-256 above
tar xzf /tmp/c.crate -C /tmp && rm /tmp/calamine-0.36.1/Cargo.lock
diff -r /tmp/calamine-0.36.1 vendor/calamine     # only the six files in the table
(cd vendor/calamine && find . -type f | LC_ALL=C sort | xargs shasum -a 256) | shasum -a 256
(cd vendor/calamine && cargo check --all-features && cargo test --all-features --lib && rm -rf Cargo.lock target)
```

Run `cargo test --all-features --lib`, not `cargo test`. The published archive
sets `exclude = ["tests/**/*"]`, so it holds no `tests/` directory. 38 doc
tests open a path under `tests/`, and they fail with `NotFound` on the
unpatched archive too. The 44 library tests pass.

**Review (2026-10-31).**

- `unsafe`: none. No file in `src/`, `benches/` or `examples/` contains the
  word `unsafe`. The crate declares no `unsafe_code` lint of its own. The
  workspace's `unsafe_code = "forbid"` is unaffected: the lint table applies
  per crate.
- `zip` call sites: `ZipArchive::new`, `len`, `by_name`, `by_index`,
  `file_names`, `ZipFile::name`, `ZipError::FileNotFound`. The patch touches
  only `file_names` and `name`. The other call sites keep their zip 8
  signatures.
- Name decoding moves. zip 8 decoded every entry name when it read the central
  directory. zip 9 decodes a name on each `name()` or `file_names()` call: it
  takes the bytes as UTF-8, or else as CP437. A decode failure now reaches the
  caller as `XlsxError::Zip`, `XlsbError::Zip` or `OdsError::Zip`. The decoded
  value for a conforming file does not change.
- I/O, network, environment, threads: none. The reader works on the
  `Read + Seek` value the caller gives it.

Updating: replace the directory with the new archive's contents, remove its
`Cargo.lock`, re-apply the six changes, update the table and the review, and
run the doc-extract spreadsheet tests.

## `zavora-xlsx/` — zavora-xlsx 0.1.2 (zip 9)

A copy of the published crate, migrated from `zip` 2 to `zip` 9.1.3. Apply it
through `[patch.crates-io]` in `libs/rust/Cargo.toml` and
`services/elitea-worker-rust/Cargo.toml`. Only `elitea-office-core` names it.

**Why vendored.** The published 0.1.2 requires `zip ^2`, so it pulled a second
`zip` into every build: 2.4.2 here and 2.6.1 in the worker. The nightly image
scan failed the release on those copies (CVE-2026-115905 CRITICAL, fixed in
9.0.5; CVE-2026-115907, CVE-2026-115908 and CVE-2026-115909 HIGH, fixed in
9.0.0). 0.1.2 is the latest published version and upstream has no fix, so a
version bump cannot remove them. This copy links the platform's own `zip`
9.1.3, which `elitea-office-core` already pins.

| | |
| --- | --- |
| Source | `https://static.crates.io/crates/zavora-xlsx/zavora-xlsx-0.1.2.crate` |
| Archive SHA-256 | `c5e2a142df28712030b103d2ac9dad6d127ed026a2d9053852c799bff7b63e67` (equals the crates.io index `cksum`) |
| Upstream commit | `6e47878cc5631c48fd5cf411785a78a458dca2cb` (`.cargo_vcs_info.json`), `https://github.com/zavora-ai/zavora-xlsx` |
| Tree digest | `60d7fdf69b94d5e183111f23c73efbfb8194a0ed65781c8250a2044bdd66f611` (242 files; command below) |
| Licence | `Apache-2.0` (its `Cargo.toml`); the text is in the directory (`LICENSE`) |
| Version | `0.1.2`, unchanged. `office-core/tests/isolation.rs` asserts the exact `=0.1.2` pin and the locked version. A patch crate must keep the version it replaces. |

**Changes.** Every other file is the archive's, byte for byte.

| File | Change | Why |
| --- | --- | --- |
| `Cargo.toml`, `Cargo.toml.orig` | The `zip` requirement becomes `9.1.3`. `default-features = false` and `features = ["deflate"]` stay as upstream set them, which is what `libs/rust/office-core/Cargo.toml` takes for its own `zip`. | This is the whole point of the copy. Both manifests change so the generated and the original agree. |
| `src/zip/zip_reader.rs` | `ZipReader::new` reads each entry name through `let Ok(name) = entry.name()`. | `zip` 9 returns `Result<Cow<str>, ZipError>` from `ZipFile::name`, not `&str`: a name is decoded on demand and can fail. An entry whose name does not decode stays out of the case-insensitive path cache, exactly as an entry that does not open already did. |
| `src/workbook/mod.rs` | Two entry-name scans (the pass-through part list and `pictures`) use `.and_then(\|e\| e.name().ok().map(Cow::into_owned))`. | The same signature change. Both scans already dropped an entry that `by_index_raw` refused, so they now drop an undecodable name the same way. |
| 10 files under `tests/` | 37 call sites add one `unwrap()` after `name()` or `name_for_index(i).unwrap()`. | The same signature change. `grep -rn '\.name()\.unwrap()\|name_for_index(i)\.unwrap()\.unwrap()' tests/` lists every one. |
| 4 files under `tests/` | 10 tests get one `std::fs::create_dir_all("output")` call, each marked `// Elitea patch`. The files are `format_variants_tests.rs` (4 tests), `named_ranges_crud_tests.rs` (2), `streaming_perf_tests.rs` (3) and `theme_color_tests.rs` (1). | The tests write to a relative `output/` path that nothing creates, so they failed with `Io(NotFound)` on a clean checkout. `tests/demo_integration.rs` already creates its directory this way. The call makes the suite pass without a prepared directory. |
| `tests/read_sheet_protection_tests.rs`, `examples/read_sheet_protection.rs` | 4 tests and 1 example get one `std::fs::create_dir_all("target")` call, each marked `// Elitea patch`. | They write to a relative `target/` path. Cargo creates `target/` inside the crate only when `CARGO_TARGET_DIR` is unset, so the 4 tests failed with `Io(NotFound)` when it points elsewhere, a common CI setting. The call makes them pass with any target directory. |
| `Cargo.lock` | Removed (see "Why no `Cargo.lock`" above). | Cargo ignores a path dependency's lockfile, and GitHub's dependency graph reads every lockfile in the repository. |
| `AGENTS.md` | Removed. | The archive ships the upstream project's own agent instructions. An agent that works here finds the file and follows it, and it prescribes a five-crate workspace and `cargo test --workspace`, which do not exist in this repository. |

The two library changes carry an "Elitea patch (zip 9)" comment. Nothing else
in the crate names `zip`: the reader and the writer wrap it in
`src/zip/`, and `src/error.rs` holds `Error::Zip(zip::result::ZipError)`.
`ZipError::FileNotFound`, `ZipArchive::{new, by_name, by_index_raw, len}`,
`ZipWriter::{new, start_file, finish}`, `write::SimpleFileOptions` and
`CompressionMethod::Deflated` keep their `zip` 2 names and shapes, so no other
library call site changes. `ZipArchive::name_for_index` returns
`Option<Result<Cow<str>, ZipError>>` in `zip` 9, not `Option<&str>`; only tests
call it.

**Verify** (from `libs/rust`).

```bash
curl -sL https://static.crates.io/crates/zavora-xlsx/zavora-xlsx-0.1.2.crate -o /tmp/x.crate
shasum -a 256 /tmp/x.crate                     # the archive SHA-256 above
tar xzf /tmp/x.crate -C /tmp && rm /tmp/zavora-xlsx-0.1.2/Cargo.lock
diff -r /tmp/zavora-xlsx-0.1.2 vendor/zavora-xlsx   # the 20 files in the table,
                                                   # and `Only in ...: AGENTS.md`
(cd vendor/zavora-xlsx && find . -type f | LC_ALL=C sort | xargs shasum -a 256) | shasum -a 256
(cd vendor/zavora-xlsx && cargo check --all-features && cargo test --all-features)
rm -rf vendor/zavora-xlsx/output vendor/zavora-xlsx/target vendor/zavora-xlsx/Cargo.lock
```

**What the tests showed (2026-10-31, `zip` 9.1.3 pinned).** 45 suites, 602
tests, 0 failures with `--all-features`; 577 tests, 0 failures with
`--no-default-features`, which is how `elitea-office-core` links it. Two
doctests are `ignore` upstream (`async_io`, `serde_support`).

The round-trip suites matter most, and they pass. `demo_integration` (45 tests)
writes workbooks with charts, tables, pictures, pivots and comments, reopens
them, and reads the parts back. `edit_tests`, `formatting_survives_an_edit` and
`tasks_77_82_tests` open a saved file, edit it and save it again.
`integration_tests`, `phase3_tests` and `edit_tests` reopen the result with
`calamine` (16 call sites), a second zip and xlsx parser, so the archives this
crate writes through `zip` 9 are proven readable by something other than
`zip` 9.

Ten tests in four suites failed on a clean checkout because they write to a
relative `output/` directory, and the published archive excludes `output/`
(`exclude` in its `Cargo.toml`). Each test now creates the directory, as the
table above records, so 602 of 602 tests pass with no prepared directory. This
was an upstream packaging gap, not a `zip` 9 effect. Do not commit the
directory; the suites create it and `.gitignore` holds it.

**Review (2026-10-31).**

- `unsafe`: 27 lines, all behind a feature this build does not enable.
  `src/cffi.rs` holds 26 (raw-pointer FFI) under `#[cfg(feature = "cffi")]`.
  `Workbook::open_mmap` holds one (`memmap2::Mmap::map`) under
  `#[cfg(feature = "mmap")]`. `elitea-office-core` takes the crate with
  `default-features = false` and no features, so neither compiles. The
  workspace `unsafe_code = "forbid"` lint is per crate and does not reach a
  path dependency.
- The `zip` paths hold no `unsafe`. `ZipReader` keeps the crate's own 200 MB
  decompressed-entry cap; the migration does not change it.
- `zip` 9 writes the same entries with the same method (`Deflated`), but its
  deflate backend is not byte-identical to `zip` 2's. Compare content, never
  archive bytes.
- `Error::Zip` now wraps `zip` 9's `ZipError`, so its `Display` text changes.
  `elitea-office-core` maps it with `e.to_string()` into
  `OfficeError::Malformed`, so a test that asserts the exact message needs a
  new expectation.
- `tests/serde_tests.rs:154` warns `dead_code`: the fields of `NumRecord` are
  never read. The warning is correct and it is kept. The struct declares the
  schema that `test_serde_type_mismatch_error` deserializes into, and that test
  asserts the read fails, so the success path never writes the fields. The
  field names map to the header cells `name` and `value`, so a rename breaks
  the test, and a read of the fields turns a negative test into a positive one.
  An `#[allow(dead_code)]` would hide an upstream signal. `elitea-office-core`
  never compiles this suite, so the warning does not reach a platform build.

Updating: replace the directory with the new archive's contents, remove its
`Cargo.lock`, re-apply the changes in the table, update the table and the
review, and rerun `cargo test -p elitea-office-core` (the hostile corpus and
the isolation test). Drop this copy when an upstream release requires `zip` 9.
