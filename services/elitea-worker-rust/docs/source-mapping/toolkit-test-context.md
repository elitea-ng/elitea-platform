# Toolkit Test runtime context

## Source mapping

| Current platform source | Required behavior | New platform source |
| --- | --- | --- |
| `projects/centry/pylon_main/plugins/elitea_core/rpc/application.py::test_toolkit_tool_sio` | Build toolkit Test context with security policy and optional model selection. | Main `internal/application/toolkitcalltool/{resolver,runtime_context,service}.go` |
| `projects/centry/pylon_main/plugins/elitea_core/api/v2/test_toolkit_tool.py` | Bind the authenticated project, actor, toolkit, and tool arguments. | Main `internal/api/v2/toolkitrun/response.go::DecodeRequest` |
| `projects/elitea-sdk/elitea_sdk/runtime/toolkits/security.py` | Enforce configured toolkit blocks and sensitive-tool policy. | Main `internal/domain/guardrails/policy.go`; Rust toolkit delivery and native tool security checks |
| Python worker `handlers/toolkit_call_tool.py` | Keep toolkit settings, arguments, and runtime context separate. | Main `internal/application/toolkitcalltool/input_bundle.go`; Rust `src/execution/toolkit_delivery.rs` |
| Current platform credential expansion | Resolve credentials only for an authorized invocation. | Main `internal/infra/storage/configurations_materializer.go::MaterializeContent` |

The current platform defines behavior. Its RPC framework and mutable runtime state do not define the new implementation.

## Implemented contract

Main freezes three immutable JSON entries for `toolkit.call_tool.v1`:

| Fixed entry ID | Semantic role | Content owner |
| --- | --- | --- |
| `toolkit-settings` | `toolkit.call_tool.settings` | Project-scoped saved toolkit resolver |
| `toolkit-arguments` | `toolkit.call_tool.arguments` | Caller input, with exact JSON bytes retained |
| `toolkit-runtime-context` | `toolkit.call_tool.runtime_context` | Main policy resolver and optional model reference |

The runtime context requires `toolkit_security` with explicit block and sensitive-tool collections.
Main uses `platformconfig.GuardrailPolicyAdapter`, which also supplies the direct read execution freeze.
A failed policy read refuses admission. Missing policy never becomes an empty object.
The resolver serializes `guardrails.Policy.Runtime()` to preserve canonical matching rules and deterministic collection ordering.

Settings retain the saved toolkit wrapper. Arguments retain numbers, null values, arrays, and object shapes without decoding and re-encoding.
The idempotency key includes settings and runtime context. Changed policy cannot return a result admitted under an older policy.
The domain binding and claim manifest grant require the third entry and reject a conflicting entry ID.
No additional migration or protobuf field is needed. The fixed entry appears in the existing input manifest.

After claim authorization, the content materializer redeems saved toolkit settings through the existing credential-reference resolver.
It returns arguments and runtime context byte-for-byte. Neither entry reaches secret expansion.
Redis commands carry manifest references, rather than context contents or credential values.

## Delegated authorization and retry

Main accepts one `mcp_authorization_reference` for the saved toolkit Test request.
The OAuth proxy issues this reference through the encrypted `mcpoauth.Tokens` store.
The store binds the grant to project, actor, saved toolkit ID, and canonical resource.
The admission resolver validates the reference through that exact binding.
The immutable runtime context contains its reference, revision, toolkit ID, and resource. It contains no access or refresh token.

The actor-scoped toolkit reader uses the folder visibility check before settings resolution.
The claim content listener verifies workload identity, execution generation, fence, and lease before materialization.
The materializer derives project and actor from that authorization, validates the immutable grant revision, and loads the transient token.
It replaces `mcp_token_reference` with the resource-keyed `mcp_tokens` response. It never writes that response into the input store.
Expired, revoked, foreign-project, foreign-actor, foreign-toolkit, and foreign-resource grants refuse redemption.

The Rust result reports `AUTHORIZATION_REQUIRED` with a bounded credential-free challenge.
Main validates the metadata allowlist, URL bounds, exact safe message, and saved toolkit identity before projection.
The projector compares the challenge resource with the resource derived from the immutable saved settings.
The settled Test returns HTTP 409 with `reason: authorization_required` and the typed challenge.
This terminal result records an authorization refusal. It does not record a successful tool invocation.

A fresh authorization reference changes the retry idempotency key.
Repeating the same request with the same reference retains the same execution identity.
The client does not submit endpoint-token maps or a random nonce to bypass a completed authorization refusal.

## Model settings and compatibility

`llm_model` preserves the selected bounded model name.
`llm_settings` accepts only `temperature`, `max_tokens`, and `reasoning_effort` with explicit bounds.
Main freezes those nonsecret generation parameters as `llm_configuration`.
Native model-independent tools retain that context without creating an unnecessary model client.
Actual model-dependent adapter behavior requires separate execution evidence.
Inline `llm_configuration` and `mcp_tokens` request fields remain refused.

Old pending two-entry tool commands do not satisfy the new claim manifest contract.
Drain or explicitly retire those commands before switching the producer and worker together.
Existing historical results remain unchanged. This change does not rewrite durable jobs or migration receipts.

## Verification

Focused Main tests cover policy-read failure, cancellation identity, immutable context cloning, credential-field refusal, and policy-sensitive idempotency.
Bundle and domain tests cover the required third role and existing settings and argument separation.
HTTP tests cover model-reference preservation and inline credential refusal.
Materializer tests cover post-claim settings redemption, exact argument and context bytes, large JSON numbers, and unknown-role refusal.
Authorization tests cover typed challenge validation, foreign saved-toolkit rejection, reference-sensitive replay, and claim-time actor/project/revision refusal.
The token-store owner supplies separate real PostgreSQL expiry, revocation, and scope tests.
These tests provide component evidence. They do not prove deployed browser execution or native model-dependent tools.

Five real PostgreSQL toolkit tests pass without skips.
They cover admission, pending settlement, capacity, input-role reconstruction, and authorization identity from immutable saved settings.
The fixture allocates the third content ID. The repository accepts exactly three admitted input entries.
The output binding query still selects the settings and arguments roles. Its two-row check remains intentional.

## Isolated Main integration delivery, 2026-09-11

The commit candidate includes shared Test/discovery admission, policy context, claim materialization, output handling, and inbox recovery.
It excludes unrelated nested-agent context changes.
The isolated candidate passes 208 focused checks across nine Go packages.
Two claim-authorization tests initially skip without PostgreSQL; both pass separately against disposable database fixtures.
PostgreSQL admission, capacity, exact input roles, authorization identity, settlement, and inbox terminal recovery tests also pass.
Main compiles. Go vet passes for Test, discovery, storage, and runtime composition.
The committed Rust library compiles against the updated shared protocol.
These checks do not replace deployed cancellation, replacement, or mixed-guard proofs.

The browser acceptance records use the deployed worktree, including pending REST and UI integration changes.
This Main runtime delivery does not claim that every adapter is committed or that point 3 is closed.
