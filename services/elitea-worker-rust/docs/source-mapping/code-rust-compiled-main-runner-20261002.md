# Compiled Rust Main and runner acceptance

## Ownership and source mapping

Main owns authorization, the compiled-snapshot index, publication quotas, and private content grants.
Worker owns the original Code activation and its selected descriptor journal.
Supervisor owns runtime allocation, capture provenance, release, and confirmed cleanup.
Runner compiles without calling the entrypoint, exports the verified executable, and executes imports in a fresh sandbox.

| Existing boundary | Integrated source | Behavior |
| --- | --- | --- |
| Main control and claim authority | `internal/transport/runtimegrpc/control/compiled_snapshot.go` | Separate Compile, Publish, Read, and Execute roles preserve exact scope and original receipt identity. |
| Main durable repositories | `internal/infra/db/repos/compiled_snapshots.go` | Atomic Ready publication, finite entry and byte quotas, expiry, and original-receipt retention. |
| Existing private object storage | `internal/infra/storage/compiled_snapshot*.go` | Bounded executable staging and reading use signed content grants. |
| Existing service composition | `internal/runtimecomposition/compiled_snapshots*.go` and `cmd/elitea-main/compiled_snapshots.go` | Optional bounded AgentState pool and verified image/profile configuration remain disabled by default. |
| Existing AgentState migrations | `migrations/agentstate/0011_rust_compiled_snapshots.sql` | Snapshot index and retained compiler provenance use owning foreign-key constraints. |
| Authoritative runtime contracts | `libs/proto/elitea/runtime/v1/compiled_code.proto`, `control.proto`, and `sandbox.proto` | The repository-pinned generator reproduces all 40 Go and Python outputs. |
| Existing Rust adapter and lifecycle | `services/elitea-code-runner/src/compiled_*.rs`, `lifecycle.rs`, and `rust_execute.rs` | Fixed compile, export, release, import, and execute helpers retain toolchain and content verification. |

The SDK supplies behavioral reference only; it has no equivalent compiled executable authority.
The ordinary PreparedJob wire bytes and fingerprint remain unchanged.
No user code or executable bytes are added to control RPC.

## Required PostgreSQL proof

Root runs the required fixture against isolated PostgreSQL 18 on the fixed loopback test listener.
The fixture uses a new empty database and touches no deployment database.
Credentials remain in process memory and are redacted from retained test output.

Two top-level tests and eight subtests pass with race detection, zero failures, and zero skips.
They cover captured publication after compiler release, fresh staging claims, and Ready compare-and-set.
Concurrent tests enforce global and tenant entry quotas and executable byte quotas.
Negative tests reject changed original receipts, stale deleters, expired uploads, and cancelled compilers.
Selected reads retain their row lock across expiry; original execution recovery survives index expiry.
Compiler provenance cannot be deleted until snapshot eviction releases its reference.

The first actual run catches an incorrect fixture assertion, not a retention failure.
PostgreSQL `ON DELETE RESTRICT` returns SQLSTATE `23001` for the owning compiler-reference constraint.
The corrected assertion checks both that code and the exact constraint name.
A fresh isolated fixture rerun passes all ten test instances.

## Runner and image proof

The assembled runner passes 64 test instances with locked offline dependencies and one Cargo build job.
Two existing network preparation tests remain ignored; this is not zero-skip network acceptance.
The shipping `Containerfile` target `rust-runtime` builds for Linux arm64.
Its local immutable image identity is `sha256:e1157ae957d81156588ef966d2d406a6f1fb3238316b8d9e43a40b279cbfd854`.

Cached execution skips user-program compilation and user build scripts.
The fixed adapter still verifies the trusted toolchain using `cargo -Vv`.
Linux PID 1 export, descendant quiescence, import, and kernel-limit proof remain separate required checks.
Host component fixtures cannot prove those runtime properties.

## Remaining gates

Verified deployment profiles and migration installation precede cache enablement.
Real content grants, cold publication, warm execution, selected disappearance, and restart recovery still need deployed acceptance.
Docker and Kubernetes must preserve the same roles and original runtime identity.
Indexed cold hydration is integrated in source and renews separate Compile and Content grants for each original import index.
Twenty-nine compiled-path Rust checks pass, including cancellation, role isolation, readiness, and recovery before grant renewal.
Those component checks do not prove Linux transfer, live authority, or service restart behavior.
Worker and Supervisor still require production startup profile selection and trusted backend launch wiring before enablement.
Native cache hits also require separately authorized hydration of their exact runtime assets before cached dispatch.
The initial Linux capture failure came from Cargo's two-link executable output.
The correction verifies the fixed Cargo alias and publishes a fresh immutable single-link copy.
The assembled runner now passes 71 tests and strict Clippy; two network preparation tests remain ignored.
The shipping Linux arm64 runtime builds as `sha256:8c403798c060330cf9836a583ea5d803890b91387b0088f3e64f005c22b00024`.

Restricted Linux cold capture and fresh warm execution pass, including five image-matched process-cleanup tests.
Warm execution performs zero compilation calls and returns the exact expected state.
The probe uses synthetic authority and read-only compiler instrumentation.
It proves runner behavior, not live Main publication, grant delivery, deployment, or browser acceptance.
Compiled admission remains disabled until those separate gates pass.
No capacity or latency guarantee follows from these component checks.
Point 5 remains open.
