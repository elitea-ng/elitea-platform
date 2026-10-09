/**
 * Numeric pipeline node identifiers.
 *
 * YAML reads an unquoted `1` as the integer 1, so a stored pipeline written
 * as `entry_point: 1` / `- id: 1` / `transition: 2` reaches the editor with
 * numbers where every helper expects strings (`id.replace is not a function`
 * and friends). The Rust Worker accepts that spelling and treats each integer
 * as its canonical decimal string, so the editor does the same, at the point
 * a document is parsed, and the rest of the editor only ever sees strings.
 *
 * Only `Number.isSafeInteger` values are converted — the Worker refuses
 * anything outside +/-(2^53-1). Every other value (fractions, booleans,
 * objects, unsafe integers, null) is returned exactly as authored so
 * validation can report it instead of this module silently coercing it.
 *
 * The stored YAML TEXT is never touched here: callers keep the author's text
 * and only the parsed view is normalized.
 */

type Json = Record<string, unknown>;

const isRecord = (value: unknown): value is Json => value !== null && typeof value === 'object' && !Array.isArray(value);

/** `String(n)` for a safe integer, otherwise the very same value. */
const normalizeOne = (value: unknown): unknown => (typeof value === 'number' && Number.isSafeInteger(value) ? String(value) : value);

/** Maps a list of identifiers; keeps the original array when no entry changes. */
function normalizeList(value: unknown): unknown {
  if (!Array.isArray(value)) return value;
  const mapped = value.map(normalizeOne);
  return mapped.some((entry, index) => !Object.is(entry, value[index])) ? mapped : value;
}

/** Maps the values of an object (HITL `routes`); keeps the original object when no value changes. */
function normalizeValues(value: unknown): unknown {
  if (!isRecord(value)) return value;
  let changed = false;
  const mapped: Json = {};
  for (const [key, entry] of Object.entries(value)) {
    const next = normalizeOne(entry);
    changed ||= !Object.is(next, entry);
    mapped[key] = next;
  }
  return changed ? mapped : value;
}

/** A router's `routes` is a list; a HITL node's is an `{ action: target }` mapping. */
const normalizeRoutes = (value: unknown): unknown => (Array.isArray(value) ? normalizeList(value) : normalizeValues(value));

/** Parallel `branches[]`: each branch owns an `id` and points at a `node`. */
function normalizeBranches(value: unknown): unknown {
  if (!Array.isArray(value)) return value;
  const mapped = value.map((branch) => patch(branch, { id: normalizeOne, node: normalizeOne }));
  return mapped.some((entry, index) => !Object.is(entry, value[index])) ? mapped : value;
}

type FieldRules = Readonly<Record<string, (value: unknown) => unknown>>;

/** Applies `rules` to the named fields of `record`; returns `record` itself when nothing changes. */
function patch(record: unknown, rules: FieldRules): unknown {
  if (!isRecord(record)) return record;
  let next: Json | undefined;
  for (const [field, normalize] of Object.entries(rules)) {
    if (!Object.hasOwn(record, field)) continue;
    const before = record[field];
    const after = normalize(before);
    if (Object.is(after, before)) continue;
    next ??= { ...record };
    next[field] = after;
  }
  return next ?? record;
}

const LEGACY_CONDITION_RULES: FieldRules = { conditional_outputs: normalizeList, default_output: normalizeOne };
const LEGACY_DECISION_RULES: FieldRules = { nodes: normalizeList, default_output: normalizeOne };

function normalizeNode(node: unknown): unknown {
  if (!isRecord(node)) return node;
  const base = patch(node, {
    id: normalizeOne,
    transition: normalizeOne,
    default_output: normalizeOne,
    nodes: normalizeList,
    routes: normalizeRoutes,
    condition: (value) => patch(value, LEGACY_CONDITION_RULES),
    decision: (value) => patch(value, LEGACY_DECISION_RULES),
  });
  // Type-gated: the editor reads these two only on the node types that own them.
  if (node['type'] === 'map') return patch(base, { worker: normalizeOne });
  if (node['type'] === 'parallel') return patch(base, { branches: normalizeBranches });
  return base;
}

function normalizeNodes(value: unknown): unknown {
  if (!Array.isArray(value)) return value;
  const mapped = value.map(normalizeNode);
  return mapped.some((entry, index) => !Object.is(entry, value[index])) ? mapped : value;
}

/**
 * Returns `document` with every integer graph identifier replaced by its
 * canonical decimal string: `entry_point`, `interrupt_before`/`interrupt_after`
 * entries, and for every node its `id`, `transition`, `default_output`,
 * decision `nodes`, router `routes` / HITL `routes` values, map `worker`,
 * parallel `branches[].id` / `.node`, and the legacy inline `condition` /
 * `decision` sub-objects.
 *
 * Pure and idempotent. Returns the same reference when nothing changes, and
 * never mutates its input. Anything that is not a mapping is returned as is.
 */
export function normalizePipelineNodeIdentifiers<T>(document: T): T {
  return patch(document, {
    entry_point: normalizeOne,
    interrupt_before: normalizeList,
    interrupt_after: normalizeList,
    nodes: normalizeNodes,
  }) as T;
}

/**
 * How an identifier that is NOT a legal one (a fraction, boolean, object,
 * unsafe integer ...) is named in validation messages. The text always
 * contains a space, so it can never be mistaken for a legal id or for a node
 * that happens to be called `true`.
 */
export function unsupportedIdentifierText(value: unknown): string {
  if (Array.isArray(value)) return 'list';
  if (value !== null && typeof value === 'object') return 'object';
  return `${typeof value} ${String(value)}`;
}
