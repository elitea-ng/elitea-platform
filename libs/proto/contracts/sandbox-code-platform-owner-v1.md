# Original Code platform owner protocol v1 (private source proposal)

This protocol is separate from whole-Code recovery read/seal. It never creates,
restarts, dispatches, or submits a job. Main reads the original Supervisor through
its existing configured mandatory mTLS client, audience and requester identity.

POST `/elitea.runtime.code-platform.v1/jobs/{original_job_key}/{operation}`.
Closed operations: `read-retained-runtime`, `read-pending-platform-call`,
`publish-committed-platform-reply`. No query, redirects, alternate host or runtime
selector. Content type is exactly application/json. Request has schema
`elitea.sandbox.code-platform-owner-request.v1`, grant, and required-nullable
committed_reply_base64url. Only publish carries a reply (binary <=2166800 bytes).
Read bodies <=16KiB; publish bodies <=3MiB. Reply/response <=3MiB, deadlines10s.

Grant envelope is schema `elitea.sandbox.code-platform-owner-signed-grant.v1`,
key_id, claims_base64url, signature_base64url. Ed25519 signs domain
`elitea.sandbox.platform-broker-owner-grant.ed25519.v1\0` + claims length u64BE
+ exact claims bytes. Claims <=8KiB, signature64, canonical unpadded base64url.
Verify signature before decoding strict fields. Claims schema
`elitea.sandbox.code-platform-owner-grant.v1`, purpose `platform_broker_runtime`:
tenant_id,project_id,execution_id,original_generation,claim_id,claim_attempt,
lease_epoch,fence_sha256,activation_id,attempt,dispatch_activation,job_key,
request_digest,binding_sha256,prepared_job_sha256,prepared_fingerprint,compiled_execute,policy_sha256,max_calls,
max_total_bytes,supervisor_audience,requester_workload_identity,operation,
sequence,platform_request_sha256,committed_reply_sha256,issued_at_unix_millis,
expires_at_unix_millis. Last three reply selectors are required-nullable; present
only on publish, sequence1..max_calls. Operation serializes snake_case. Current
claim selectors come from Main, lifetime<=30s/current lease/execution deadline.
Main signs publish only from its exact immutable committed platform-call record;
the signed hash binds the exact Main-signed binary committed reply.

Supervisor stores a separate immutable broker binding from the actual validated
PreparedJob at original Execute admission. It contains schema
`elitea.sandbox.code-platform-binding.v1`, prepared_job_sha256,prepared_fingerprint,policy_sha256,
max_calls,max_total_bytes,compiled_execute. The owning PreparedJob getter enforces revision5 and
platform revision1; policy syntax is64lowerhex, max_calls1..4096,
max_total_bytes1..67108864. No later read grant can create this binding.
It matches the signed original whole-Code intent and final exact PreparedJob SHA.

Indexed Execute hydration carries the refreshed signed original intent in `HydrateSandboxDependenciesRequestV1.code_execution_intent_json`, field 16.
Field numbers 6 through 15 remain reserved.
Supervisor verifies the peer, current execution grant, original dispatch, exact PreparedJob, and signed intent before runtime staging.
Supervisor registers the immutable whole-Code and broker bindings before it provisions the inert runtime.
Platform-enabled hydration requires the intent. Legacy plain hydration can omit it.
Hydration does not dispatch user code or change the original execution identity.
Native hydration validates the underlying dependency revision through the existing extension parser.
Workspace and platform extensions retain their validation and exact request digest.

Owner selects only its original row under exact tenant/project/job/request,
committed original WholeCodeBinding digest, broker binding and configured
Supervisor audience. Require phase dispatched, non-null dispatched_at/runtime_id,
non-cancelled, live original owner lease/epoch and no terminal recovery receipt.
Runtime kind comes from its backend, never from runtime ID text. Verify existing
runtime fingerprint and exact instance ID before and after the fixed operation,
then re-read the same live row owner/epoch/runtime. Any mismatch refuses output.
Read exposes only actual ledger-derived Docker full CID or Kubernetes PodUID,
owner epoch/lifecycle and exact original prepared/broker binding; no invocation
or checkpoint authority follows from that observation.

Response schema `elitea.sandbox.code-platform-owner-response.v1`, state
`running`, runtime, pending_call_base64url, reply_published. Runtime contains
kind (`docker`|`kubernetes`),runtime_id,owner_epoch,binding_sha256,
prepared_job_sha256,prepared_fingerprint,compiled_execute,policy_sha256,max_calls,max_total_bytes,lifecycle (`dispatched`).
The two operation outputs are required-nullable: retained read null/null,
pending read nullable CP1 frame<=327688/null, publish null/true. Failure is an
HTTP refusal without any runtime/proof payload. Response is no-store.

Only `read-retained-runtime` can return this additional HTTP 200 body:

```json
{"schema":"elitea.sandbox.code-platform-owner-response.v1","state":"not_ready","runtime":null,"pending_call_base64url":null,"reply_published":null}
```

This closed body requires all five fields. The three outputs are null.
It gives no runtime, mailbox, publication, dispatch, replay, or lease authority.
Supervisor first verifies the Main signature, peer, exact route, operation, and grant lifetime.
Main verifies the current parent claim and exact original intent before owner IO.
Main rechecks the current parent claim after NotReady before returning step disposition `idle`.

