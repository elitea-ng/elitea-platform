# Code debug artifact v1 — original-visit integration

This private source contract supersedes the pre-consumer draft. It is source-ready;
formatting is the only executed verification. Original Code visit, child-scope,
runtime binder, final composition and migration prerequisites remain explicit.

## Declaration and authority

`debug` is the existing boolean Code field. Omission/false creates no debug
request, event or artifact. Null/nonboolean remains invalid. Compiler-only pins
are skipped by serde; old saved configuration bytes and digests remain unchanged.
The shared Main declaration inspector normalizes only current Code defaults and
known compiler-owned `recovery`/`failure_handler` separation. Optional
`platform_client:false` matches the producer's skip-false serde. Unknown fields
are not dropped. Valid saved anchors/aliases are resolved without implicit merge
application, matching the owning compiler's Value-to-Code parsing path. The raw
document is at most 1 MiB, with at most 128 nodes, depth 64 and 65,536 unique YAML
nodes. Before decoding the selected Code declaration, every alias occurrence is
counted against depth 64, 65,536 nodes and 2 MiB of scalar bytes; normalized policy
and exact configuration JSON also remain at most 2 MiB. Cycles, duplicate mapping
keys, duplicate selected Code IDs, unknown aliases and multiple documents refuse.
Unselected declaration bodies are not expanded by original declaration lookup.
The shared anchored fixture and pathological negatives are authored in both Main
and owning Rust parser tests; execution remains pending the root's combined slot.
The shared fixture uses declared user output `answer`; `result` is a reserved
framework root and is not a valid state declaration in the owning compiler.
Root declaration lookup excludes exact Map worker and Parallel branch node ownership;
registered child lookup additionally requires the sole registry's exact `AllowsNode`.

The pure saved Code input policy derives approved root names and normalized scalar
types from exact owning instructions, never supplied PreparedJob keys. Omitted or
empty input selection and exactly `[messages]` mean all present approved business
roots plus built-in `input:str`; messages/framework roots themselves are excluded.
Other selections admit only present selected roots and do not require missing
values. Values retain the existing top-level str/int/float/bool/list/dict checks;
unknown nested list/dict shapes remain opaque. Input is bounded to 512 KiB/256 keys.
The ordinary original-visit owner must consume this policy before freezing input
digests; selector cardinality alone does not reproduce existing Code semantics.

The Worker must obtain an original Main Code visit from the actual sealed
`NodeAttemptAuthority`, compiler-pinned declaration and owning graph context.
The original visit carries logical activation, original admitted execution/generation,
node/thread/step/attempt, original configuration, source/input hashes and exact
pre-workspace PreparedJob SHA. The visit is identity, never an Execute grant.
A current authenticated claim and original writer must independently authorize debug.
Main reads writer identity columns only; it never reads/evaluates checkpoint state.
It does not recompute the state-bound logical activation from partial selected input.
The original visit must also retain the sole definition producer's complete
pre-redemption source receipt/revision/fingerprint. That Main frozen-source
fingerprint differs from `definition_sha256`, which denotes the admitted Rust
compiler definition and can change after HTTP/recovery binding. They are distinct
pins; neither is recomputed or treated as equal by debug. The separate Main frozen
runtime frame after static/HTTP freeze also has its own fingerprint. The original
visit reference binds the source producer's `SourceReference` and root/registered
child lookup retains its `OriginalSource`. The source owner currently requires
the exact Pipeline YAML to remain unchanged across runtime freezing; an instruction
rewrite refuses until the owner supplies a distinct projection contract. Debug
checks the trusted source reference's YAML identity against the admitted owning
YAML, without equating any of the three full-definition fingerprints. Those source
adapters must be assembled into the original visit owner; cfg/YAML/node membership
alone is insufficient.

Nested Code requires the exact Main `SavedChildScopeRef` retained by the admitted
child runtime binder. The sole family registry verifies original ancestry/catalog,
thread, node ownership and `code_debug` purpose on each short phase. Missing scope
cannot fall back to a root definition, current application, same-name node or YAML cache.
Root binding must be independently retained. No actor/project/grant is accepted from
an admission request. A constructor without the concrete visit/scope adapter is not closure.

## Snapshot ordering

Resolve source and selected input in the admitted Code node. Build and admit the
bounded canonical original PreparedJob before workspace acquisition, dependency
preparation, compilation or user execution. Export debug immediately after that
original visit is admitted. Preparation/compile errors can therefore retain the snapshot.
The original existing PreparedJob wire bound remains 1 MiB; debug does not expand
sandbox execution limits. A failed original visit admits no debug publication.
Final prepared/compiled descriptor, dispatch request and one-time execution grant
remain separate. Debug cannot run user code or authorize its execution.

Debug denied, missing bucket, unavailable, timeout, changed/corrupt export or trace
failure emits a safe warning and leaves the Code result/state semantics unchanged.
It does not create a bucket, add credentials or change model-text artifact limits.
The complete export attempt is bounded to five seconds; trace delivery to one second.

## Admission and exact binary transfer

Private mTLS POST routes under existing execution/generation authority:
`/code-debug/admit` and `/code-debug/commit/{visit_id}`. Existing claim/fence headers
are required. Four HTTP requests share a fixed budget and do not queue; request/body
read deadline is five seconds. Admission is at most 3 MiB; exact configuration JSON
at most 2 MiB. Commit requires `application/json` and exact bounded Content-Length.

