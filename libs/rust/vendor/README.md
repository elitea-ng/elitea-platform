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
