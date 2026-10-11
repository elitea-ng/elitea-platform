import { normalizePipelineNodeIdentifiers } from '@/shared/lib/pipelineNodeIdentifiers';

import { graphParallelIssues } from './graphParallelAdmission.helpers';
/**
 * Pipeline **graph admission** — the editor's transcription of the Rust
 * pipeline compiler's own refusals. See `./graphAdmission.types.ts` for the
 * full "what and why"; this file holds the document/`state:` rules, the rule
 * catalogue, and the one entry point the UI calls.
 *
 * ## How to use it
 *
 * ```ts
 * const issues = collectGraphAdmissionIssues(yamlJsonObject);
 * if (issues.length > 0) { /* refuse the save, show issuesForNode(...) *\/ }
 * ```
 *
 * ## What it is NOT
 *
 * It is not a second authority and not a superset. Where the compiler
 * accepts something the editor finds odd — an empty Decision `nodes:` list,
 * a state key nobody reads — this module stays quiet, because refusing a
 * save the runtime would have accepted is its own defect. Every rule below
 * names the runtime `file:line` it mirrors; that citation is the diff to
 * re-check when `services/elitea-worker-rust/src/agents/graph/` changes.
 */
import { graphShapingIssues } from './graphShapingAdmission.helpers';
import { graphMapIssues } from './graphMapAdmission.helpers';
import { RuntimeContractConstants } from './flow-editor/constants';
import type { YamlPipelineDocument } from './flow-editor/helpers/pipelineFlow.types';
import { isValidGraphId, isValidOutputKey } from './graphAdmission.nodeReads';
import { NODE_ADMISSION_RULES } from './graphAdmission.nodes';
import type { AdmissionGraph, AdmissionNode, GraphAdmissionIssue, GraphAdmissionRule } from './graphAdmission.types';
import { admissionIssue, readNodeIdentity } from './graphAdmission.types';

const { isReservedStateKey, TYPED_STATE_REDUCERS_ADMITTED } = RuntimeContractConstants;

/** `compiler.rs:51` — `MAX_PIPELINE_NODES`. */
const MAX_PIPELINE_NODES = 128;

/** `compiler.rs:52` — `MAX_PIPELINE_STATE_KEYS`. */
const MAX_PIPELINE_STATE_KEYS = 256;

/** `compiler.rs:56, 187-217` bounds each authored pause list. */
const MAX_STATIC_INTERRUPTS = 128;

/** `compiler.rs:1378-1390` — every state type the compiler normalises, mapped to the normalised name it becomes. */
const STATE_TYPE_ALIASES: ReadonlyMap<string, string> = new Map([
  ['str', 'str'],
  ['string', 'str'],
  ['int', 'int'],
  ['number', 'int'],
  ['float', 'float'],
  ['bool', 'bool'],
  ['list', 'list'],
  ['dict', 'dict'],
]);

/** `compiler.rs:1391-1394` — the two built-in keys whose declared type is pinned. */
const PINNED_BUILTIN_STATE_TYPES: ReadonlyMap<string, string> = new Map([
  ['input', 'str'],
  ['messages', 'list'],
]);

/** The declared type name of one `state:` entry — a bare string, or a `{type, value}` descriptor (`compiler.rs:100-114`). */
function readStateTypeName(spec: unknown): string | undefined {
  if (typeof spec === 'string') return spec;
  if (spec !== null && typeof spec === 'object' && 'type' in spec) {
    const declared = (spec as { readonly type?: unknown }).type;
    return typeof declared === 'string' ? declared : undefined;
  }
  return undefined;
}

/** `state_reducers.rs` `StateReducer::parse` — each reducer and the one normalised type it accepts (`overwrite`: any). */
const STATE_REDUCER_TYPES: ReadonlyMap<string, string | undefined> = new Map([
  ['overwrite', undefined],
  ['append', 'list'],
  ['sum_int', 'int'],
  ['merge', 'dict'],
]);

