# Sandbox whole-Code recovery v1

Status: private contract amendment. No production adoption is claimed.

## Ownership

Main records original execution admission before it signs a sandbox execute grant.
The original Supervisor owns execution binding, dispatch, terminal results and no-effect sealing.
Main never reads or changes Supervisor tables or Worker checkpoints.
Neither Main nor Worker calls Submit to inspect a recovery result.
A completed platform call does not prove that the whole Code node completed.

## Original identities

Node recovery `effect_id` is the original 32-byte sandbox dispatch activation, encoded as lowercase hexadecimal.
It is not the Supervisor job key or the journal visit activation.
The Supervisor job key preserves the existing frame:
SHA256(`elitea.sandbox.activation.v1\0` + len(execution_id)u64be + execution_id + len(dispatch_activation_hex)u64be + dispatch_activation_hex).
The original generation remains fixed. A replacement claim changes the writer fence, not these identities.
Main retains the exact original request digest and configured Supervisor audience before issuing an execute grant.
Plain execution uses PreparedJob.fingerprint. Compiled execution uses the original Execute intent digest.
Preparation, compile, content and cancellation grants cannot prove whole-Code execution results.

## Two-stage original admission

POST `/executions/{execution_id}/generations/{generation}/code-sandbox/visits` on Main's existing claim-fenced mTLS content listener.
The strict request schema is `elitea.sandbox.original-code-visit-request.v1`.
Fields are activation_id, node_id, graph_thread, step, attempt, node_digest, owning_yaml_sha256,
exact_configuration_json_base64url, pre_workspace_prepared_job_json_base64url, and required nullable saved_child_scope.
The exact configuration comes from the compiler's unconditional original declaration getter.
Main independently validates the saved declaration. It does not normalize another Code configuration.
Admit the original visit before debug, workspace, dependency preparation or compilation.
The response schema is `elitea.sandbox.original-code-visit-response.v1` with original_visit,
execution_id, original_generation, activation_id, attempt, node_digest and pre_workspace_prepared_sha256.
original_visit has exactly visit_id, revision=1, and digest_sha256.
visit_id hashes `elitea.sandbox.original-code-visit.v1\0`, a u64be byte length, and compact ordered JSON
of [execution_id,generation,activation_id,attempt]. It is nonzero lowercase64hex.
The digest hashes the canonical server-owned visit record. This reference grants no execution or recovery authority.

POST the same prefix `/code-sandbox/intents` to freeze the final exact execution binding once.
The strict schema is `elitea.sandbox.original-code-intent-request.v1`.
Fields are original_visit, dispatch_activation, request_digest, supervisor_audience,
prepared_job_json_base64url, required nullable compiled_binding_json_base64url and selected_descriptor_sha256.
Final preparation may add only validated dependency, workspace or broker extensions.
It must preserve the original saved declaration, resolved source and selected input.
Compiled admission uses the owning profile and original indexed descriptor before computing the Execute intent digest.
It cannot substitute the plain PreparedJob fingerprint for a compiled request.

The final response has schema `elitea.sandbox.original-code-intent-response.v1` and intent.
intent is a signed envelope with schema `elitea.sandbox.original-code-intent-signed.v1`,
key_id, claims_base64url and signature_base64url. It uses Ed25519 domain
`elitea.sandbox.original-code-intent.ed25519.v1\0` + u64be claims length + exact claims bytes.
Claims schema is `elitea.sandbox.original-code-intent.v1`, purpose whole_code_execute.
Exact claims fields are schema,purpose,tenant_id,project_id,execution_id,original_generation,
claim_id,claim_attempt,lease_epoch,fence_sha256,activation_id,node_id,graph_thread,step,attempt,node_digest,
dispatch_activation,job_key,request_digest,supervisor_audience,submitter_workload_identity,language,
prepared_job_sha256,source_sha256,input_sha256,issued_at_unix_millis,expires_at_unix_millis.
Claims are bounded to8KiB and30seconds; signature is64bytes; base64url has no padding.
Supervisor checks the independent intent against its actual Execute grant and admitted PreparedJob.
The new Submit plain field18 and compiled field10 carry this envelope only during Execute admission.
Compile, preparation and receipt-only observation cannot register it. Historical empty intent grants no recovery audience.

## Owner routes

Use POST `/elitea.runtime.node-code-recovery.v1/jobs/{job_key}/read` to read original committed evidence.
Use POST `/elitea.runtime.node-code-recovery.v1/jobs/{job_key}/seal-no-effect` for an explicit authorized reconciliation.
Use the original Supervisor mTLS listener and configured audience. Reject redirects and query parameters.
Both routes accept strict JSON of at most16KiB. They never provision, hydrate, compile, dispatch or submit code.
Main presents its verified certificate identity. The certificate must match the signed requester identity.

