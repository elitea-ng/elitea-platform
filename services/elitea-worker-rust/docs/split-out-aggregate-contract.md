# SplitOut and Aggregate pipeline nodes — contract (Gate 5c, first tier)

Status: implemented in the Rust Worker and **not admitted in production**. The compiler accepts `split_out` and
`aggregate` only in builds with the off-by-default Cargo feature `graph-extensions-rehearsal`. Web shows them only
when `VITE_GRAPH_EXTENSIONS_REHEARSAL=true`. Both gates flip together after deployed acceptance (Point 5, Wave 2e).

The Worker is the authority. Web admission (`apps/elitea-web/src/features/pipelines/lib/graphShapingAdmission.helpers.ts`)
mirrors these rules so that authors see readable errors before saving.

Sources: Spark `explode`/`posexplode`/`groupBy().agg()` and n8n Split Out / Aggregate / Summarize. These sources
informed the design. They do not define compatibility. Wider shaping operations are listed in
`data-shaping-node-catalog.md`.

## 1. Common keys

Both node types reject unknown keys.

| Key | Rule |
|---|---|
| `id` | A graph id (`[A-Za-z0-9_.:-]`, at most the node-id byte limit). |
| `type` | `split_out` or `aggregate`. |
| `source` | A declared state variable. It cannot be a builtin (`input`, `messages`, …) or a reserved key. SplitOut `row_field` needs a `dict`. Every other mode, and Aggregate, needs a `list`. |
| `output` | A list with exactly one declared `list` state variable. It differs from `source` and cannot be a builtin or reserved key. |
| `transition` | A node id, `END` or absent. When it is absent, the node has no outgoing edge (as with `state_modifier`). |
| `limits` | An optional mapping. Each value is an integer from `1` to the ceiling. A limit can only lower its ceiling. |

| Limit | Ceiling and default | Applies to | Meaning |
|---|---|---|---|
| `input_items` | 10,000 | both | Rows in `source`. SplitOut `row_field` has exactly 1. |
| `output_items` | 10,000 | both | Rows in the emitted list. |
| `groups` | 1,000 | aggregate only (refused on `split_out`) | Distinct groups, or parents with `regroup: parent`. |
| `bytes` | 524,288 | both | Compact JSON bytes of the emitted list. |
| `depth` | 32 | both | JSON nesting of the emitted list. The list is depth 1, each row depth 2. |
| `values` | 32,768 | both | JSON values in the emitted list, containers included. The list itself counts as 1. |

The `values` ceiling is half the whole-state checkpoint structure limit (65,536). A maximal shaping output can
therefore still pass a later Map or Parallel checkpoint boundary. A SplitOut envelope row holds at least 5 values
(row, `parent_index`, `position`, `data` and the element), so under the default `values` ceiling SplitOut emits at most
6,553 rows. `values` is reached before `output_items` for scalar elements.

A node YAML larger than 64 KiB is refused.

## 2. JSON pointers and names

- **Pointers** (RFC 6901):
  - A pointer is a non-empty string that starts with `/`. It has at most 512 bytes and 32 tokens.
  - `~0` means `~` and `~1` means `/`. Any other `~` sequence is refused when the pipeline compiles.
  - Empty tokens are legal.
- **Pointer resolution:**
  - Against an object, a token is an exact key.
  - Against an array, a token must be canonical decimal (`0` or `[1-9][0-9]*`) and in range. `-` and leading zeros
    never match.
  - Anything that does not resolve counts as **missing**. That includes traversal through `null` or a scalar.
  - A pointer that resolves to JSON `null` is **null**, not missing.
- **Names** (`output`, `destination`, `retain.fields[].output`, `except` fields, `group_by[].output`) are literal
  object keys. A name is non-empty, has at most 256 bytes and contains no control characters.

## 3. `type: split_out`

```yaml
- id: split_items
  type: split_out
  source: orders            # list (dict for row_field)
  output: [order_lines]     # declared list
  split: {mode: rows_field, path: /lines}   # mode list | row_field | rows_field; path required unless list
  destination: line         # name of the element inside data
  retain: {mode: all}       # none (default) | all | only | except
  missing_list: error       # error (default) | empty
  null_list: error          # error (default) | empty
  remove_source: true       # default true
  transition: process
```