Genuine absence before admission can return NotReady under that verified authority.
An existing Reserved row must match the request digest and each present original or broker binding.
Null binding fields represent admission in progress. They authorize only this no-effect observation.
Cancelled and terminal rows refuse the observation. Dispatched rows require the existing live owner lease and exact bindings.
Generic HTTP refusals, malformed bodies, or other operations never become NotReady.
No new grant purpose, runtime selector, or execution request follows from this response.

Both read operations can also return this HTTP 200 body:

```json
{"schema":"elitea.sandbox.code-platform-owner-response.v1","state":"completed","runtime":null,"pending_call_base64url":null,"reply_published":null}
```

This observation has all five fields and three null outputs.
Supervisor verifies the exact grant, request digest, original binding, and broker binding.
The original job must have a dispatched, non-cancelled, completed row with a valid persisted result.
Supervisor repeats this check if normal completion closes the runtime during a read.
Publication, cancelled jobs, wrong bindings, missing results, and generic refusals cannot return this observation.
Main rechecks the current claim and original snapshot before returning `idle`.
The observation stops further mailbox work while the original submission returns its receipt.
It gives no success, replay, dispatch, runtime, or publication authority.
The original submission receipt remains the only result authority.

Both read operations can observe the stopped process before its result commit:

```json
{"schema":"elitea.sandbox.code-platform-owner-response.v1","state":"completing","runtime":null,"pending_call_base64url":null,"reply_published":null}
```

This body has the same five required fields and three null outputs.
Supervisor first verifies the current dispatched row, live owner lease, exact bindings, and non-cancelled state.
The original runtime must match its immutable identity before and after receipt observation.
The stopped runtime must provide a bounded, valid terminal Code result with exit code zero.
Supervisor then rechecks the same owner, epoch, runtime, bindings, and cancellation state.
A completed ledger check takes precedence if the original submission commits during that observation.
Missing, failed, invalid, replaced, running, cancelled, or unauthorized observations retain the original refusal.
Only read operations can return this body. Publication never uses this observation.
Main rechecks the current claim and original snapshot before returning `idle`.
This read does not commit a result, acquire a lease, or perform a platform operation.
The original submission must still commit and return its result within the existing deadline.
No new success, replay, dispatch, runtime, mailbox, or publication authority follows from this body.

Backend calls only fixed helpers --platform-read/--platform-reply with launch
revision1 (retained_runtime_id,prepared_sha256,policy_sha256,max_calls,
max_total_bytes) for plain Execute, preserving its exact original bytes. Compiled
Execute uses revision2 with those same fields plus request_digest, derived only
from the stored original compiled Execute Control.intent_digest(Execute). It must
equal WholeCodeBinding.request_digest. prepared_sha256 remains the distinct final
prepared fingerprint. Docker/Kubernetes helpers compare runtime request identity
to request_digest for revision2; runner capture independently verifies it against
its original compiled control. Neither accepts a caller-selected digest or a
fingerprint equality fallback. Launch <=4096. The backend verifies
original full Docker ID or PodUID around each helper. Publish verifies the
original prepared/policy/sequence/request and signed committed reply. It must not
perform any platform operation. Backend ports default to refusal until the owning
fixed wrapper/image/key installation is present; compiled eligibility is separately
default denied. These source contracts do not claim measured image/runtime proof.

prepared_job_sha256 hashes exact raw canonical PreparedJob.to_transport bytes.
prepared_fingerprint is the existing domain/length-framed PreparedJob.fingerprint.
Fixed launch prepared_sha256 receives prepared_fingerprint. The required-nullable
compiled_execute selector is null for plain Execute, where original
request_digest==prepared_fingerprint remains mandatory. For compiled Execute it
contains binding (the exact existing Rust snapshot Binding JSON),
snapshot_key_sha256 and selected_descriptor_sha256. Reconstruct the existing
Execute Control: binding.key must equal snapshot_key_sha256, its base prepared
fingerprint must equal prepared_fingerprint, and its exact Execute intent digest
must equal original request_digest. The original descriptor bytes must be canonical,
validate against that control and hash to selected_descriptor_sha256. Original
registration independently verifies the actual signed AuthorizedSnapshotExecute,
exact admitted SnapshotProfile/source/image/policy/wrapper/module identity and
final PreparedJob bytes before persisting this immutable selector. Later read
grants and runtime responses must match the same stored selector exactly. No
plain-to-compiled fallback or new artifact selection is permitted.

Supervisor lease renewal retains owner_epoch; existing lease takeover can advance
it while preserving the original runtime. Each fixed operation requires unchanged
actual owner, epoch and runtime across its read/operation/recheck. Main pins the
attested epoch across a pending-call/reply sequence and refuses drift until a
fresh authenticated attestation; it cannot infer runtime replacement or restart
permission. Original kind/runtime identity remains immutable.

The generic Code failure effect is the original whole-Code dispatch. Platform
call IDs remain in their owning call journal and bounded typed failure details.
No platform receipt, retained runtime view or call relation grants whole-Code
completion, no-effect, retry or checkpoint authority.
