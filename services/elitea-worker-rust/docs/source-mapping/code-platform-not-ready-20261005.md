# Code platform admission observation

Worker must poll Main while its Execute RPC waits for the retained runner. This packet supplies the safe admission observation for that concurrent poll.

| Source | Current behavior | New behavior |
| --- | --- | --- |
| `protocol/sandbox_code_platform_grant.rs` | Running owner checks require both stored bindings. | A separate read-only predicate checks grant expiry and every present admission binding. |
| `sandbox/ledger_code_platform.rs` | Phase filtering makes absent and inert jobs indistinguishable from owner refusals. | A read-only query distinguishes admission from conflicts. Dispatched jobs retain every existing owner fence. |
| `sandbox/docker_code_platform_owner.rs` | Each owner read requires a running runtime. | A typed NotReady returns before any runtime call. |
| `sandbox/service_code_platform.rs` | Every observation failure returns a refusal. | NotReady returns the closed HTTP 200 observation. Refusals keep their status. |
| Main `code_sandbox_owner_client.go` | Each successful body requires a running runtime. | The exact NotReady body becomes a dedicated typed error. |
| Main `code_platform_pump.go` | A missing running observation refuses the step. | Only the typed NotReady result becomes idle after a current parent claim recheck. |

The new body uses schema `elitea.sandbox.code-platform-owner-response.v1` and state `not_ready`. The required fields `runtime`, `pending_call_base64url`, and `reply_published` are null. Only `read_retained_runtime` admits this body.

A verified Main grant can observe genuine absence before admission. The grant binds the peer, route, operation, parent identity, original job, digest, and expiry. Main checks the current parent scope before and after owner IO. Reserved rows must match the request digest and each stored binding. Null admission fields authorize no execution, mailbox read, reply publication, or lease operation.

Cancelled and terminal rows refuse admission observation. Dispatched rows require the existing active owner lease and exact stored bindings. Generic HTTP refusals, malformed bodies, and foreign operations never become NotReady.

The baseline uses current dirty owning source at `ad4da99ff2dcf63b9c92c76e1fadb837c550153a`. The packet records exact hashes. Root owns Worker sibling polling and shared composition. This packet does not change Worker execution, deadlines, replay policy, migrations, or deployment.

Focused fixtures cover admission states, signed authority, wire shape, closed client parsing, parent revocation, and no-effect pump behavior. Test and skip receipts state the measured boundary. Runtime concurrency and recovery remain acceptance work.
