# Package-backed Code data-processing benchmark

This record extends the [dependency profile verification](code-dependency-profiles-20261001.md).
The user requests complex, repeatable processing across all four supported Code languages.
The fixture processes 20,000 deterministic transactions without an LLM call.
Gate 5 remains open after this verification.

## Source mapping

| Business behavior or current source | New source | Verification |
| --- | --- | --- |
| SDK `infra/data/sandbox/main.ts::install_imports` installs Python dependencies for Code execution. | Rust runner `src/execute.rs` selects `adapters/python.mjs`. The immutable profile supplies a frozen package closure. | Inline installation loads prepared `python-slugify` and `python-dateutil` without network access. |
| Current Code nodes receive selected graph state and publish their results. | Rust worker `src/agents/graph/code_state.rs`, `code_result.rs`, and `code_remote.rs` retain the existing typed state contract. | Four deployed nodes transfer compressed records and independently reconcile identities, counts, category totals, and weighted totals. |
| The new runtime supports JavaScript and TypeScript package imports. No equivalent current-platform package contract is assumed. | `adapters/prepare_javascript.mjs` and `Containerfile` prepare the native locked npm graph. | CSV parsing, tuple validation, compression, and typed async reconciliation run offline. |
| The new runtime supports isolated Rust Code execution. No current-platform Rust execution behavior is assumed. | `services/elitea-code-runner/src/rust_execute.rs` copies a prepared Cargo manifest and lockfile, then compiles offline. | A struct, trait, implementation, async reducer, and `futures::join_all` reconcile the full dataset. |
| The current platform does not define this benchmark's durable sandbox receipts. | Rust worker `src/sandbox/dispatch.rs` records delivery identity. The supervisor persists results in `elitea_runtime.sandbox_jobs`. | Repeated Docker receipt reads remain identical. Each browser run settles four jobs and removes its execution Pods. |

The fixture lives in `scripts/runtime/fixtures/code-data-processing/`.
The independent expected result uses deterministic arithmetic and Python's standard SHA-256 implementation.
The probe lives in `scripts/runtime/probe_code_data_processing.py`.
Its README defines build steps, runtime limits, timing boundaries, and browser acceptance.

## Dependencies and boundaries

Python installs `python-slugify==8.0.4` and `python-dateutil==2.9.0.post0` from the prepared closure.
The closure includes `text-unidecode==1.3` and `six==1.17.0`.
JavaScript and TypeScript use `csv-parse@5.6.0`, `zod@3.24.2`, and `fflate@0.8.2`.
Rust uses locked `serde`, `base64`, `flate2`, and `futures` dependencies.
The fixture's Cargo lockfile contains the exact resolved closure and checksums.

The profiles extend only the test runtime images. Default approved profiles remain unchanged.
Package preparation occurs during image construction. The execution network remains disabled.
Inline Python installation does not prove arbitrary on-demand acquisition.
The Cargo profile proves operator-prepared dependency execution, not user-selected runtime packages.

Each isolated Docker invocation uses one CPU, 512 MiB memory, and a 256 MiB workspace.
The container uses UID 10001, read-only root storage, and no additional capabilities.
Every job starts a new process and receives private writable state.
Async helpers run within that process. They do not increase the CPU quota.
Intermediate results stay below 160,000 bytes. The final result stays below 800 bytes.
Compression keeps this fixture within existing state limits. It does not replace future artifact-backed input.

## Docker verification and performance

Three complete runs pass every deterministic expected field.
Each run verifies identical repeated terminal receipts, terminated processes, and test-container cleanup.
The first run uses cached local images. It does not include image download or dependency preparation.
All Rust invocations compile their dependency graph and source again.
These repeat runs do not use a compiled-artifact cache.

| Stage | First processing | Median processing | First sandbox | Median sandbox |
| --- | ---: | ---: | ---: | ---: |
| Python | 213.639 ms | 238.859 ms | 2,247.540 ms | 2,118.190 ms |
| JavaScript | 132.222 ms | 185.457 ms | 217.417 ms | 256.407 ms |
| TypeScript | 82.189 ms | 105.641 ms | 308.546 ms | 330.067 ms |
| Rust | 17.031 ms | 16.304 ms | 6,358.500 ms | 6,195.843 ms |

The complete probe takes 10.009, 9.501, and 9.764 seconds.
This measurement includes container creation, result reads, verification, and cleanup.
Sandbox measurements use Docker's authoritative start and finish timestamps.
Program measurements exclude startup, module loading, and Rust compilation.
Python's program measurement includes its inline offline installation calls.
These local results describe this fixture. They do not prove concurrent-user capacity or production latency.

## Deployed browser verification

Pipeline 141 contains the four fixed-source Code nodes.
Its editor Test chat and persistent participant chat 771 return the exact expected result.
The result contains 18,947 accepted records, 1,053 rejected records, and 824 refunds.
The reconciled total is 866,440,800 cents. The weighted total is 8,662,764,360,486.
The SHA-256 is `6a3c4d063a5ba0c21949cf7d67d4bda843cb50a0bc76169c19e2c5f7e272326d`.
Persistent browser reload preserves one unchanged result. Both browser checks report no console errors.

| Stage | Editor receipt | Persistent receipt | Persistent program |
| --- | ---: | ---: | ---: |
| Python | 4.348 s | 4.377 s | 196.951 ms |
| JavaScript | 2.282 s | 1.781 s | 128.336 ms |
| TypeScript | 1.781 s | 2.285 s | 101.219 ms |
| Rust | 7.849 s | 7.906 s | 15.355 ms |

The four-receipt spans are 16.374 and 16.541 seconds.
These database timestamps include dispatch, Pod startup, execution, compilation, polling, and receipt settlement.
They exclude browser observation delay. All eight jobs complete, and no execution Pod remains.
The application uses the existing hybrid rehearsal. This does not prove a complete Kubernetes deployment.

## Runtime identity

| Backend | Deno profile | Rust profile |
| --- | --- | --- |
| Docker canonical image ID | `sha256:40fc267539dca68c03ae56182d9b96585aa15dd640e3e64c9d580fe0f537584a` | `sha256:a79f0664adfd7676c168e98e1052c26da8c539219060b4eea3f58f5a3ef335a7` |
| Imported Kubernetes manifest | `sha256:f59135a40af27dae2c4b16c0957e927a09f930ba6b99913d17198297467a73b4` | `sha256:a8f060b0a9207122fe031fbf73b308b43f72732d10155b24ce18533ed7115914` |

The supervisor profiles and worker language bindings use the matching Kubernetes manifest identities.
Existing private backups preserve the previous rehearsal profiles.
No application schema or database data migration is added.

## Remaining work

On-demand package preparation and authorization remain open.
Compilation caching, scoped platform-client access, and debug artifacts remain open.
Dynamic state-supplied source requires a separate admission decision.
Repeat this fixture when artifact-backed state becomes available.
Retain separate capacity and concurrent-pipeline acceptance requirements.
