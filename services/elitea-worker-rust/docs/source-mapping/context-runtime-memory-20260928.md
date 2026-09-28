# Runtime memory during Full-window compaction

## Ownership and reference

Current SDK context middleware provides conversation-summary behavior, not the new compaction or allocation policy.
The behavioral mapping remains in [native history retention](context-native-history-retention-20260921.md).
Rust `agents/model_checkpoint.rs` retains prepared model history after its compaction callback.
`agents/model_scope.rs` applies this ownership separately to child model scopes.
`agents/runtime.rs` enables session event refresh to avoid retaining partial stream history.
`agents/context_compaction.rs` owns structured summary preparation.
These checks measure the deployed implementation without changing those paths.

## Method

The rehearsal worker runs image `sha256:f2e305dc9993834b2b38247f22863db07c401434eda2bc170a6a06e533862e67`.
No compilation runs during the observation.
Docker statistics supply container memory, CPU percentage, and PID counts.
These values are not allocator live-byte counts or process RSS measurements.
Sampling includes ten idle seconds, the browser execution, and twenty seconds after browser completion.
The collector records 137 samples across approximately 69 seconds.
Docker statistics can repeat counters between refreshes; samples are not independent observations.

The first collector fails to parse Docker terminal-control sequences and produces no samples.
That attempt is excluded from performance evidence.
The corrected collector validates baseline samples before starting another model request.

## Result

Fresh headed Playwright executes synthetic chat 711 without browser response interception.
Execution `7fe416c4e9bc29b5a23557385777e67f` succeeds.
The Full window is 1,000,000 tokens, with Auto output and a separate Luna summary request.
Compaction reduces estimated input from 956,800 to 48,585 tokens.
The answer preserves all four project facts and remains correct after reload.
The reload screenshot is visually checked.

| Phase | Samples | Minimum MiB | Median MiB | Maximum MiB | Maximum CPU percent |
| --- | ---: | ---: | ---: | ---: | ---: |
| Idle baseline | 19 | 223.2 | 223.9 | 224.1 | 0.05 |
| Browser execution | 78 | 223.1 | 253.25 | 258.0 | 3.72 |
| After completion | 40 | 256.5 | 257.0 | 257.3 | 0.15 |

The final memory sample is 256.7 MiB, with 17 reported PIDs.
The sampled PID maximum is 18.
The sampled memory peak exceeds the baseline median by 34.1 MiB.
Memory does not return to the original baseline during this observation.

## Limits and next check

This observation does not prove a leak, bounded repeated-run memory, or production concurrency capacity.
Allocator retention, caches, and retained runtime objects require separate investigation.
Run repeated equal workloads and compare post-run plateaus before attributing the retained memory.
Use allocation profiling if those plateaus continue to grow.
Do not extrapolate these measurements to 5,000 users or 1,000 concurrent agents.

Local evidence uses the `elitea-auto-memory` prefix.
The samples and browser result identify chat 711; the earlier unsuccessful collector used chat 710.
No database schema or production runtime behavior changes in this measurement.