/** The `reducer` of one `{type, value, reducer}` descriptor. A bare type name and `reducer: null` have none (`compiler.rs:155`). */
function readStateReducer(spec: unknown): unknown {
  if (spec === null || typeof spec !== 'object' || !('reducer' in spec)) return undefined;
  const declared = (spec as { readonly reducer?: unknown }).reducer;
  return declared ?? undefined;
}

/** Normalise a document into the lookup sets every rule reads. Runs once per collection pass. */
function readAdmissionGraph(document: YamlPipelineDocument | undefined): AdmissionGraph {
  const safeDocument: YamlPipelineDocument = document ?? {};
  const nodes: readonly AdmissionNode[] = (safeDocument.nodes ?? []).map((raw) => ({ ...readNodeIdentity(raw), raw }));
  const stateEntries = Object.entries(safeDocument.state ?? {});
  return {
    document: safeDocument,
    nodes,
    nodeIds: new Set(nodes.map((node) => node.id)),
    stateKeys: new Set(stateEntries.map(([key]) => key)),
    stateTypes: new Map(stateEntries.map(([key, spec]) => [key, STATE_TYPE_ALIASES.get(readStateTypeName(spec) ?? '') ?? ''])),
  };
}

const nodeCountRule: GraphAdmissionRule = {
  id: 'document.node-count',
  citation: 'compiler.rs:459-463',
  summary: 'a pipeline holds between 1 and 128 nodes',
  check: (graph) => {
    const count = graph.nodes.length;
    if (count >= 1 && count <= MAX_PIPELINE_NODES) return [];
    return [
      admissionIssue(
        'document.node-count',
        'compiler.rs:459',
        undefined,
        'nodes',
        String(count),
        `nodes: a pipeline must hold between 1 and ${String(MAX_PIPELINE_NODES)} nodes — this one holds ${String(count)}.`,
      ),
    ];
  },
};

const entryPointRule: GraphAdmissionRule = {
  id: 'document.entry-point',
  citation: 'compiler.rs:464-468, 477-481',
  summary: '`entry_point` is a well-formed id and names a declared node',
  check: (graph) => entryPointIssues(graph),
};

function entryPointIssues(graph: AdmissionGraph): readonly GraphAdmissionIssue[] {
  const entryPoint = graph.document.entry_point;
  if (typeof entryPoint !== 'string' || !isValidGraphId(entryPoint)) {
    return [
      admissionIssue(
        'document.entry-point',
        'compiler.rs:464',
        undefined,
        'entry_point',
        typeof entryPoint === 'string' ? entryPoint : '',
        'entry_point: a pipeline must name the node it starts at, as a legal node id.',
      ),
    ];
  }
  if (!graph.nodeIds.has(entryPoint)) {
    return [
      admissionIssue('document.entry-point', 'compiler.rs:477', undefined, 'entry_point', entryPoint, `entry_point: "${entryPoint}" does not name any node in this pipeline.`),
    ];
  }
  return [];
}

/** `compiler.rs:187-217, 620-621, 1971-1984` validates authored pause lists. */
const staticInterruptRule: GraphAdmissionRule = {
  id: 'document.static-interrupts',
  citation: 'compiler.rs:187-217, 1971-1984',
  summary: 'each pause list contains at most 128 unique stored node identifiers',
  check: (graph) => (['interrupt_before', 'interrupt_after'] as const).flatMap((field) => staticInterruptIssues(graph, field)),
};