Required admission keys:
`original_visit` = `{visit_id, revision:1, digest_sha256}` (nonzero lowercase 64-hex IDs),
`attempt` (the actual original visit/Started attempt, integer 1..16),
`schema_version: elitea.runtime.code-debug-admission.v1`, `node_id`,
`graph_thread_id`, `graph_step` (canonical unsigned decimal string), `activation_id`
(the original visit logical activation), `definition_sha256`, `yaml_sha256`,
`configuration_json`, `request_sha256`, `source_sha256`, `input_sha256`,
`snapshot_sha256`, `byte_length`. All hashes are 64 lowercase hexadecimal characters.
`request_sha256` means the exact original pre-workspace PreparedJob SHA, never the
final sandbox dispatch digest. Main binds every field to the original visit and
saved `debug:true` declaration, then the exact current admitted writer.

Snapshot is at most 3 MiB and has exactly:
`schema_version: elitea.runtime.code-debug-snapshot.v1`, `language`, `source`,
`selected_input`. Source is at most 256 KiB, selected input at most 512 KiB.
All four languages use resolved user source, with no executable framework preamble.
Source preserves Unicode, CRLF, multiline and trailing newline. Raw selected-input
JSON preserves exact bytes and integer values. Main stores the exact snapshot bytes,
verifies digest/length, decoded source hash and raw selected-input hash.
No framework credential, current grant, client, workspace projection, broker mailbox,
manifests/replies or environment enters the snapshot. Authored source and explicitly
selected values remain authorized content; automatic secret discovery is not promised.

## Short reservations, upload and final CAS

The concrete original visit owner runs short authenticated metadata transactions.
Reserve one immutable publication row and opaque object key before bytes arrive.
The row key is tenant/project/execution/original admitted generation plus the exact
original visit ID/revision/digest. Logical activation and actual attempt remain
checked immutable metadata; configuration/request/snapshot identities never change.
At most 1024 reservations exist per admitted execution/generation. Same-generation
claim replacement of the same original visit may reconcile identical bytes and
reference. A newly admitted retry visit receives a different reserved object and
artifact, even when logical activation/request bytes are unchanged. Earlier
committed refs remain history and cannot be displayed as the current attempt's
artifact. A fresh generation
cannot inherit the old visit/grant/ref implicitly. Explicit historical-effect recovery
or promotion belongs to the recovery owner.

Prepare upload in another short current-visit/purpose/writer check, then release all
execution/claim/command/writer locks before body read and object-store IO. Use existing
`code-debug` bucket ACL, actor/project permissions, quotas and retention. No bucket
is created. Upload uncertainty leaves only the scoped staging reservation; no accepted ref.
A final short transaction rechecks exact original visit, purpose, live claim/writer,
reservation and metadata before generation-aware commitment. The exact writer's
short identity lock remains held through the Main metadata transaction's completion,
then releases before binary/storage IO. Return the reference only
after commit. Response loss reconciles the same stored key; changed/missing committed
bytes refuse. Never rebind the same original visit's reference to another body or
allocate a replacement key for that visit. A new retry visit receives its own key.
These are separately fenced Main and writer transactions, not an atomic
cross-database transaction. Exact takeover before reserve, after external IO and
at final metadata commit must refuse publication.

Main maintenance owns `CleanupCodeDebugStaging(ctx,limit)` with limit 1..16. After
24 hours unpublished reservations are tombstoned and only their exact reserved
project/code-debug/key is deleted outside SQL locks. Tombstones prevent reuse and
are revisited for late uncertain uploads. Committed artifacts retain normal bucket
retention. Root must wire this maintenance call and coordinate debug receipt pruning
before execution-job pruning (the private SQL template retains its FK). These are
production assembly prerequisites, not completed operations.

## Inert reference and trace

Committed reference has exactly:
`schema_version: elitea.runtime.code-debug-artifact.v1`, `project_id`,
`bucket: code-debug`, `name` (64 lowercase hex + `.json`),
`media_type: application/json`, `byte_length`, `sha256`.
No URL, presigned grant, credential, mutable version alias or snapshot data is returned.
The existing authenticated artifact download route applies current read ACL. Consumers
must verify exact byte length/SHA before interpreting or downloading the snapshot.
The reference is an immutable expected digest; the existing mutable bucket route does
not promise an immutable physical storage version.

Worker event metadata key `elitea.code.debug.v1`; Main tool metadata key `code_debug_v1`.
Proof contains exactly `revision:1`, `original_visit` (same strict reference as admission),
`attempt` (actual integer 1..16), `execution_id`, `generation` (original admitted
current canonical positive decimal), `node_id`, `activation_id` (logical),
`request_sha256` (original prepared SHA), `status`, optional `artifact`.
Status is `committed|denied|unavailable`; artifact exists only for committed.
Run ID hashes length-prefixed execution, admitted generation, original visit ID,
visit revision (decimal), visit digest, actual attempt (decimal), logical activation
and original prepared SHA under `elitea.graph.code.debug-trace.v1\0`.
Tool name is `<node> / debug export`; inputs empty, output/error null, finish stop,
start/finish equal. Event is partial with no content/state/model turn.
Main verifies signed frame execution/generation, exact node and committed durable
receipt for that exact visit/attempt before accepting a reference. Replayed warnings
for the same visit cannot erase a committed ref. Distinct retry visits cannot merge
their traces or reuse the old artifact as current; each trace labels its actual attempt.
Safe warning: `The Code debug artifact is unavailable.` No internal error/source/input
or authority is placed in a trace. Ordinary default Stop and recovery Code paths both
require the actual admitted attempt seam before release.

## Evidence boundary

Authored source/serializer/real-caller/guard tests are unrun. Formatter parsing does not
prove types, compilation, SQL atomicity, upload recovery, deployment or browser behavior.
Root owns the combined verification slot and must run original-source/unknown-source,
ordinary/recovery, compile-failure, off/false, child ownership, current-claim/writer,
ACL/missing bucket, uncertain upload/commit response loss, cancellation, same-generation
replacement, distinct retry visit/publication/history, fresh-generation refusal and
cleanup tests against the assembled adapters.
