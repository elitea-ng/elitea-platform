# Code Worker and Supervisor integration, 2026-10-05

This integration composes the frozen Code recovery, workspace, broker and debug sources onto the current Worker source. Current Code source mapping, dependency preparation, compiled snapshots, publication recovery and existing graph node kinds remain the baseline. Legacy SDK `runtime/tools/sandbox.py` and `alita_sdk/runtime/langgraph/nodes/function.py` are business-behavior references, not a literal source port.

## Current-to-new source map

| Current owner | New or composed owner | Resulting behavior |
| --- | --- | --- |
| `graph/code.rs`, `compiler.rs` | Same files plus `code_committed.rs`, `code_workspace.rs` | Preserve fixed-source bytes and omitted-field digests; pin the immutable original saved declaration, validate workspace modes and projection paths at parsing, and carry the same visit into all Code purposes. |
| `graph/code_remote.rs`, `code_runtime.rs` | `code_attempt_remote.rs`, `code_workspace_remote.rs`, `code_workspace_failure.rs` | Original-visit admission and debug export precede dependency work. Typed hydration failures preserve pre/post-dispatch effect knowledge; recovered Started uncertainty produces reconciliation rather than a new execution. |
| `bootstrap.rs`, `transport/input_content.rs`, `runtime_context.rs` | Typed Code-intent, workspace, platform and debug transport modules | Share existing authenticated claim clients, bind canonical receipts to the accepted claim and original saved visit, and retain exact bytes for debug uploads. |
| `protocol/sandbox_authority.rs`, `sandbox_compiled_grant.rs` | `sandbox_workspace_authority.rs`, `sandbox_code_recovery_grant.rs`, `sandbox_code_platform_grant.rs` | Bind purpose, exact original visit, claim identifier, current fence, audience, job/request digest and signed whole-Code intent. Compile and reconcile operations cannot acquire Execute intent. |
| `sandbox/client.rs`, `client_compiled.rs`, `service.rs`, `service_compiled.rs` | `client_workspace.rs`, `service_workspace.rs`, Code-owner services | Plain submission carries intent field 18; compiled Execute carries field 10; Compile/reconcile carry empty intent. Hydration uses optional intent field 7 and exact cursor field 8. Snapshot authorization carries original visit field 11 and grant claims field 42. |
| `sandbox/request.rs`, `ledger.rs`, `docker_supervisor.rs`, `runtime.rs` | Whole-Code binding, owner CAS, retained runtime cleanup and workspace admission | Match prepared request against independently signed intent; persist original instance and provenance; keep cleanup lease authority separate from execution; retain cancellation and fencing before provisioning, transfer and dispatch. |
| Vendored Docker workspace owner and existing Kubernetes owner | Repository content, compiled/broker launch and workspace ports | Both adapters use original instance/container provenance; Kubernetes also retains UID. Content must pass exact inventory, bounded transfer cursor, root and manifest checks before execution. Docker cleanup checks original volume labels and creation identity after its container is gone. |
| Existing graph/checkpointer/output/control owners | Node recovery ledger, committed journal, receipt projector, inspection/ack transport | Commit Started before Code dispatch; preserve attempt history and immutable result restoration; bind operator actions to the exact current activation/revision/fence and publish safe recovery receipts. Existing node kinds remain unchanged. |

## Exact shared saved-family contract dependency

Code needs a typed saved-child capability reference for nested visits. `pipeline/saved_child_scope_provider.rs` uses the exact stage contract from packet `elitea-saved-tool-parallel-factory-20261004`: `for_visit(thread_id,node_id,purpose)` and deny-by-default `for_registration_parent`. Its Code purpose labels remain `code_recovery`, `code_debug`, `code_workspace` and `platform_broker`.

`pipeline/saved_child_http.rs` contains only the exact `SavedChildScopeRef` wire type and validator extracted from the frozen nested-execution source: `scope_id`, positive `revision`, lowercase 64-hex `digest_sha256`, serde serialization/deserialization and `deny_unknown_fields`. `graph/http_action.rs` contains only the shared `HttpActionError` enum, its safe Display strings and Error implementation, extracted from the frozen resilience dependency. No HTTP executor, saved-HTTP dispatch, data-shaping node or new graph node kind is registered by this integration. Broader unregistered HTTP packet files are excluded from delivery.

## Composition and implementation history

The final packet records each actual current source hash, frozen ancestor hash, composed source hash and source origin. Conflicting Code/compiler/module hunks were composed narrowly against current source; whole frozen files were not used to overwrite newer Code mapping, preparation or publication logic. `Cargo.toml` and `Cargo.lock` are unchanged.

Native compilation exposed and fixed concrete integration defects: missing Code debug/workspace fields; missing accepted claim identifier; workspace hydration references; nested debug transport placement and exports; supervisor-only workspace runtime imports; typed error conversions; Reserved-request unwrapping before workspace checks; actual Bollard 0.18.1 map and exec-option types; and sibling Kubernetes repository port visibility. Request fixture constructors and Supervisor mocks were updated for the joint protocol.

Fixture repairs preserve production policy: NUL source remains rejected, admitted non-NUL controls test lossless debug export, anchor fixtures use legal pipeline state/nodes fields, reserved `result` state keys remain rejected, recovery clocks advance through reconciliation, and fresh versus recovered Started failures retain their different safe outcomes. The common workspace manifest fixture is included for the separately owned runner.

## Default admission and ownership

`agent_node_recovery` defaults to false and requires durable checkpoint storage when explicitly enabled. Default runtime factories have no broker profile; Docker/Kubernetes broker profile selectors default to false. Compiled snapshot and repository content profiles remain explicit owner assembly hooks. Main's saved-visit, dynamic-source, workspace, debug and broker authorization gates are owned by Main and remain disabled until explicit operator configuration. No gate, image or deployment configuration is enabled in this source delivery.

Saved-child scope attachment requires an actual containing-family capability from its owner. Missing scope/reference/configuration denies the operation; it is never replaced with root/project authority. The type-only contract does not claim acceptance of the separately owned saved-tool family implementation.

## Verification and limits

- Pinned joint protocol Go/Python generation and Buf lint passed in the root owner's private generator. Rust build.rs generated from the same exact joint sandbox and compiled inputs in this private candidate.
- `cargo check --locked --offline --all-targets --features sandbox-supervisor`, `CARGO_BUILD_JOBS=1`: exit 0, including production Supervisor and all Worker targets. This is compile coverage, not a Cargo test claim.
- Fresh native Worker test binary from exact candidate source and pinned read-only dependencies: Code filter 142 passed; node recovery filter 62 passed. Filters overlap and counts must not be added as unique tests.
- Fresh native Supervisor binary uses a freshly linked private patched `adk-sandbox`: Code filter 156 passed, 6 ignored; sandbox pure filter 159 passed, 54 ignored; signed-grant filter 27 passed. Filters overlap.
- Two loopback transport tests initially hit sandbox EPERM; their focused approved loopback-only reruns each passed. They do not connect to deployed services.
- Patched sandbox crate workspace tests: 79 passed, 6 ignored. These exercise adapter construction, content checks and retained volume identity without starting Docker.
- Changed-source rustfmt passed; adoption patch checks and exact hash roundtrip are recorded in the delivery receipt.

Ignored fixtures require isolated PostgreSQL owner/workspace schemas, explicit mTLS preparation services, Docker images or Kubernetes resources. They were not executed. There was no image build/publication, live Docker/Kubernetes operation, browser acceptance, migration execution, commit, push, merge or deployment in this integration. Root owns live publication recovery and final deployment acceptance.
