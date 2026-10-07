# Frozen Cargo lookup before acquisition

## Implemented behavior

This change follows commit `efa7213e803d6f314ee8b6c482b78f45544140b4`.
Normal generation, contract checks, and focused component tests pass in the owning repository.
Image builds and runtime acceptance remain pending.

Fresh Cargo requests read the operator-selected immutable root before online acquisition.
The selected root comes from the pinned compiled profile catalogue.
The worker binds the saved TOML declaration and the complete native preparation profile.
It requests fresh revision 3 content authority for that fingerprint and root.
The new lookup RPC verifies the existing mTLS peer and content grant.
The preparation supervisor checks its admitted platform, image, policy, timeout, and request fingerprint.
It reads bounded canonical metadata from the existing scoped native content endpoint.
The lookup does not reserve a job, provision a runtime, or claim cleanup.

A matching bundle binds execution through the existing dependency path.
Existing indexed record/archive hydration verifies the frozen package content before dispatch.
The existing compiled lookup then uses the exact prepared request and original compiler provenance.
A genuine absent root or valid unmatched fresh profile keeps the existing acquisition path.
Authority, integrity, transport, and uncertain storage errors do not become acquisition misses.

Recorded preparation continues through its original runtime, publication, and cleanup path.
Recorded compiler or execution work cannot fall back to fresh online acquisition.
The supervisor refuses lookup when the same preparation scope already owns a ledger row.
The patch changes no Main index, storage backend, package version, image pin, or deployment default.

## Contract and ownership

`LookupSandboxDependenciesRequestV1` carries one revision 3 content grant and strict preparation JSON.
The signed grant supplies the root, tenant/project scope, activation, request fingerprint, audience, and expiration.
The request carries no independent content root, execution state, or package bytes.
`LookupSandboxDependenciesResponseV1` carries bounded verified bundle metadata.
An empty response means an absent root or valid unmatched fresh tuple.
An RPC error preserves authority, integrity, and uncertain storage failures.
The lookup does not reuse preparation completion status or terminal runtime receipts.

Worker preparation owns selection and the recorded-work guard.
The existing supervisor content client owns authenticated bounded metadata transfer.
The supervisor owns preparation profile admission and ledger conflict refusal.
Main keeps its current content grants and metadata-last native publication contract.

## Verification

The normal repository generator updates five sandbox Go and Python outputs.
Generated importer files remain unchanged.
The Rust build script generates its bindings from the changed protocol.

Focused tests cover exact Cargo metadata, genuine absence, and hard storage failures.
They reject changed roots, other bundle kinds, malformed metadata, and excessive metadata.
They check every preparation profile field against the retained bundle.
The transport fixture binds fresh grants to the original activation, exact fingerprint, audience, and root.
It proves lookup errors cause no preparation, publication, or hydration RPC.

Current UI acceptance uses saved application 145, version 158, after exact rebuilt image cutover.
Keep the saved CSV, run_id, Rust source, dependency declaration, and timeout unchanged.
Use cold and warm persistent chats, then editor Test with History reload and Restore.
Prove exact results, original bundle identity, no later registry acquisition, and original compiled provenance.
A fresh preparation job count is a diagnostic. It does not prove package downloads.
Trusted Main content hydration remains distinct from registry package acquisition.

## Fresh Main authority and flow tests

Main content grant admission requires the exact live claim, signed command, resource scope, allowed audience, and RUNNING state.
It has no preparation-journal prerequisite and can authorize the fresh pinned-root metadata read.
Generic revision 3 content grants do not approve compiled execution or globally select catalogue roots.
The Worker selects only its private operator-pinned root. Main validates the compiled catalogue independently at later admission.
Original Code visit and final intent validation remain separate and required.

Compiled cold can reuse an already published frozen dependency root.
A genuine fresh absent root can prepare normally. A different resulting root remains ordinary and does not close R13.
Later no-download proof must bind the original published selected root and the bypass of online Cargo.

The production lookup-or-prepare flow now uses one private sequential function. It introduces no custom trait, backend, or allocation.
Five additional tests call that same production function and count lookup, preparation, and original reconciliation actions.
They cover exact hit, fresh miss, errors, recorded preparation, and recorded compiler/execution misses.
These tests use controlled futures. They do not claim real Main authorization or runtime delivery.

## Verification result

All 14 source stages report direct exit zero.
Buf format, lint, build, fixture validation, and contract validation pass.
Generated Go compilation, Python imports, Python syntax, and Rust format pass.
Go reports two packages without tests. These results receive compilation credit only.
Repository locks, module sums, toolchain pins, and production recipes remain unchanged.

The wrapper's changed-path inventory uses incorrect documentation paths and exits one after the completed checks.
The separate read-only reconciliation verifies all source, generated, and documentation paths.
Its frozen completion receipt has SHA-256 `84af6de15620ca6666cb597c7a711490dbb248259ab5c4b987e50becfc019b93`.
The original wrapper failure remains preserved. No passing test reruns.

Run the library test target. The binary target contains no tests.

```sh
CARGO_BUILD_JOBS=1 cargo test --manifest-path services/elitea-worker-rust/Cargo.toml --locked --all-features --lib frozen -- --test-threads=1
CARGO_BUILD_JOBS=1 cargo test --manifest-path services/elitea-worker-rust/Cargo.toml --locked --all-features --lib sandbox::client::preparation -- --test-threads=1
```

The frozen selector passes 30 tests, including all ten new cases.
The preparation client selector passes eight tests. Two cases overlap with the frozen selector.
Both selectors report zero failed and zero ignored tests.
The initial private binary selector exits successfully with zero tests and receives no test credit.
Normal Rust generation runs through `build.rs` in the private build target.
The macOS linker reports a large compact-unwind section warning. Release flags remain unchanged.
These results prove component behavior. They do not prove Main authorization, image delivery, or UI acceptance.