**Parents by mode:**
- `list`: `source` is the list. All of its elements belong to parent 0, and only `retain: none` is allowed.
- `row_field`: `source` is one object that holds the list at `split.path`. It is parent 0.
- `rows_field`: `source` is a list of rows. Row *i* holds its list at `split.path` and is parent *i*.

**Retention:**

| Mode | Meaning |
|---|---|
| `none` | `data` holds only the destination. |
| `all` | Copies the parent's top-level fields. The parent must be an object (`invalid_row`). |
| `except` | Like `all`, but leaves out the listed top-level names. `fields` holds 0–64 unique names. |
| `only` | Selections `[{path, output, missing}]`, 0–64 of them, with unique outputs that differ from `destination`. `missing` is `error` (default), `null` or `skip`. A null value is retained as null. |

**Emitted rows.** Each element produces one row:

```json
{"parent_index": 0, "position": 0, "data": {"<retained…>": "…", "<destination>": "<element>"}}
```

- Rows come in parent order and then in element order.
- `position` is the element's 0-based index in its parent's list.
- Object keys are emitted in sorted order, because state objects are ordered maps.

**Semantics:**
- `remove_source: true` removes the split list from a copy of the parent before retention. Retained data therefore
  never contains the split list. The list is removed from its containing object, or from its slot in a containing
  array.
- `destination` equal to the last token of a one-token `split.path` replaces the list field with its element.
  This is n8n's common pattern, and with `remove_source: true` it does not collide.
- A missing list follows `missing_list`, and a null list follows `null_list`. With `empty`, the parent produces no
  rows, but its retention checks (`invalid_row`, `field_collision`, `only` selections) still run. With `error`, the
  node fails with `missing_field` or `null_value`.
- An empty list produces zero rows for that parent.
- A value at `split.path` that is neither a list nor null fails with `type_mismatch`. A scalar is never wrapped.
- For every parent with `all` or `except`, a retained top-level name equal to `destination` fails with
  `field_collision`. This check runs even when that parent's list is empty. `only` collisions are refused when
  the pipeline compiles.

## 4. `type: aggregate`

```yaml
- id: summarize
  type: aggregate
  source: order_lines
  output: [summary]
  layout: split_out           # plain (default) | split_out
  regroup: none               # none (default) | parent
  group_by:                   # 0..64; outputs unique
    - {path: /line/sku, output: sku, missing: error}   # missing: error (default) | null
  operations:                 # 1..64; outputs unique across group_by AND operations
    - {operation: count_rows, output: lines}
    - {operation: sum_int, field: {path: /line/qty}, output: quantity}
  transition: END
```

**Layouts:**
- `plain`: every row is an object (otherwise `invalid_row`). Pointers resolve against the row.
- `split_out`: every row is exactly `{parent_index, position, data}` (otherwise `invalid_envelope`).
  - `parent_index` and `position` are non-negative integers, compared by exact value.
  - `data` is an object.
  - Pointers resolve against `data`. `collect_rows` projects `data`.

**Field selection.** `field` is `{path, missing, null}`.
- `missing` is `error` (default), `null` or `skip`.
- `null` is `keep` (default), `error` or `skip` for `collect`, `first` and `last`.
- For `sum_int`, `min_int` and `max_int`, `null` is `error` (default) or `skip`. **`keep` is refused.**

The policies apply in this order:
1. Resolve the pointer.
2. If the pointer is missing, apply `missing`. With `null`, the value becomes JSON null and continues to the next
   step.
3. Apply the `null` policy.

A skipped row does not contribute to that operation. Every row passes the policies, also after `first` has found
its value.

**Operations:**