Request fields are exactly `schema` and `grant`.
The request schema is `elitea.sandbox.node-code-recovery-request.v1`.
The grant fields are exactly `schema`, `key_id`, `claims_base64url`, and `signature_base64url`.
The grant schema is `elitea.sandbox.node-code-recovery-signed-grant.v1`.
Base64url has no padding. Decode at most8KiB claims and exactly64 signature bytes.
Sign `elitea.sandbox.node-code-recovery-grant.ed25519.v1\0`, then claims length as u64be, then the exact claims bytes.
Verify the Ed25519 signature before decoding claims. Reject duplicate fields and unknown fields.
This separate domain cannot authorize ordinary sandbox work, cancellation, content or checkpoint access.

## Signed claims

The claim schema is `elitea.sandbox.node-code-recovery-grant.v1`.
The exact fields are:

- `schema`, `tenant_id`, `project_id`, `execution_id`, `original_generation`.
- `claim_id`, `claim_attempt`, `lease_epoch`, `fence_sha256`.
- `activation_id`, `node_id`, `graph_thread`, `step`, `attempt`, `expected_revision`, `receipt_sha256`.
- `dispatch_activation`, `job_key`, `request_digest`, `binding_sha256`, `supervisor_audience`, `requester_workload_identity`.
- `operation`, `issued_at_unix_millis`, `expires_at_unix_millis`.

`operation` is `read` or `seal_no_effect`. It must match the route.
Main derives every field from original admission and the exact latest stored node receipt under its current claim.
Browser selectors cannot supply grants, job identities, effects, digests, proof objects or content paths.
binding_sha256 hashes canonical WholeCodeBinding bytes and binds every original metadata field.
`execution_id` is exactly32lowercasehex. Other digest and activation fields are nonzero64lowercasehex.
`project_id`, generation, claim attempt and lease epoch are positive. All numeric fields fit signed64bits.
Node attempt is1..16. Journal revision is positive. Graph step may be zero.
Use existing node ID and graph thread bounds. Reject controls and U+2028/U+2029 in graph threads.
Certificate identities are nonempty and at most256bytes. Reject controls and whitespace.
The lifetime is1..30000ms. Reject future issuance and expired grants.
Main checks cancellation and the latest action before issuing a grant and again before admitting the returned proof.

## Original execution binding

The Supervisor records `elitea.sandbox.whole-code-binding.v1` before any execution dispatch boundary.
It records only an actual PreparedJob admitted through an execution endpoint.
The exact binding fields are:
`schema`, `purpose`, `execution_id`, `original_generation`, `dispatch_activation`, `job_key`, `request_digest`,
`supervisor_audience`, `node_digest`, `activation_id`, `node_id`, `graph_thread`, `step`, `attempt`, `language`, `prepared_job_sha256`, `source_sha256`, `input_sha256`.
`purpose` is exactly `whole_code_execute`. Language is python, javascript, typescript or rust.
Hash source as exact UTF8. Hash input as compact recursively ordered JSON of the original PreparedJob input object.
Hash prepared_job_sha256 over canonical admitted PreparedJob::to_transport bytes.
Main requires the exact canonical Worker-emitted wire and refuses noncanonical alternatives.
Bind metadata immutably to the existing tenant/project/job/request row. Reject changed metadata.
Do not infer metadata from a checkpoint, result object, a missing row or a platform-call receipt.
Old rows without this metadata cannot issue new recovery evidence.

## Response and receipt

The response fields are exactly `schema`, `state`, and required nullable `receipt`.
The response schema is `elitea.sandbox.node-code-recovery-response.v1`.
State is completed, verified_no_effect, pending, failed, cancelled, uncertain, missing or conflict.
Only completed and verified_no_effect have receipts. Other states have null and issue no authority.
The receipt schema is `elitea.sandbox.whole-code-recovery-receipt.v1`.
Common fields are `schema`, `kind`, `binding`, and `visit`.
Visit fields are `activation_id`, `node_id`, `graph_thread`, `step`, `attempt`, `expected_revision`, and `receipt_sha256`.
Visit comes only from the authenticated Main grant. It is checked against the original owner binding.
A committed_result receipt also contains `result_json_base64url` and `result_sha256`.
These fields contain the exact original completed Code result bytes and their SHA256.
Decode at most512KiB. Never replace a failed, cancelled or partial result with a successful result.
A verified_no_effect receipt instead contains `seal_id`.
The seal ID is SHA256(`elitea.sandbox.whole-code-no-effect-seal.v1\0` + u64be byte length
+ compact recursively ordered JSON array [binding,visit]). It is nonzero64lowercasehex.
Persist the exact compact recursively ordered receipt bytes. Identical reads return identical bytes.
Do not include claim IDs, tokens, timestamps or new runtime identities in immutable receipt bytes.

