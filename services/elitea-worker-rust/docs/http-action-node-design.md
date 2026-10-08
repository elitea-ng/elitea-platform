# HTTP action node design (Gate 5d, revision 2)

Status: proposed design. No runtime code. Capability-disabled.

| Item | Value |
| --- | --- |
| Date | 2026-10-08 |
| Wave | Wave 1 design (Track D, deliverable D5). Implementation is Wave 2. |
| Gate | `HTTP_ACTION_INTEGRATION_READY=false` in production builds. Also keep the Web `CompilerAdmittedNodeTypes` entry for `http` off. |
| Sources | Point 5 Wave 1 handoff: `PLAN.md` section 3 "2c" and section 6 answer 1, `experts/05-main-actions.md`, `experts/02-durability.md` section 4. |
| Schemas | `libs/jsonschema/runtime/v1/http-action-*.schema.json` and `fixtures/http-action-*`. |
| Unverified claims | Marked ASSUMPTION. |

Paths are relative to the repository root. `M/` is `services/elitea-main/`. `W/` is `services/elitea-worker-rust/`.
Line numbers were checked on `origin/main` in this worktree on 2026-10-08.

## 1. Summary

A pipeline author writes `type: http`, `revision: 2`. The node names a rule (`operation`) and binds pipeline state to
the parts of the request that may vary. The YAML never contains a URL, a method or a credential.

Main owns everything dangerous: the rule, the credential, the rendering, the network call and the receipt. The Worker
sends only canonical inputs. Main renders one deterministic request, calls the remote system once, and stores a
receipt. A crash anywhere cannot cause a second call to the remote system. If Main cannot prove what happened, the
outcome is `uncertain` and the existing Gate 5b reconciliation takes over.

## 2. Binding decisions

These are not reopened here.

| # | Decision |
| --- | --- |
| 1 | YAML `type: http`, `revision: 2`. Bindings use `{from: <state key>}` or a literal `{value: ...}`. |
| 2 | Main renders from the frozen template plus canonical inputs. The Worker sends `inputs_b64` and `inputs_digest` (invocation v3). |
| 3 | Effects use Lookup, Begin, Commit. A `dispatching` row held by another claim, or past its deadline, becomes `uncertain/reconciliation_required`. It is never dispatched again. |
| 4 | Rules live in a project-scoped table `http_action_rules`. They are authored by project admins with a new permission, inside the operator egress allowlist (user decision of 2026-10-08, overrides expert 05 D4). |
| 5 | Live RBAC on the configuration for the original actor. Live credential revision. Validation against the egress guard at write time and at dial time. |
| 6 | The egress guard moves to `M/internal/infra/egress` (Track M1, separate PR). HTTPS only. `Proxy=nil`. Transport-only constructor. |
| 7 | Dedicated content pool for HTTP actions: 32 slots, at most 8 in flight per execution. |
| 8 | HTTP inside Parallel and Map children uses `scope_ref` (saved-child scope). |

## 3. What exists today (verified)