function staticInterruptIssues(graph: AdmissionGraph, field: 'interrupt_before' | 'interrupt_after'): readonly GraphAdmissionIssue[] {
  const value: unknown = graph.document[field];
  if (value === undefined) return [];
  if (!Array.isArray(value)) {
    return [admissionIssue('document.static-interrupts', 'compiler.rs:217', undefined, field, '', `${field}: declare a list of stored node identifiers.`)];
  }
  const entries: readonly unknown[] = value;
  if (entries.length > MAX_STATIC_INTERRUPTS) {
    return [admissionIssue('document.static-interrupts', 'compiler.rs:204', undefined, field, String(entries.length), `${field}: declare at most ${String(MAX_STATIC_INTERRUPTS)} node identifiers.`)];
  }
  const seen = new Set<string>();
  return entries.flatMap((nodeId, index) => {
    const entryField = `${field}[${String(index)}]`;
    if (typeof nodeId !== 'string' || !isValidGraphId(nodeId)) {
      return [admissionIssue('document.static-interrupts', 'compiler.rs:1978', undefined, entryField, typeof nodeId === 'string' ? nodeId : '', `${entryField}: use a legal node identifier.`)];
    }
    if (!graph.nodeIds.has(nodeId)) {
      return [admissionIssue('document.static-interrupts', 'compiler.rs:1978', undefined, entryField, nodeId, `${entryField}: "${nodeId}" does not name any stored node in this pipeline.`)];
    }
    if (seen.has(nodeId)) {
      return [admissionIssue('document.static-interrupts', 'compiler.rs:1978', undefined, entryField, nodeId, `${entryField}: "${nodeId}" already appears in ${field}.`)];
    }
    seen.add(nodeId);
    return [];
  });
}

const stateKeyRule: GraphAdmissionRule = {
  id: 'state.key',
  citation: 'compiler.rs:1358, 1373-1377 (yaml.rs:371, compiler.rs:1456)',
  summary: 'state keys are well-formed, not reserved, and within the 256-key bound',
  check: (graph) => stateKeyIssues(graph),
};

function stateKeyIssues(graph: AdmissionGraph): readonly GraphAdmissionIssue[] {
  const keys = [...graph.stateKeys];
  const overBound =
    keys.length > MAX_PIPELINE_STATE_KEYS
      ? [
          admissionIssue(
            'state.key',
            'compiler.rs:1358',
            undefined,
            'state',
            String(keys.length),
            `state: at most ${String(MAX_PIPELINE_STATE_KEYS)} variables — this pipeline declares ${String(keys.length)}.`,
          ),
        ]
      : [];
  return [...overBound, ...keys.flatMap((key) => stateKeyIssue(key))];
}

function stateKeyIssue(key: string): readonly GraphAdmissionIssue[] {
  if (!isValidOutputKey(key)) {
    return [admissionIssue('state.key', 'compiler.rs:1373', undefined, `state.${key}`, key, `state: "${key}" is not a legal variable name.`)];
  }
  if (isReservedStateKey(key)) {
    return [admissionIssue('state.key', 'compiler.rs:1373', undefined, `state.${key}`, key, `state: "${key}" is reserved by the runtime and cannot be declared here.`)];
  }
  return [];
}

const stateTypeRule: GraphAdmissionRule = {
  id: 'state.type',
  citation: 'compiler.rs:1370-1372, 1378-1390',
  summary: 'every state variable declares one of the six admitted types',
  check: (graph) =>
    [...graph.stateTypes.entries()]
      .filter(([, normalised]) => normalised === '')
      .map(([key]) =>
        admissionIssue(
          'state.type',
          'compiler.rs:1386',
          undefined,
          `state.${key}`,
          readStateTypeName(graph.document.state?.[key]) ?? '',
          `state.${key}: its type must be one of str, int, float, bool, list or dict.`,
        ),
      ),
};

const builtinStateTypeRule: GraphAdmissionRule = {
  id: 'state.builtin-type',
  citation: 'compiler.rs:1391-1395',
  summary: '`input` is declared `str` and `messages` is declared `list`',
  check: (graph) =>
    [...PINNED_BUILTIN_STATE_TYPES.entries()]
      .filter(([key, pinned]) => graph.stateTypes.has(key) && graph.stateTypes.get(key) !== pinned)
      .map(([key, pinned]) =>
        admissionIssue('state.builtin-type', 'compiler.rs:1391', undefined, `state.${key}`, graph.stateTypes.get(key) ?? '', `state.${key}: this built-in variable must be declared "${pinned}".`),
      ),
};

/**
 * A declared reducer is kept exactly as written: Save stores the YAML text and
 * a canvas edit spreads the descriptor (`stateVariableSpec.helpers.ts`). The
 * canvas cannot show or edit it yet, so this notice says where it lives.
 * Whether the runtime admits it is the compiler's call (`compiler.rs:2774`).
 */
