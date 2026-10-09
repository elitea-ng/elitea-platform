# Numeric normalization log test flake

PR elitea-ng/elitea-platform#1172, branch `fix/worker-numeric-normalization-log-flake`, based on `origin/main` at `543de2912` (#1162). Evidence was
collected on 2026-10-09. This is a test-only change. No runtime code, contract or configuration changes.

The Worker test `agents::graph::compiler_identifier_tests::numeric_normalization_is_logged_as_counts_only` (added by
#1158) failed intermittently when the graph suite ran in one process (`cargo test --offline --lib agents::graph::`).
It always passed when run alone.

## Business behaviour

**Kept:** a pipeline whose node identifiers are written as YAML numbers (e.g. `id: 7777`) is admitted and normalized to
the string form. The normalization is logged once, as counts only (`normalized_identifier_count`,
`numeric_identifier_count`), never with identifier or template text. The emitting code,
`services/elitea-worker-rust/src/agents/graph/compiler.rs:778-785`, is unchanged.

**Not ported:** nothing. The current platform does not log this normalization.

## Root cause

The test captured logs with `tracing::subscriber::with_default`, which installs a thread-scoped dispatcher. Other
graph tests run on parallel threads with no subscriber and reach the same `tracing::warn!` callsite in
`PipelineDefinition::from_yaml`. `tracing` caches each callsite's interest process-wide. The most likely explanation,
consistent with the evidence below, is that when another thread registered the callsite first, the cached interest
excluded the scoped subscriber, so it never saw the event and the captured log was empty. The evidence shows the empty
capture. It does not show the cached interest value directly.

Baseline evidence on `543de2912`, same test binary, `agents::graph::` filter, 12 runs: **4 failed** (runs 2, 4, 11
and 12). Each time the only failure was this test, panicking at the `pipeline_legacy_identifier_normalized` assertion
with an empty log.

## Changes

| Path | Change |
|---|---|
| `services/elitea-worker-rust/src/agents/graph/compiler_identifier_tests.rs:478-525` | The parent test re-runs the test binary (`current_exe`) with `--exact …::numeric_normalization_log_child --nocapture --test-threads=1` and `ELITEA_IDENTIFIER_LOG_CHILD=1`. The child installs a **global** `fmt` subscriber that writes to stdout, then parses the same numeric pipeline and prints `CHILD-RAN`. Without the env var, the child test returns immediately, so the parent process never installs a global subscriber. The parent asserts on the child's stdout and prints the child's stderr if it exits non-zero. |

The assertions are unchanged: the log contains `pipeline_legacy_identifier_normalized` and
`numeric_identifier_count=2`, and it contains neither the identifier `7777` nor the template text
`private-template-text`. One assertion is new: `CHILD-RAN`, which proves the child actually ran the parse rather than
matching no test.

This is the same pattern as `shaping_failure_logs_carry_no_item_values` on `feat/graph-shaping-split-out-aggregate`
(`a47f35175`). The process-replacement tests on `main` (`agents/context_compaction_tests.rs:841`,
`agents/instruction_authority_tests.rs:237`) re-run the binary the same way.

Other approaches were rejected:
- `tracing::callsite::rebuild_interest_cache()` inside the scoped block still races with other threads that register
  the callsite.
- A process-wide test subscriber would change logging for every other test in the binary.

## Tests

| Run | Result |
|---|---|
| Baseline `origin/main` `543de2912`, `agents::graph::`, 12 runs | 4 failed / 8 passed (394 tests each) |
| Fix, `cargo test --offline --lib agents::graph::`, 20 runs | **20/20 passed, 0 failures** (395 tests each, 1073 filtered out) |
| Fix, full `cargo test --offline --lib` | 1458 passed, 0 failed, 10 ignored (the existing PostgreSQL-gated tests) |
| Mutation: the warn also logs the first YAML line (`entry_point: 7777`) | Fails: "an identifier leaked" |
| Mutation: the warn is never emitted | Fails at the `pipeline_legacy_identifier_normalized` assertion |
| `cargo clippy --offline --all-targets --all-features -- -D warnings` | Clean |
| `cargo fmt --check` | Clean |

`graph-extensions-rehearsal` does not exist on `main`. It is defined only on `feat/graph-shaping-split-out-aggregate`,
so that variant was not run here. The fix reaches that branch with its next merge from `main`. The fix does not depend
on any feature: the child process is the same test binary.

## Performance

The test now spawns one child process. Measured graph-suite wall time (libtest `finished in`): baseline 1.05–1.24 s
over 12 runs, fix 1.04–1.12 s over 20 runs, so there is no measurable change. Runtime code is unchanged.

## Durability

Not applicable. No runtime, persistence or recovery path is touched.

## Resilience

The test is now deterministic. The child owns a global subscriber with one test thread, so no other test can register
the callsite with a different interest. The parent fails readably, printing the child's stdout, if the child exits
non-zero, never runs (`CHILD-RAN` missing), or logs the wrong content.

## Security

The no-leak guarantee is still proven, and the proof is now reliable: a flaky leak test that silently captured nothing
could not have caught a regression. The mutation above shows that a leaked identifier fails the test. The child's
environment variable selects a test mode only. It carries no data.

## Recovery guarantees

No component × phase row changes. This is a test-only change, and the platform recovery matrix is unchanged.

## Browser evidence

Not applicable. There is no runtime or UI behaviour change to observe in a browser.

## Fixtures

The YAML is inline in the test, unchanged from #1158. There is no database or UI fixture.

## Follow-ups

- Other tests that capture logs with `tracing::subscriber::with_default` can flake the same way when their callsite
  is shared with parallel tests (`agents/assembly_tests.rs:1363` and `:1399`, `sandbox/docker_compiled_observability_tests.rs:33`, `sandbox/service_compiled_observability_tests.rs:27`, and `set_default` in `agents/image_turn_tests.rs:95`). None was seen failing
  in these runs. Convert them if they flake.