| `operation` | Extra keys | Result | Empty group or all skipped |
|---|---|---|---|
| `count_rows` | none | The number of rows in the group. Skips do not apply. | `0` |
| `collect_rows` | `retain`: `all` (default), `only` or `except`. **`none` is refused.** | A list of projected rows. | `[]` |
| `collect` | `field`, `merge_lists` (default false) | A list of the selected values. With `merge_lists`, each value must be a list (otherwise `type_mismatch`) and the lists are concatenated. | `[]` |
| `sum_int` | `field` | The checked i64 sum. | `0` |
| `min_int`, `max_int` | `field` | An i64. | `null` |
| `first`, `last` | `field` | The first or last selected value in row order. | `null` |

**Integers.**
- A number qualifies only if its exact decimal value is an integer: `2`, `2.0`, `1e2` and `10e-1` all qualify.
  `2.5` fails with `type_mismatch`, and so does a non-number.
- Values outside i64 fail with `integer_overflow`, and so does an overflowing sum.
- An exponent that does not fit i64 arithmetic fails with `unsupported_number`.
- Arithmetic never uses floating point. Results are emitted as plain integers.

**Groups.**
- Group keys use **canonical equality**:
  - numbers compare by exact decimal value (`1 == 1.0 == 1e0`, and `-0 == 0`);
  - objects compare by their sorted key/value pairs;
  - arrays compare element by element, in order;
  - strings compare by exact code points, with no normalization and no case folding.
- The emitted key is the **first-seen** representation.
- Groups appear in first-appearance order. Rows within a group keep input order.

**Output rows are flat:**

```json
{"<group output>": "<key>", "…": "…", "<operation output>": "<result>", "…": "…"}
```

- Group-output and operation-output names must be unique **across both lists**. This is checked when the pipeline
  compiles.
- Without `group_by`, the output has exactly one row.
- Empty input with `group_by` gives `[]`.
- Empty input without `group_by` gives one row of empty-group results (decision U2): `count_rows` 0,
  `collect`/`collect_rows` `[]`, `sum_int` 0, and `min_int`/`max_int`/`first`/`last` null.

### 4.1 `regroup: parent`

`regroup: parent` requires `layout: split_out` and forbids a non-empty `group_by` and `collect_rows`.

1. Rows are grouped by `parent_index`, ascending. Within a parent, rows are ordered by `position`, ascending.
   Gaps are allowed, for example when an intermediate node filtered rows. A duplicate
   `(parent_index, position)` fails with `regroup_conflict`.
2. The **consumed** fields are the first token of every operation's `field.path`.
3. The **base** is the lowest-position row's `data` without its consumed fields. Every other row's `data` without
   its consumed fields must canonically equal the base (otherwise `regroup_inconsistent`).
4. The output row is the base plus each operation output under its literal name. An operation output that equals a
   base field fails with `field_collision`.
5. Empty input gives `[]`. The number of parents counts against `groups`.

**Exact inverse.** This Aggregate:

```yaml
layout: split_out
regroup: parent
operations: [{operation: collect, field: {path: /f}, output: f}]
```

restores the input of this SplitOut:

```yaml
split: {mode: rows_field, path: /f}
destination: f
retain: {mode: all}
remove_source: true
```

It holds for every parent whose list is non-empty. A parent with an empty list emits no envelope, so it cannot be
restored. This is documented behavior, not an error.

## 5. Budgets, atomicity and determinism

- **`input_items`** is checked before any work.
- **Every emitted row is charged** for its exact compact JSON bytes, its value count and its depth, as the row is
  built. A limit fails at the first violating row, so memory stays at most `bytes` plus one row.
  - Aggregate charges each collected value as it accumulates. This is a lower bound of the output, so collection
    fails fast too.
  - Aggregate charges the final rows exactly.
- **`output_items`** and **`groups`** are checked on insertion.
- **Validation is complete before the node writes.** It then emits one update to its `output` channel. It never
  writes `source` or any other channel.
- **Output is a pure function of the input state and the configuration.** Both nodes are deterministic, so they
  join `recovery_frontier_supported`: a recovery re-runs them and gets the same result. They need no runtime
  receipts and no node-recovery policy.
- **Config digests** use the domains `elitea.graph.split_out.config.v1\0` and `elitea.graph.aggregate.config.v1\0`.
  The pipeline digest kind tags are `split_out` and `aggregate`. Limits are digested as authored, with a presence
  tag, so a later ceiling change does not move old digests.