const stateReducerRule: GraphAdmissionRule = {
  id: 'state.reducer',
  citation: 'compiler.rs:2774',
  summary: 'a declared state reducer is admitted only where the runtime admits it, and is then a YAML-only notice',
  check: (graph) => Object.entries(graph.document.state ?? {}).flatMap(([key, spec]) => stateReducerIssues(graph, key, readStateReducer(spec))),
};

/** `compiler.rs:2729, 2774-2808` (state type parse, `typed_reducer`), in the compiler's order; a valid reducer becomes a non-blocking notice. */
function stateReducerIssues(graph: AdmissionGraph, key: string, reducer: unknown): readonly GraphAdmissionIssue[] {
  if (reducer === undefined) return [];
  const field = `state.${key}`;
  const subject = typeof reducer === 'string' ? reducer : '';
  const refuse = (line: string, message: string): readonly GraphAdmissionIssue[] => [admissionIssue('state.reducer', `compiler.rs:${line}`, undefined, field, subject, message)];
  if (typeof reducer !== 'string') return refuse('2729', `${field}: reducer must be one of overwrite, append, sum_int or merge.`);
  if (!TYPED_STATE_REDUCERS_ADMITTED) return refuse('2784', `${field}: the "${reducer}" reducer is not available on this deployment — the whole pipeline is refused.`);
  if (PINNED_BUILTIN_STATE_TYPES.has(key)) return refuse('2789', `${field}: this built-in variable cannot declare a reducer.`);
  if (!STATE_REDUCER_TYPES.has(reducer)) return refuse('2794', `${field}: reducer must be one of overwrite, append, sum_int or merge.`);
  const required = STATE_REDUCER_TYPES.get(reducer);
  if (required !== undefined && graph.stateTypes.get(key) !== required) return refuse('2800', `${field}: the "${reducer}" reducer needs a "${required}" variable.`);
  const notice = admissionIssue(
    'state.reducer',
    'compiler.rs:2774',
    undefined,
    field,
    reducer,
    `${field}: the "${reducer}" reducer can be changed in YAML only for now. The visual editor keeps it as written.`,
  );
  return [{ ...notice, severity: 'warning' as const }];
}

/**
 * Every admission rule, document-level first, then per-node — the order the
 * compiler itself hits them (`from_raw` before `parse_pipeline_nodes`).
 * Exported so the unit suite can assert one case per rule id and so nothing
 * here is reachable only from a test.
 */
export const GRAPH_ADMISSION_RULES: readonly GraphAdmissionRule[] = [
  nodeCountRule,
  entryPointRule,
  staticInterruptRule,
  stateKeyRule,
  stateTypeRule,
  builtinStateTypeRule,
  stateReducerRule,
  ...NODE_ADMISSION_RULES,
];

/** Every reason the Rust pipeline compiler would refuse `document`. Empty means "admissible". */
export function collectGraphAdmissionIssues(rawDocument: YamlPipelineDocument | undefined): readonly GraphAdmissionIssue[] {
  const graph = readAdmissionGraph(normalizePipelineNodeIdentifiers(rawDocument));
  return [...GRAPH_ADMISSION_RULES.flatMap((rule) => rule.check(graph)), ...graphShapingIssues(graph.document), ...graphMapIssues(graph.document), ...graphParallelIssues(graph.document)];
}

/** The issues that refuse the document; a `'warning'` notice never blocks a save. */
export function blockingIssues(issues: readonly GraphAdmissionIssue[]): readonly GraphAdmissionIssue[] {
  return issues.filter((issue) => issue.severity !== 'warning');
}

/** The issues a given node's panel should show. */
export function issuesForNode(issues: readonly GraphAdmissionIssue[], nodeId: string): readonly GraphAdmissionIssue[] {
  return issues.filter((issue) => issue.nodeId === nodeId);
}

/** The issues that belong to the document rather than to any one node (`entry_point`, `state:`, interrupts). */
export function documentLevelIssues(issues: readonly GraphAdmissionIssue[]): readonly GraphAdmissionIssue[] {
  return issues.filter((issue) => issue.nodeId === undefined);
}
