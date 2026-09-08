# Direct toolkit terminal recovery

Status: partial. Encrypted-spool component tests cover replacement claims.
The live old-protocol repair, service replacement, and cross-replica proofs remain open.

This slice applies to `toolkit.execute.read.v1`. It does not implement the broader toolkit Test contract.
See [toolkit-test.md](toolkit-test.md) for `TKTEST-RUST-01`.

## Evidence and ownership

The source review uses merged platform revision `3eb65608`.
The old rehearsal output uses the pre-merge protocol at `046e84a2`.

| Source | Required behavior | Rust owner |
| --- | --- | --- |
| `libs/proto/elitea/runtime/v1/{control,output}.proto` | Bind output and settlement to one execution, generation, claim, and sequence. | `protocol/control.rs` and `protocol/output.rs` |
| Python `transport/output_grpc.py::replace_pending_cancelled_recovery` and `replace_pending_ambiguous_recovery` | Compare the exact saved frame before atomic replacement. Advance only authenticated recovery fences. | `transport/output_grpc.rs::PreparedOutputSpool` |
| Python `transport/output_grpc.py::_validate_deadline_winner` | Accept deadline decisions only with the exact output identity and fence. | `execution/toolkit_output.rs::replay_toolkit_terminal_with_replacement` |
| Main `internal/infra/db/repos/claims.go` | Reject active foreign claims. Replace expired claims with a new lease and fence. | `protocol/control.rs::AcceptedAgentClaim` |
| Main `internal/infra/db/repos/{output_inbox,settlements}.go` | Commit terminal state before transport retirement. Retain duplicate-delivery safety. | Existing toolkit replay, settlement, and retirement coordinator |

Python provides the existing compare-and-swap and recovery ownership model.
This slice deliberately adds exact-result takeover for the Rust direct-read path.
It does not assume that every Python recovery branch already provides this behavior.

## Confirmed rehearsal failure

The isolated database copy contains execution `15f6d2c70254eb8573094fa4d1cc839e`, generation 1.
The copied latest claim attempt is 1,756. The job remains `RUNNING/PREPARING` without committed settlement.
Its signed command deadline has already expired.

The encrypted output snapshot contains one terminal frame at sequence 1.
Authenticated decryption, command identity, and payload digest validation succeed.
The frame contains a direct-read result from claim 1, not a runtime error.
The producer and workload session match. The claim fence does not match.

The worker reports `toolkit_delivery_failed` with `agent_output.invalid_durable_state`.
Output preflight runs before the direct invocation deadline check.
It rejects the old fence before output replay, settlement, or Redis retirement.
After lease expiry, Main issues another claim. The same preflight rejection repeats.

This evidence establishes a terminal-delivery recovery cycle, not repeated tool execution.
It does not identify the first publication or acknowledgement failure.
The original owner logs are still required for that initial trigger.

## Implemented recovery

Preflight first authenticates the encrypted spool and validates its sole terminal frame.
It checks command identity, payload digest, canonical output, and immutable request binding.
An exact current-fence terminal keeps the existing replay path.

An older terminal can use a replacement claim only when these checks pass:

- The accepted claim binds the same signed command and input manifest.
- Its claim attempt and lease epoch both advance.
- Its fence token differs from the saved token.
- Its producer matches the encrypted spool identity.
- Its authenticated handoff watermark precedes the exact saved sequence.
- The saved bytes still match at atomic replacement.

The Main claim creation time determines the initial recovery outcome.
Before the signed deadline, recovery preserves every result field except the new fence and handoff binding.
At or after the deadline, recovery creates the canonical deadline failure under the replacement fence.
The worker clock alone cannot convert an earlier claim into a deadline failure.
An inconsistent earlier local clock refuses deadline replacement and leaves the frame intact.

Recovery consumes the fresh claim and destroys input materialization authority.
The returned type permits only lease supervision and exact terminal replay.
It cannot reserve execution capacity, call Begin, redeem credentials, or invoke the toolkit.

The existing output coordinator still handles Main's later cancellation or deadline decision.
A bound durable acknowledgement must precede settlement authority.
An acknowledgement under the old fence cannot remove the replacement frame.
Transport retirement still requires the existing committed settlement proof.

The safe event `toolkit_output_terminal_rebound` marks successful local replacement.
It contains no arguments, result content, credentials, or fence tokens.

## Verification

`execution/toolkit_output_tests.rs` uses signed fixtures, accepted claim parsing, and encrypted spools.
It covers these cases:

- Successful and failed old terminals recover without fresh invocation authority.
- A later replacement claim recovers again, including a changed workload session.
- Pre-deadline takeover preserves the complete original result and settlement proposal.
- Local clock drift cannot invent an authenticated deadline decision.
- Exact deadline and large sequence boundaries preserve the handoff contract.
- Changed identity, fence, payload, request revision, content digest, or handoff fails without altering saved output.
- Current-fence and empty-spool cases retain their existing paths.

`transport/output_grpc.rs` adds exact-replacement and acknowledgement tests.
They reject result changes and a stale compare-and-swap attempt.
They retain output after an old-fence acknowledgement, then derive settlement authority from the new-fence acknowledgement.
These transport tests use a controlled stream, not deployed Main or Redis.

Restoring the old fence-equality guard makes the focused recovery regression fail.
Removing that guard through the validated recovery path makes the regression pass.

The 2026-09-08 locked, offline suite passes 904 library tests and 84 integration or contract tests.
No tests are ignored. PostgreSQL component tests use isolated databases on the rehearsal service.
Formatting, strict all-target Clippy, and warning-free Rust documentation checks pass.
Eleven new tests cover this recovery slice.
These checks do not constitute a deployed takeover, browser, load, or Kubernetes proof.

## Remaining recovery gates

Horizontal scaling requires transferable state and recovery, not only additional replicas.
The following gates remain open:

| Gate | Required proof | Status |
| --- | --- | --- |
| `REC-RUST-01` | Recover and settle old-protocol rehearsal output before the merged-schema cutover. | Open |
| `REC-RUST-02` | Replace a worker process during result publication. Prove one result, committed settlement, and Redis retirement. | Open |
| `REC-RUST-03` | Transfer ownership to another replica without the original local spool. Recover shared checkpoints and output ownership. | Open |
| `REC-RUST-04` | Recover paused and running nested agents and pipelines. Preserve completed siblings and exact interrupt ownership. | Open |
| `REC-RUST-05` | Prove crash boundaries before and after tool submission, including effect receipts before enabling writes. | Open |
| `REC-RUST-06` | Prove sustained replacement, load, and Kubernetes recovery without an unlimited claim cycle. | Open |

An expired command must reach a durable failure or approved retirement.
It must not restart business work merely to keep a replica busy.
Corrupt or ambiguous output still needs an explicit operator recovery policy.
This slice does not silently discard that evidence or enable effectful toolkit operations.

See [main-sync-20260908.md](main-sync-20260908.md) for the data-preserving deployment gate.