## 6. Errors

A failure is `GraphError::NodeExecutionFailed { node: <node id>, message }`, where the message is
`graph.shaping.<code>: <config field> at item <i>[ position <p>]`.

- The config field is the path of a configuration key, such as `split.path`, `operations[1].field` or
  `limits.bytes`.
- The item is the index of the input row: the parent for SplitOut, the row for Aggregate.
- **Messages never contain data values, pointer text or state values.**

| Code | When |
|---|---|
| `invalid_source` | `source` is absent or has the wrong JSON type. |
| `invalid_row` | A row is not an object where one is required. |
| `invalid_envelope` | A row is not a strict `{parent_index, position, data}` envelope (`layout: split_out`). |
| `missing_field` | A pointer is missing and the `missing` policy is `error`. |
| `null_value` | A value is null and the `null` policy is `error`, or `null_list: error` applies. |
| `type_mismatch` | A value has the wrong type: a non-list split value, a non-integer for `*_int`, or a non-list with `merge_lists`. |
| `integer_overflow` | An integer value or sum leaves i64. |
| `unsupported_number` | An exponent does not fit checked i64 arithmetic. |
| `field_collision` | A retained name equals `destination`, or a regroup output equals a base field. |
| `regroup_conflict` | A `(parent_index, position)` pair is duplicated. |
| `regroup_inconsistent` | The base rows of one parent differ. |
| `limit_exceeded` | Any limit. The config field names the limit, for example `limits.bytes`. |

Configuration errors are refused when the pipeline compiles and surface as
`graph.pipeline.invalid_configuration`. When the gate is off, a pipeline that contains either node type is refused
with `graph.pipeline.unsupported_capability` ("the pipeline contains a node type that is not enabled").

Public failure kinds (`pipeline.shaping_invalid` and `pipeline.shaping_limit`) are Wave 2. Until then, a runtime
shaping failure reaches the user through the existing generic node-failure text.

## 7. Worked examples

### 7.1 n8n "split items → process → aggregate back"

State: `orders: list`, `lines: list`, `orders_out: list`.

```yaml
- id: split
  type: split_out
  source: orders
  output: [lines]
  split: {mode: rows_field, path: /items}
  destination: items
  retain: {mode: all}
  transition: process
- id: process                    # an llm or state_modifier node that rewrites lines
  ...
  transition: join
- id: join
  type: aggregate
  source: lines
  output: [orders_out]
  layout: split_out
  regroup: parent
  operations: [{operation: collect, field: {path: /items}, output: items}]
  transition: END
```

`orders = [{"id": "A", "items": [1, 2]}, {"id": "B", "items": [3]}]` produces these rows in `lines`:

```json
[{"data":{"id":"A","items":1},"parent_index":0,"position":0},
 {"data":{"id":"A","items":2},"parent_index":0,"position":1},
 {"data":{"id":"B","items":3},"parent_index":1,"position":0}]
```

When `process` leaves the rows unchanged, `orders_out` equals `orders`. When it rewrites `data.items`, each order is
rebuilt with the rewritten items in their original positions.

### 7.2 Group-by summary

`source = [{"sku":"x","qty":2},{"sku":"y","qty":"1"},{"sku":"x","qty":3.0}]`. Because `"1"` is a string,
`sum_int` fails with `type_mismatch` at item 1. With `{"sku":"y","qty":1}` instead:

```yaml
group_by: [{path: /sku, output: sku}]
operations:
  - {operation: count_rows, output: rows}
  - {operation: sum_int, field: {path: /qty}, output: qty}
```

```json
[{"qty":5,"rows":2,"sku":"x"},{"qty":1,"rows":1,"sku":"y"}]
```

### 7.3 Empty input

The source is `[]`.

- **Without `group_by`**, the operations `count_rows`→`n`, `collect`→`all`, `sum_int`→`s`, `max_int`→`m` and
  `first`→`f` give one row: `[{"all":[],"f":null,"m":null,"n":0,"s":0}]`.
- **With any `group_by`**, the result is `[]`.
- **SplitOut** emits `[]` for an empty `list` source, and for parents whose lists are all empty.