| Area | Finding | Evidence |
| --- | --- | --- |
| Executor | The revision-1 executor is complete as a library: `Parse`, `Validate`, `Project`, `Service.Execute`. | `M/internal/application/httpaction/contract.go:157`, `:254`, `:425`; `service.go` |
| Wiring | No production implementation of `HTTPActionSource`, `Effects`, `Credentials`, `Artifacts`, the transport or a rule source. `ContentServer` is built without `WithRuntimeHTTPActions`. | `M/internal/infra/storage/runtime_http_action.go:20-62,148`; `M/internal/runtimecomposition/composition.go:1806-1832` |
| Route | `POST …/runtime-context/http-actions` exists but is registered only when the service is set. | `M/internal/infra/storage/content_server.go:403-405` |
| Invocation v2 | Carries `request_wire_b64`, `request_digest`, `binding_digest`. `schema_version` is `elitea.runtime.http-action.v2`. | `contract.go:21,32-42` |
| Receipt v2 | Eight fixed fields. States `completed`, `failed`, `uncertain`. Schema `elitea.runtime.http-action-receipt.v2`. | `contract.go:22,92-101`, `DecodeReceipt` |
| Snapshot v1 | The whole request is frozen from YAML and `revision` must be `1`. `Verify` demands byte equality. | `M/internal/application/httpaction/frozen.go:20,39,81,142` |
| Rules | One rule per `(project, actor, origin, path, method, configuration)`. `CredentialRevision` is static. Only the root thread is admitted. Sensitive operations fail closed. | `runtime_http_action.go:20-31,102,121,133` |
| Effects table | `execution_http_effects` (0145), frozen digests and receipt wire (0151), saved-child columns (0152). Primary key `(execution_id, generation, activation_id)`, unique `effect_id`. | `M/migrations/shared/0145_execution_http_effects.sql`, `0151_http_frozen_request_receipts.sql`, `0152_execution_saved_child_scopes.sql` |
| Recovery owner | Composed. Reads a committed receipt under `FOR SHARE` and builds an owner proof. | `composition.go:519`; `M/internal/infra/db/repos/http_action_recovery.go:31-90` |
| Results route | `POST …/node-recovery/results/{contentID}/versions/{version}`. | `content_server.go:386` |
| Egress guard | `webhook.DestinationGuard` re-resolves DNS and dials the IP literal. It clones `http.DefaultTransport`, so it inherits `Proxy: ProxyFromEnvironment`. It does not block CGNAT, `0/8`, `192.0.0/24`, `198.18/15`, `240/4` or NAT64. | `M/internal/api/webhook/ssrf.go:183,316-336`; `libs/go/egresslib/allowlist.go:119-125` |
| Transport port | `Transport` is `Do(*http.Request)`. An `*http.Client` satisfies it and follows redirects. | `service.go` (`Transport` interface) |
| Content pool | One shared pool of 16 slots. `PostHTTPAction` holds a slot for the whole call (up to 30 s). Attachments already have their own pool. | `content_server.go:25,126-130,1097-1115` |
| Worker | `http_action.rs` is an error vocabulary only. No node, no client, no gate constant. | `W/src/agents/graph/http_action.rs` (46 lines) |
| Saved child | `SavedChildPurpose::NestedHttp` and `SavedChildScopeRef {scope_id, revision, digest_sha256}` exist. | `W/src/agents/pipeline/saved_child_scope_provider.rs:11-17`; `saved_child_http.rs:7-16` |
| Encoding | Worker `encode_component` percent-encodes everything except `A-Z a-z 0-9 - . _ ~`. Hex is uppercase. Space is `%20`. | `W/src/toolkits/families/openapi/client.rs:50-54,797-806` |
| Node recovery | The owner-proof domain type and the results route exist on main. The contract file is not on main. | `M/internal/domain/noderecovery/owner_proof.go:7`; `libs/proto/contracts/node-recovery-v1.md` (lands with #1084) |

Code facts that differ from the plan text:

- The effects migration number `0154` is taken (`0154_agent_stop_question_author.sql`). This document names no number. Use
  the next free shared migration number at implementation time (0154 is taken; check main and #1084).
- The guard does not set `Proxy` explicitly. It inherits it from `DefaultTransport.Clone()`. The effect is the same as
  expert 05 F5: if an operator sets `HTTPS_PROXY`, the proxy address is dialled, not the target.
- The Worker gate constant `HTTP_ACTION_INTEGRATION_READY` does not exist yet. Wave 2 adds it.
- The Go invocation `schema_version` string is `elitea.runtime.http-action.v2`. The schema `$id` for the new shape is
  `...invocation.v3`, so the in-body constant for v3 is a proposal (`elitea.runtime.http-action.v3`).

## 4. YAML, revision 2

```yaml
- id: create_ticket
  type: http
  revision: 2
  operation: tracker.create_item        # names a Main rule
  request:
    path:    {project: {from: project_key}, item: {from: item_key}}
    query:   {notify: {value: "false"}, page: {from: cursor, optional: true}}
    headers: [{name: x-trace-id, from: trace}]
    body:    {kind: json, from: payload}  # json | text | artifact | empty
    idempotency_key: {from: request_id}
  response: {mode: json, accepted_statuses: [{first: 200, last: 299}], max_bytes: 1048576}
  timeout_ms: 15000
  output: [ticket]                       # receives {status, content_type, byte_length, data}
  transition: next
```

Schema: `elitea.pipeline.http-action-node.v2` (`libs/jsonschema/runtime/v1/http-action-node.schema.json`).

| Part | Rule |
| --- | --- |
| `operation` | Rule name, `^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`. Resolved in the execution's project. |
| `request.path` | One entry per `{placeholder}` of the rule's path template. No more, no fewer. |
| `request.query` | Names must be in the rule's `query_names`. Values are string, integer or boolean. |
| `request.headers` | List of `{name, from}` or `{name, value}`. The existing `RestrictedHeader` set still applies (`contract.go`, `RestrictedHeader`). |
| `request.body` | `empty`, `json` or `text` with `from` or a literal `value`. `artifact` takes `from` only and needs `content_type`. GET, HEAD and OPTIONS must be `empty`. |
| `request.idempotency_key` | Binding. Characters `A-Za-z0-9._:-`, at most 128. |
| `response` | Same contract as revision 1: `mode`, `accepted_statuses`, `max_bytes`. The extra rules in `Request.Validate` (no 3xx, 401, 403 in the accepted set) are still enforced by Main. |
| `output` | Exactly one state key. It receives the projection from `Project`. |
| `from` | A state key. ASSUMPTION: plain identifiers `^[A-Za-z_][A-Za-z0-9_]{0,127}$`. Check against the compiler's state-key rules (`W/src/agents/graph/compiler.rs`, `reserved_user_state_key`). |
| `optional: true` | Only on `from` bindings. The Worker omits the input when the state value is missing or null, and Main omits the part. |

Not allowed in YAML (the schema has `additionalProperties: false`, so each is rejected by shape): a URL, a method, a
credential or configuration id, any templated URL string.

Bindings are deliberately simpler than the LLM node `input_mapping` (`type: fixed|variable|fstring`). There is no
string interpolation.

### Revision 1 stays readable

- `FreezeSnapshot` keeps accepting `revision: 1` unchanged. It produces snapshot `elitea.runtime.http-action-snapshot.v1`.
- Revision 2 produces a new snapshot schema `elitea.runtime.http-action-snapshot.v2`. Each node entry keeps `node_id`,
  `output`, `transition` and the binding digest, and adds `operation`, `revision` and the template (the `request`,
  `response` and `timeout_ms` objects as canonical JSON). `Verify` picks the branch by `schema_version`.
- The route accepts invocation v2 for revision-1 nodes and v3 for revision-2 nodes, selected by `schema_version`.
- Nothing runs revision 1 in production today (the executor is unwired), so no data migration is needed. New authoring
  and the Web editor emit revision 2 only. ASSUMPTION: revision-1 nodes, if ever admitted, match a rule by exact
  `(origin, path, method, configuration)` instead of by actor.
- Related Wave 1 fix (expert 05 F1, not part of this track): freeze only pipelines that contain an HTTP node
  (`M/internal/application/agentexecution/start.go:413`, `http_action_snapshot.go:9`).

## 5. Rules

Table `elitea_runtime.http_action_rules`, one row per rule, scoped by project. Body schema:
`elitea.runtime.http-action-rule.v1`. The project id comes from the route, not from the body.

| Field | Meaning |
| --- | --- |
| `name` | Unique per project. Referenced by `operation`. |
| `origin` | `https://host[:port]`. No path, no userinfo. |
| `path_template` | `/`-separated. A placeholder `{name}` must fill a whole segment. Literal segments use `A-Za-z0-9._~-`. `.` and `..` are refused. |
| `method` | One of `GET HEAD OPTIONS POST PUT PATCH DELETE`. |
| `query_names` | Allowed query parameter names, at most 32. |
| `configuration_id` | Credential source. `0` means no credential. |
| `toolkit`, `tool` | Names used by the platform guardrail policy (blocked, sensitive). |
| `output_bucket` | Needed only for `mode: artifact`. |
| `enabled` | A disabled rule denies. |

Authority:

- Authors are project admins with a new permission. Proposed name: `models.http_action_rules.manage`.
  **This name is a proposal for Wave 2.** The final name must follow the existing permission naming (`models.*`,
  `configurations.*`). Listing rule names for the Web editor needs a list permission (proposal
  `models.http_action_rules.list`).
- The rule must stay inside the operator egress allowlist (`ELITEA_HTTP_ACTION_EGRESS_ALLOWLIST`, the outer bound for
  private networks).
- The admin API validates the rule against the egress guard when it is written. The dialer validates again at dial
  time. A rule that was fine yesterday can be refused today.
- The `ActorID` match (`runtime_http_action.go:121`) is removed. Instead Main checks, live, that the original actor
  (the user who started the execution) may use `configuration_id`. ASSUMPTION: this reuses the existing configuration
  permissions (`configurations.*`). Verify the exact check in Wave 2.
- `CredentialRevision` is read live from the configuration row at admission. It is no longer stored in the rule.
- The Web editor lists rule names only. It never shows origin, credential or path.
- The `policy_digest` pinned into the effect is computed over the rule as read at admission.

## 6. Rendering (normative)

Main renders. Rust only needs the same functions to build `inputs` and to check its own fixtures. Both languages must
pass `fixtures/http-action-render-vectors-v1.json`.

Inputs are canonical JSON: keys sorted, no whitespace, UTF-8, strings escaped only for `"`, `\` and control
characters below U+0020 (`\b \t \n \f \r` short forms, others `\u00xx` lowercase hex).

```json
{"body": "<json | string | artifact>", "headers": {"x-trace-id": "..."}, "idempotency_key": "...",
 "path": {"project": "..."}, "query": {"page": 2}}
```

Only bindings with `from` have an entry. Literals stay in the frozen template. Main rejects missing, extra or
non-canonical inputs (`invalid_input`).

Steps:

1. Find the rule by `operation` in the execution's project. Missing, disabled or not matching: `policy_denied`.
2. Path: for each `{name}` segment, take the scalar (string, integer, boolean), convert it to text, refuse empty, `.`,
   `..` and more than 1024 bytes, then percent-encode with the unreserved set (the same set as Worker
   `encode_component`). `/` becomes `%2F`, space becomes `%20`, UTF-8 bytes become `%XX` with uppercase hex.
3. Query: each name must be in `query_names` (else `policy_denied`). Refuse floats, null, arrays and objects. Sort pairs
   by raw name, bytewise. Encode name and value with the same set. Join with `&`. Empty means no `?`.
4. Headers: lowercase the name, refuse restricted and duplicate names, refuse `\r`, `\n` and NUL. Sort by name. Values are
   not encoded.
5. Body: `json` is the canonical bytes of the value as received. `text` is the UTF-8 bytes. `empty` is zero bytes.
   `artifact` is resolved from the stored reference as today. GET, HEAD and OPTIONS with a body: `invalid_input`.
6. URL is `origin + path + ("?" + query)`. Then run the unchanged `Request.Validate` and the rest of the service.
7. `request_digest` is the SHA-256 of the canonical request wire: sorted keys, only present fields (`credential` only when
   `configuration_id > 0`, `headers` only when not empty, `body` only when not empty, `idempotency_key` only when set),
   `response` and `timeout_ms` copied from the template.

Pitfalls the vectors pin down:

- Go `encoding/json` escapes U+2028 and, before Go 1.22, `\b` and `\f` differently. The canonical encoder must follow the
  rule above, so Go needs a small custom writer.
- Go `Request.Body` is a struct, so `omitempty` does not omit it. Build the wire from an ordered map, not from the struct.
- Numbers in a JSON body keep their text from the inputs. Do not round-trip through floats.

Reused code: only the encoding sets of `serialize_query` and `encode_component` (`client.rs:702-819`). Credentials and
receipts never go through the Worker OpenAPI client.

## 7. Invocation v3 and the flow

Schema `elitea.runtime.http-action-invocation.v3`. Fields: `schema_version`, `node_id`, `thread_id`, `step`,
`activation_id`, `binding_digest`, `inputs_b64` (at most 256 KiB decoded), `inputs_digest` (SHA-256, lowercase hex),
optional `scope_ref {scope_id, revision: 1, digest_sha256}`. The `schema_version` value `elitea.runtime.http-action.v3`
is a proposal. `request_wire_b64` and `request_digest` are gone: the template digest is already inside
`binding_digest` (`FrozenBindingDigest`, `frozen.go`), and Main computes the rendered `request_digest` itself.

Limits: the whole body stays within `MaxInvocation` (768 KiB). Base64 of 256 KiB is 349,528 characters.

Flow:

1. Main freezes the snapshot at start (template and rule name, never a rule body).
2. The Worker resolves bindings from checkpointed state into canonical inputs. Replay must use only checkpointed state.
3. The Worker computes `activation_id = VisitID(thread, node, step, binding)` and writes the journal entry
   `unknown_external_effect{effect_id}` first.
4. The Worker posts the invocation. Main admits (claim, RBAC, rule, scope), renders, then Lookup, Begin, dispatch, Commit.
5. The Worker projects the receipt into `output` and checkpoints.

Retry rules in the Worker:

| Response | Action |
| --- | --- |
| 200 receipt `completed` or `failed` | Use it. Never call again. |
| 200 receipt `uncertain` | Stop the node with 5b `effect_reconciliation_required`. |
| 503 or transport error | Retry the same invocation. Safe because Lookup returns the existing receipt if an effect exists. |
| 409 | Inputs differ from the stored `inputs_digest` for this activation. Non-deterministic bindings. Fail the node. |
| 403, 422 | Fail the node (policy or invalid input). |

`effect_id` v3 hashes `(execution, generation, activation_id, binding_digest, inputs_digest)` with the label
`elitea.http.effect.v3`. The v2 hash used `request_digest` (`contract.go:230`).

## 8. Effects

New repository `M/internal/infra/db/repos/http_action_effects.go`, implementing the existing `Effects` port
(`service.go`).

| Call | Behaviour |
| --- | --- |
| Lookup | Return the stored receipt if the row is terminal. If the row is `dispatching` and the claim differs or the deadline passed, set it to `uncertain/reconciliation_required` and return that. Runs before artifact or credential redemption. |
| Begin | `INSERT ... ON CONFLICT DO NOTHING` on `(execution_id, generation, activation_id)`. If it inserted: return `dispatch=true`. If it conflicted: compare `request_digest` and `inputs_digest`. A mismatch is a 409. A match returns the existing receipt (or the `uncertain` conversion above). Never `dispatch=true` twice. |
| Commit | Update from `dispatching` to a terminal state with the original `dispatch_claim_id` and lease epoch. A different claim cannot overwrite. |

Migration (next free shared number at implementation time; 0154 is taken, check main and #1084): add `operation`
(rule name), `inputs_digest` (64 hex) and `inputs_bytes` (bytea, at most 256 KiB) to `execution_http_effects`.
Existing rows are revision 1 and keep these columns null, so the checks must allow null for them. The saved-child
columns from 0152 are reused for `scope_ref`.

Why `inputs_bytes`: the recovery owner re-renders from stored bytes and compares with `request_digest`, so a recovered
result is proven to belong to this request.

## 9. Recovery

- A `completed` receipt is redeemed through the existing owner proof (`http_action_recovery.go:31-56`, route
  `content_server.go:386`). For revision 2, `committedHTTPRecoveryProof` re-renders from `inputs_bytes` in the same PR
  that adds rendering. Otherwise the proof would verify a template shape that no longer fits.
- `failed` is final and needs no proof.
- `uncertain`, including a `dispatching` row whose claim is gone, goes to 5b as `effect_reconciliation_required`. The
  owner never answers `verified_no_effect` for HTTP: a missing or ambiguous record is never a no-effect proof
  (`http_action_recovery.go:30-31`). Only an operator can resolve it.
- Inside a fan-out, an `uncertain` effect stops that child. The operator path needs the per-thread pending visit
  change (expert 05 T9, F8).

## 10. Egress

Target layout (Track M1, separate PR). Until it merges, this is the requirement, not the code.

| Item | Requirement |
| --- | --- |
| Location | `M/internal/infra/egress`. `webhook` keeps an alias. |
| Blocked classes | CGNAT `100.64.0.0/10` (includes `100.100.100.200`), `0.0.0.0/8`, `192.0.0.0/24`, `198.18.0.0/15`, `240.0.0.0/4`, NAT64 `64:ff9b::/96`, IPv4-mapped forms of all of these, plus the existing loopback, private, link-local, multicast and unspecified classes. |
| Proxy | `Proxy = nil`. |
| Headers | `MaxResponseHeaderBytes = 64 KiB`. |
| Constructor | Accepts only `*http.Transport`, so a redirect-following `http.Client` cannot be wired. A redirect response stays a `redirect_refused` failure (`Project`, 3xx). |
| Scheme and TLS | HTTPS only. TLS verified against the URL host. |
| DNS | Resolve, filter, then dial the IP literal (existing `dialContext` behaviour). |
| Allowlist | The operator allowlist is the outer bound for private networks. Link-local, multicast and unspecified are never allowed. |

## 11. Capacity

Dedicated pool in `ContentServer`, modelled on `attachmentRequests` (`content_server.go:126-130`).

- 32 slots for HTTP actions. At most 8 in flight per execution. The 9th request gets 503 with `Retry-After`, which the
  Worker retries.
- The shared 16-slot pool is not used. Token, version and artifact reads are protected.
- Budget: 16 concurrent HTTP actions add at most 50 ms p95 to token reads.
- ASSUMPTION: the per-execution counter is an in-process map keyed by execution id. If Main runs more than one replica
  the limit is per replica. Verify whether a cluster-wide limit is needed.

## 12. Fan-out

- Root thread: no `scope_ref`. Main checks `inv.ThreadID` against `RootGraphThread` as today (`runtime_http_action.go:102`).
- Inside Parallel or Map children (non-root graph thread): the Worker sends `scope_ref` obtained from
  `SavedChildScopeProvider::for_visit(thread, node, SavedChildPurpose::NestedHttp)`. Main checks that the row in
  `execution_saved_child_scopes` (0152) is `active`, matches `(scope_id, revision, digest)`, and that the thread belongs
  to it.
- `activation_id` already includes the child thread, so `effect_id` is child-scoped.
- An HTTP node on an unknown thread without `scope_ref` is refused. It never falls back to root authority.
- Gate: nothing runs until `HTTP_ACTION_INTEGRATION_READY` and the fan-out gates are flipped (PLAN section 3 2e: HTTP
  flips last).

## 13. Crash cases

| # | Crash or fault | Expected result | Test |
| --- | --- | --- | --- |
| 1 | Worker killed after Main committed the receipt, before the Worker checkpoint | Reclaim replays, Lookup returns the receipt. 0 extra requests to the remote server. | Counting test server, process loss |
| 2 | Worker killed after the journal write, before posting | Reclaim posts. Begin inserts. 1 request. | Process loss |
| 3 | Main killed between Begin and Commit | Row stays `dispatching`. The next Lookup (new claim, or deadline passed) turns it `uncertain/reconciliation_required`. Never re-dispatched. | Real PostgreSQL |
| 4 | Row `dispatching` held by a live claim, second request arrives | The second request gets `uncertain` or the first outcome. It never dispatches. | Real PostgreSQL |
| 5 | 32 concurrent Begin calls on one activation | Exactly one `dispatching` row and one `dispatch=true`. | Real PostgreSQL |
| 6 | Main returns 503 before any effect row exists | Worker retries. Lookup finds nothing. Begin proceeds. 1 request. | Unit |
| 7 | Main returns 503 after the row exists | Worker retries. Lookup returns the row state. No second dispatch. | Unit |
| 8 | Replay produces different inputs for the same activation | Begin sees a different `inputs_digest`: 409. Node fails. 0 extra requests. | Replay test |
| 9 | Remote closes the connection mid-response | Receipt `uncertain/reconciliation_required`. 5b stops the node. | Service test |
| 10 | Lease lost during dispatch | Commit fails (original dispatch token). Row stays `dispatching` and becomes `uncertain` on the next Lookup. | Real PostgreSQL |
| 11 | Remote returns 3xx | `failed/redirect_refused`. No follow. A following client cannot be constructed. | Constructor test |
| 12 | DNS rebinding to a blocked IP between write and dial | Dial refused. Failed, no request sent. | Guard test |
| 13 | Rule edited or disabled between freeze and call | The live read decides. Disabled: `policy_denied`. | Unit |
| 14 | Credential revision changed after the rule was written | The live revision is used. The old revision is never sent. | Unit |
| 15 | Actor loses access to the configuration mid-run | The next admission refuses (403). | Unit |
| 16 | Recovery of a `completed` receipt after the Worker lost its checkpoint | The owner proof re-renders from `inputs_bytes` and returns the receipt wire. | Real PostgreSQL |
| 17 | Two children in one Map call the same node with different items | Different `activation_id`, two effects, no collision. | Fan-out test |
| 18 | Uncertain effect in a fan-out child | That child stops. Siblings keep running. Operator path through 5b. | Fan-out test |

Effect safety (expert 02 section 4): HTTP stays closed as a retryable 5b node and as a fan-out worker until this
executor is wired. ASSUMPTION: an approved sensitive tool that crashes between dispatch and its tool-result checkpoint
is re-dispatched in the older tool path. This design avoids that for HTTP because Begin is durable before the call.

## 14. Wave 2 tasks

Numbering follows expert 05 section 3.

| Task | Work | Anchors | Depends |
| --- | --- | --- | --- |
| T4 | Effects repo, migration (next free number), real-PostgreSQL tests | `M/internal/infra/db/repos/http_action_effects.go`, `M/migrations/shared/` | this doc |
| T5 | Rules table, admin API with the new permission, live RBAC and credential revision, Credentials adapter | `repos/http_action_rules.go`, `storage/runtime_http_action.go:20-31,121`, new API package | T4 |
| T6 | Revision 2 freeze, render, snapshot v2, shared render fixture, recovery proof re-render | `httpaction/frozen.go:39,81,142`, new `render.go`, `http_action_recovery.go:31-90` | this doc |
| T3 | Egress guard in `infra/egress` (Track M1) | `M/internal/infra/egress/*`, `M/internal/api/webhook/ssrf.go:183,316` | none |
| T7 | `HTTPActionSource`, dedicated pool, composition wiring | `content_server.go:25,126,403`, `composition.go:1806-1832` | T3 to T6 |
| T8 | Worker `http` node, runtime-context client, journal integration, gate constant | `W/src/agents/graph/http_action.rs`, `compiler.rs`, `W/src/transport/` | T6 |
| T9 | 5b admits the http family; per-thread pending visit | node-recovery definition, migration 0146 uniqueness (both land with #1084) | T8, #1084 merged |

T7 and T8 touch files that #1084 changes (`postgres_content.go`, the journal). Schedule them after #1084 merges.

## 15. Acceptance and budgets

Unit and fixture:

- Render determinism: both languages pass `http-action-render-vectors-v1.json` (25 vectors: 15 expect a request, 10 expect an error code).
- Every restricted header is refused. Path and query injection (`../`, `%2F`, CRLF) is refused or encoded as specified.
- A redirect-following client cannot be constructed. Each new IP class is blocked, including IPv4-mapped and NAT64 forms.
- Schemas: every `fixtures/http-action-*` file passes or fails as its name says.

Real PostgreSQL: crash cases 3, 4, 5, 10 and 16.

Process loss: crash case 1 with a counting server proves 0 extra requests.

Budgets:

| Budget | Limit |
| --- | --- |
| Main overhead per HTTP action, excluding the remote call | at most 15 ms p95 |
| 16 concurrent HTTP actions, effect on token reads | at most 50 ms p95 added |
| Inputs | at most 256 KiB |
| Rendered request | at most 384 KiB (`MaxRequest`) |
| Response | at most 2 MiB; inline at most 512 KiB (`MaxResponse`, `MaxInline`) |
| Timeout | at most 30 s per call |

Browser confirmation: deferred to the gate flip. Until then, browser regression of unchanged flows only.

## 16. Schema index

| Schema `$id` | File | Purpose |
| --- | --- | --- |
| `elitea.pipeline.http-action-node.v2` | `http-action-node.schema.json` | YAML node as JSON |
| `elitea.runtime.http-action-invocation.v3` | `http-action-invocation.schema.json` | Worker to Main |
| `elitea.runtime.http-action-rule.v1` | `http-action-rule.schema.json` | Admin-authored rule |
| `elitea.runtime.http-action-receipt.v2` | `http-action-receipt.schema.json` | Mirror of the existing Go receipt |
| `elitea.runtime.http-action-render-vectors.v1` | `http-action-render-vectors.schema.json` | Go/Rust render fixture shape |

## 17. Rejected alternatives

- Worker renders the full request and Main verifies it: two renderers can drift.
- String or Jinja URL templating in YAML: injection risk.
- JSONPath projection inside the node: Gate 5c data shaping already does it.
- Worker-side egress through the OpenAPI client: credentials and receipts must stay in Main.
- Platform-admin-only rules (expert 05 D4): replaced by the project-admin permission.
- Helm static rule config: a redeploy for every change.
- A generic `execution_effects` table now: owner-specific ledgers stay behind `NodeRecoveryEffectProofProvider`.
- Rule matching by `ActorID`: per-user rules do not scale, and live RBAC is stronger.
- Rule placeholders inside a segment (`/v{ver}`): values could add path structure. Whole-segment only.
