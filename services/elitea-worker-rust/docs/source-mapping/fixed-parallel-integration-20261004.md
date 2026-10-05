# Fixed Parallel private integration mapping

Status: private, production admission gated. Fixed declared branches, item Map,
and explicit Reduce remain separate designs. This packet composes the corrected
fixed runtime with compiler/YAML admission and the existing PostgreSQL owner.

| Existing owner | Private change | Behavioral proof or remaining boundary |
| --- | --- | --- |
| `graph/yaml.rs` strict prototype | 2–16 owned branches and 1–8 concurrency; one declared list output | Strict bounds and whole-pipeline fixtures |
| `graph/compiler.rs` raw node admission | Parallel variant and isolated owned Agent graphs; owned nodes excluded from parent binding/recovery frontier | Conflict, route, entry, interrupt, legacy name/order tests |
| `graph/application.rs` task/variable mapping | `parallel_application.rs` freezes existing task/fixed/variable/template mappings before admission | Frozen values retain names; unmapped parent state is absent |
| `graph/parallel.rs` disconnected prototype | Typed outcomes, declared-order bounded drain, complete resume sets and stable child recovery | Original runtime tests (19) plus marker preservation fixture |
| `graph/parallel_structure.rs` new private resource guard | Iterative depth 125/value-count 100,000 validation before hash/serialization/clone; 8 MiB streamed business snapshot ceiling | Deep/wide/shared-budget, pre-effect/pre-checkpoint and frozen mapping fixtures |
| `graph/parallel_checkpoint.rs` typed metadata | Atomic appender required; checked snapshot retained; immutable old replay unchanged | Competing freeze/pause/decision parents fail closed |
| `state/postgres_checkpointer.rs` existing writer transaction | `save_checkpoint_inner` condition checks complete expected latest after fenced immutable replay | Per-field parent comparison tests; real SQL contention/reclaim not run |
| `state/postgres_checkpointer/parallel_children.rs` existing hash/factory | Reusable activation helper retains exact root authority/hash; exact thread catalog forwarded | Scoped owner exact catalog tests; no prefix grant |
| `agents/session.rs` checkpointer erasure | Retains the one typed authority alongside its Checkpointer upcast | Compiler mismatched-Arc fixture; production constructor remains gated |
| `graph/parallel_compiler.rs` new private compiler helper | Single authority, deadline and required descendant proof capability; no blanket authority impl | Same underlying Arc required before scoped wrapping |
| `pipeline/parallel_application_resolver.rs` new private binding helper | Exact frozen participant/digest and event rebinding; call owners/gates retained | Ordinary Agent without sealed identity refuses admission |
| `graph/parallel_published_resume.rs` new private parent proof | Last session event, immutable/latest checkpoint, exact activation/frontier and complete cards | Forged author/checkpoint/digest/cards and partial/foreign decisions rejected |
| Scoped receipt and event owners | Typed overlay, exact branch catalog and factory-minted event sidecar composed from separate owner packet | Receipt mutation/races and exact local scope/attribution tests |

The runtime uses existing checkpoint metadata and claim-fenced tables. It adds no
shadow store, migration, dependency upgrade, new interrupt ledger or raw database
bypass. New Parallel revisions remove an older graph-call append marker while
preserving original receipt bytes. The scoped atomic overlay independently
validates those bytes before delegating the exact expected-parent transaction.

The compiled branch contains exactly its declared Agent then END. The branch
configuration binds the declared node and frozen saved participant fingerprint.
Private parent event scope is captured outside the business mapping/digest; exact
factory thread membership owns projection. Ordinary state declaration order and
non-Parallel digest construction retain their existing behavior.

Blocked records a typed parent occurrence and returns a non-success graph error.
It cannot become successful END or Completed by checking a missing result. Public
BLOCKED settlement/delivery remains a proposal until Main/UI classify the exact
current typed receipt. Successful terminal child checkpoint replay does not prove
an external effect's intent/outcome crash window; approved effect receipts remain
mandatory before effectful admission.

Production remains gated on authenticated saved Agent identity retention, concrete
published descendant continuation, public non-success delivery, root deadline
assembly and real PostgreSQL process/reclaim/output-redelivery acceptance. Focused
in-memory tests, SQL source inspection, Clippy and fmt are reported separately from
those unperformed acceptance checks in the private freeze manifest.

The guarded revision retains the earlier passing snapshot as historical evidence.
Its resource ceilings allow arbitrary JSON shapes within bounded structure: at most
125 child edges and 100,000 values across a state or receipt walk. The depth reserves root-value and state/metadata-object room below serde's
rejected 128th container; new occurrence metadata is checked again
before append to account for wrapper nesting. The value ceiling covers broad trees
of tiny values that can otherwise fit a byte cap. One iterator per level bounds the
validator's memory; it does not recurse or push every sibling into a pending list.
The complete Parallel business snapshot is streamed through an 8 MiB cap before
cloning, while existing branch/joined caps remain in force. These restrictions apply
to the gated fixed Parallel family and preserve ordinary pipeline behavior.

The receipt-guard revision validates the complete child checkpoint again after the
captured typed Paused/Interrupt receipt is inserted. Raw pause data at the allowed
depth or value-count boundary can exceed that bound once wrapped. The regression
fixture proves both raw payloads pass their own guard but the augmented candidate
is rejected before the generic component store accepts it. PostgreSQL has its own
stricter depth guard; no corrupt PostgreSQL write was reproduced or claimed.
