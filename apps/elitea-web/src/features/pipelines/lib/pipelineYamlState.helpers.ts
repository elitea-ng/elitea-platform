import { CORE_SCHEMA, eventsToAst, loadAll, parseEvents, visit } from 'js-yaml';
import type { Node } from 'js-yaml';

const STRING_TAG = 'tag:yaml.org,2002:str';

/** Read declaration order before plain-object enumeration changes numeric keys. */
export function readPipelineStateOrder(source: string): string[] {
  if (!source.trim()) return [];
  const documents = eventsToAst(parseEvents(source, {}), {
    source,
    schema: CORE_SCHEMA,
  });
  if (documents.length === 0) return [];
  if (documents.length !== 1) throw new Error('Expected one pipeline YAML document');
  const root = documents[0]?.contents;
  if (root?.kind !== 'mapping') return [];
  const state = root.items.find(({ key }) => key.kind === 'scalar' && key.value === 'state')?.value;
  if (!state) return [];
  const anchors = new Map<string, Node>();
  visit(documents, (node) => {
    if (node.kind !== 'alias' && node.anchor) anchors.set(node.anchor, node);
  });
  const resolved = resolveAlias(state, anchors);
  if (resolved.kind !== 'mapping') return [];
  return resolved.items.map(({ key }) => {
    if (key.kind !== 'scalar' || key.tag !== STRING_TAG) {
      throw new Error('Pipeline state keys must be strings');
    }
    return key.value;
  });
}

function resolveAlias(node: Node, anchors: ReadonlyMap<string, Node>): Node {
  const seen = new Set<string>();
  while (node.kind === 'alias') {
    if (seen.has(node.anchor)) throw new Error('Invalid state mapping alias');
    seen.add(node.anchor);
    const target = anchors.get(node.anchor);
    if (!target) throw new Error('Unknown state mapping alias');
    node = target;
  }
  return node;
}

/** Keep surviving declarations in place. Append only newly added roots. */
export function reconcilePipelineStateOrder(value: unknown, previousOrder: readonly string[]): string[] {
  const keys = stateKeys(value);
  const declared = new Set(keys);
  const preserved = previousOrder.filter((key) => declared.has(key));
  const retained = new Set(preserved);
  return [...preserved, ...keys.filter((key) => !retained.has(key))];
}

/** Require one entry for each declared root when a caller supplies explicit order. */
export function validatePipelineStateOrder(value: unknown, order: readonly string[]): void {
  const keys = stateKeys(value);
  const declared = new Set(keys);
  if (order.length !== keys.length || new Set(order).size !== order.length || order.some((key) => !declared.has(key))) {
    throw new Error('Pipeline state declaration order does not match its roots');
  }
}

/** Preserve order explicitly when a caller renames a root. */
export function renamePipelineStateOrder(order: readonly string[], name: string, newName: string): string[] {
  if (!order.includes(name) || (name !== newName && order.includes(newName))) {
    throw new Error('Invalid pipeline state rename');
  }
  return order.map((key) => (key === name ? newName : key));
}

function stateKeys(value: unknown): string[] {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return [];
  const state = (value as Record<string, unknown>)['state'];
  return state !== null && typeof state === 'object' && !Array.isArray(state) ? Object.keys(state) : [];
}

/** Compare semantic values and the compiler-owned state declaration sequence. */
export function pipelineValueFingerprint(value: unknown, stateOrder: readonly string[]): string {
  return JSON.stringify({ document: canonicalYamlValue(value, new Set()), stateOrder });
}

export function pipelineYamlFingerprint(source: string): string {
  const documents = loadAll(source);
  if (documents.length > 1) throw new Error('Expected one pipeline YAML document');
  return pipelineValueFingerprint(documents[0], readPipelineStateOrder(source));
}

/** Typed tokens keep undefined, non-finite numbers, dates and binary distinct from JSON null/objects. */
function canonicalYamlValue(value: unknown, ancestors: Set<object>): unknown {
  if (value === null) return ['null'];
  switch (typeof value) {
    case 'undefined':
      return ['undefined'];
    case 'string':
      return ['string', value];
    case 'boolean':
      return ['boolean', value];
    case 'number':
      return ['number', Object.is(value, -0) ? '-0' : String(value)];
    case 'object':
      return canonicalYamlObject(value, ancestors);
    case 'bigint':
    case 'function':
    case 'symbol':
      throw new Error('Unsupported pipeline YAML value');
  }
}

function canonicalYamlObject(value: object, ancestors: Set<object>): unknown {
  if (value instanceof Date) return ['date', value.toISOString()];
  if (value instanceof Uint8Array) return ['binary', Array.from(value)];
  const prototype: unknown = Object.getPrototypeOf(value);
  if (!Array.isArray(value) && prototype !== Object.prototype && prototype !== null)
    throw new Error('Unsupported pipeline YAML object');
  if (ancestors.has(value)) throw new Error('Cyclic pipeline YAML value');
  ancestors.add(value);
  const result = Array.isArray(value)
    ? ['sequence', value.map((entry) => canonicalYamlValue(entry, ancestors))]
    : [
        'mapping',
        Object.entries(value)
          .sort(([a], [b]) => a.localeCompare(b))
          .map(([key, entry]) => [key, canonicalYamlValue(entry, ancestors)]),
      ];
  ancestors.delete(value);
  return result;
}