## Result audience and projection

Register `elitea.runtime.code-sandbox-whole-result.v1` separately from HTTP and platform-call audiences.
Use the existing NodeRecoveryOwnerProof committed_result contract.
Committed result uses result_ref and no owner_receipt_ref. Verified no-effect uses owner_receipt_ref and no result_ref.
Both references use the registered Code audience and exact immutable owner receipt digest, length and identifiers.
ResultRef content_id is the original dispatch activation. Version and digest are the exact owner receipt SHA256.
Media type is application/json. Receipt size is positive and at most1MiB.
Main proxies exact stored receipt bytes over its existing current-claim result route.
Worker checks the current visit, attempt, original effect, source hash and selected input hash.
Worker projects only the original result through the admitted Code output contract.
This projector has no runtime, client, credentials, grants or code execution capability.

## No-effect seal and races

Seal only an existing whole-Code execution row in reserved phase with dispatched_at NULL.
Reject cancellation_requested, ordinary failed, cancelled, dispatched, uncertain and missing rows.
Lock the row, compare the exact request and immutable binding, then commit a terminal no-effect tombstone.
The tombstone sets phase failed, failure_code recovery_verified_no_effect, and the immutable receipt bytes.
Increment the lease epoch and expire the old owner lease in the same transaction.
Every future dispatch path requires reserved phase and its original live epoch. The tombstone defeats those paths.
A dispatch-first race refuses no-effect. A seal-first race rejects dispatch. Never issue both authorities.
Reading an existing exact tombstone returns the same proof. It never clears or recreates the row.
A cancelled tombstone cannot issue no-effect evidence.

Worker commits verified evidence under its current writer and preserves the original failed attempt history.
The journal emits version2 only after a NoEffectReconciled event; version1 records remain readable and unchanged.
The event binds the exact original failure attempt, effect, request, revision and immutable owner receipt hash.
Reconciliation alone never dispatches another attempt. Eligible retry requires a separate authorized operator action.
Elapsed or exhausted budgets remain consumed. Denial and cancellation remain terminal.
Main ACK keeps eligible continuation suspended. Terminal outcome requires separate sealed failure settlement authority.
An admitted typed failure route needs explicit restore authorization for that route; it cannot become fresh input authority.
ACK failure_route_continuation is exclusive with continuation_receipt and terminal_stop_reason.
Its schema is elitea.pipeline.node-recovery-failure-route.v1 with route_id and failed
{activation_id,attempt,failure_class,stop_reason}. Main checks the immutable saved dedicated handler.
Checkpoint restore emits the original typed failure descriptor and goto, with zero failed Code execution.

## Required checks

Test exact original result reuse with zero execution calls.
Test changed source, input, request, effect, visit, ordinal, receipt version and audience rejection.
Test old rows, missing rows, preparation results, partial platform receipts and cancellation refusal.
Test actual owner dispatch-versus-seal races and exact replay after replacement.
Distinguish authored component checks from PostgreSQL and deployed proof.

WholeCodeBinding also retains exact original activation_id,node_id,graph_thread,step,attempt.
Read/seal must match these original signed visit fields independently of Main.
Supervisor code_owner_requester is a default-absent configured exact Main mTLS
identity; absent denies owner read/seal/platform routes. Preparation forbids it.

## Compile workspace access

The authoritative `compiled_code.proto` extends only Compile request11 and
signed revision4 claims42. `OriginalCodeVisitRefV1` contains visit_id, revision1
and digest_sha256; `OriginalCodeVisitAccessV1` contains that reference, current
claim_id, claim_attempt, lease_epoch and exact32 fence_sha256 bytes. The existing
job-grant signature domain remains unchanged. Supervisor verifies signature
before strict recursive field scanning and decoding. Access is present exactly
when Compile's actual PreparedJob has a workspace. Execute, Read and Publish
reject it. Compile access never creates a final Execute intent or recovery proof.
The sealed Compile owner exposes only its original execution/generation/dispatch
and admitted original reference to the owning workspace read path.

## Platform-call failure ownership

The generic node journal and OwnerProof effect_id remain the original whole-Code
dispatch activation. Per-call IDs and their exact original intent relation remain
in the owning platform-call journal and bounded typed diagnostics. An unknown
call prohibits replay of the whole Code node. A committed call never substitutes
for a committed original sandbox result. Verified no-effect excludes every
observed call: owner seal requires reserved/no-dispatch, while owner platform
reads require dispatched/non-null dispatch time, and lifecycle never resets that
row to reserved. Main additionally checks its owning observed-call facts.
