# Data shaping node catalog

## Scope and delivery

The user extends Gate 5c on 2026-10-04 beyond SplitOut and Aggregate.
This catalog is planned capability, not a statement of compiler or editor admission.
Code-node completion remains the first delivery priority.
Deliver each shaping family through typed contracts, independent review, compiler/editor wiring and actual runtime acceptance.

| Family | Explicit operations | Primary contract |
| --- | --- | --- |
| Fields | Select/Project, Rename, Set/Compute, Remove | Preserve unselected data unless the operation explicitly projects or removes fields. |
| Selection | Filter, Partition, Distinct/Deduplicate | Define predicates, retained duplicates, empty results and stable item identity. |
| Expansion | SplitOut/Unnest, Flatten, FlatMap | Define expansion depth, cardinality, parent identity and element ordering. |
| Transformation | Map, Cast/Convert, Normalize | Define per-item input/output types, conversion failures and null handling. |
| Reduction | GroupBy, Aggregate, Fold/Reduce, Scan | Define grouping, initial accumulator, reducer semantics and output cardinality. |
| Ordering and batching | Sort, Reverse, Slice/Limit, Chunk/Batch | Define stable ties, bounds, offsets, batch identity and ordering. |
| Combining datasets | Concat/Union, Zip, Join, Merge | Define input ownership, keys, duplicates, conflicts and unmatched rows. |
| Reshaping | Pivot, Unpivot | Define key collisions, missing cells, schema and expansion bounds. |
| Validation | Schema validation, required-field checks | Return explicit typed failures or validated data. Never silently discard invalid records. |

These are explicit operations within a small set of node families.
An operation's YAML schema must remain specific, validated and discoverable.
Do not expose arbitrary untyped configuration through a generic transformation node.

The catalog draws on [Spark's built-in transformations](https://spark.apache.org/docs/latest/sql-ref-functions-builtin.html)
and [n8n's data transformation nodes](https://github.com/n8n-io/n8n-docs/blob/main/docs/build/work-with-data/expressions-versus-data-nodes.md).
These references inform the design. They do not establish compatibility or implementation parity.

## Execution and shaping

Gate 5a owns Map's child execution, concurrency, interruption and recovery.
Gate 5c owns the deterministic input/output shaping around that execution.
SplitOut must not silently start children. Map must explicitly own each item execution.

Fold processes items in order with an explicit initial accumulator.
Scan retains each intermediate accumulator.
Parallel Reduce requires an associative reducer with a defined identity and deterministic collection rules.
Custom sequential reduction does not imply safe parallel reduction.

Define whether each operation receives plain rows, item envelopes or another typed layout.
Do not confuse a returned list with n8n's item execution model.
Preserve item lineage explicitly when converting between layouts.

## State variable reducers

Node transformations and state channel reducers have separate contracts.
A node shapes its result. A channel reducer combines accepted updates to a declared variable.
Keep existing overwrite behavior by default. Do not infer a reducer from the connected node family.

The pinned ADK 2.2.0 supplies Overwrite, Append, Sum and Custom in `adk-graph/src/state.rs`.
Its Append accepts scalars, and its Sum converts through floating-point values with zero fallback.
Those permissive defaults cannot define strict user-facing list and numeric contracts.

Expose explicit bounded typed reducers, including overwrite, list append, checked numeric sum and deterministic dictionary merge.
Specify ordering and key-conflict behavior. Reject incompatible values before changing state.
Custom reducers are registered platform implementations, not executable functions supplied in YAML.
Keep arbitrary user code inside the admitted Code sandbox.

## Common acceptance requirements

- Define empty, missing, null and wrong-type behavior explicitly.
- Bound input items, groups, expansion, recursion, bytes and processing time.
- Use stable ordering and exact item identities across recovery.
- Validate the complete result before one atomic state update.
- Preserve unrelated state variables and existing pipeline definitions.
- Reject arithmetic overflow and unsupported precision instead of silently rounding.
- Verify YAML/editor round trips and readable errors on both chat surfaces.
- Prove cancellation, repeated visits and original-activation recovery for admitted execution nodes.

SplitOut and Aggregate are the first implementation tier.
Remaining catalog operations stay planned until their contracts and acceptance are complete.
