import type { YamlPipelineDocument, YamlPipelineNode } from './flow-editor/helpers/pipelineFlow.types';
import { admissionIssue, type GraphAdmissionIssue } from './graphAdmission.types';
import { fixedParallelWorkerReferences } from './graphExtensionReferenceRename.helpers';

/**
 * Per-node `settings:` (5b-1). The single source is
 * `libs/proto/contracts/pipeline-node-settings-v1.json`; the Worker compiler
 * (`node_settings_definition.rs`) and Main's save check (`pipelinelimits/node_settings.go`)
 * read the same file and refuse with the same text, in the same order.
 */
export const NODE_SETTINGS_KEY = 'settings';
export const NODE_ERRORS_KEY = 'node_errors';
export const EXECUTED_ONCE_KEY = '__elitea_executed_once_v1';

export const NodeSettingsLimits = {
  min_node_tries: 2,
  max_node_tries: 5,
  min_wait_between_tries_ms: 100,
  max_wait_between_tries_ms: 300_000,
  default_wait_between_tries_ms: 1_000,
  min_node_timeout_ms: 1_000,
  max_node_timeout_ms: 900_000,
  max_node_errors: 64,
  max_node_error_entry_bytes: 1_024,
  max_node_error_object_bytes: 512,
  max_executed_once_nodes: 128,
} as const;

export const NODE_ON_ERROR = ['stop', 'continue', 'error_output'] as const;
export type NodeOnError = (typeof NODE_ON_ERROR)[number];

export const NODE_SETTINGS_KEYS = [
  'retry_on_fail', 'max_tries', 'wait_between_tries_ms', 'on_error', 'error_transition', 'always_output_data', 'execute_once', 'timeout_ms',
] as const;

const DETERMINISTIC_KEYS = ['on_error', 'error_transition', 'always_output_data', 'execute_once'] as const;

/** The node types that take a `settings:` block, and the keys each admits (contract `families`). */
export const NodeSettingsFamilies: Readonly<Record<string, readonly string[]>> = {
  aggregate: DETERMINISTIC_KEYS,
  router: DETERMINISTIC_KEYS,
  split_out: DETERMINISTIC_KEYS,
  state_modifier: DETERMINISTIC_KEYS,
};

/** Pauses never take settings. */
export const NodeSettingsPauseFamilies: readonly string[] = ['hitl', 'printer'];

/** The contract's readable refusal texts, by key. */
export const NodeSettingsRefusalTexts = {
  malformed: 'Its settings are malformed: use a mapping of the known settings with plain values.',
  family_not_yet: 'Settings are not available for this node type yet.',
  family_pause: 'Settings are not available for a node that pauses the run.',
  fanout_owned: 'Settings are not available for a node inside a Parallel or Map yet.',
  with_recovery: 'Settings cannot be combined with recovery or failure_handler on one node.',
  retry_not_allowed: 'Retry on fail is not available for this node type.',
  timeout_not_allowed: 'Timeout is not available for this node type.',
  max_tries_range: 'Max tries must be between 2 and 5.',
  wait_range: 'Wait between tries must be between 100 and 300000 ms.',
  timeout_range: 'Timeout must be between 1000 and 900000 ms.',
  retry_fields_need_retry: 'Max tries and wait between tries need Retry on fail.',
  error_transition_required: "On error 'error output' needs an error transition.",
  error_transition_unexpected: "An error transition needs On error 'error output'.",
  error_transition_target: 'The error transition must name END or another node of this pipeline outside any Parallel or Map.',
  always_output_reducer: 'Always output data cannot write an output that uses the sum_int or merge reducer.',
  router_continue_needs_default: "On error 'continue' on a Router needs an explicit default output.",
  node_errors_type: 'When a node continues on error, a declared node_errors must be a list with reducer: append.',
  node_errors_nested_mapping: 'An Agent node cannot map node_errors into or out of a nested pipeline.',
} as const;

export type NodeSettingsRefusal = keyof typeof NodeSettingsRefusalTexts;

/** One parsed block; every field defaults to today's behaviour. */
export interface NodeSettings {
  readonly retry_on_fail: boolean;
  readonly max_tries?: number | undefined;
  readonly wait_between_tries_ms?: number | undefined;
  readonly on_error: NodeOnError;
  readonly error_transition?: string | undefined;
  readonly always_output_data: boolean;
  readonly execute_once: boolean;
  readonly timeout_ms?: number | undefined;
}

const GRAPH_ID = /^[A-Za-z0-9_.:-]{1,128}$/;

const isRecord = (value: unknown): value is Readonly<Record<string, unknown>> =>
  typeof value === 'object' && value !== null && !Array.isArray(value);
/** Any integer within +/-(2^53-1): out-of-range values are range refusals, as in the Worker and Main. */
const isCount = (value: unknown): value is number => typeof value === 'number' && Number.isSafeInteger(value);

const BOOLEAN_KEYS = ['retry_on_fail', 'always_output_data', 'execute_once'];
const COUNT_KEYS = ['max_tries', 'wait_between_tries_ms', 'timeout_ms'];
/** Like the Worker's `Option`s, null is "absent" for the optional fields only. */
const NULLABLE_KEYS = [...COUNT_KEYS, 'error_transition'];

function validSettingValue(key: string, value: unknown): boolean {
  if (!(NODE_SETTINGS_KEYS as readonly string[]).includes(key)) return false;
  if (value === null || value === undefined) return NULLABLE_KEYS.includes(key);
  if (BOOLEAN_KEYS.includes(key)) return typeof value === 'boolean';
  if (COUNT_KEYS.includes(key)) return isCount(value);
  if (key === 'on_error') return (NODE_ON_ERROR as readonly unknown[]).includes(value);
  return typeof value === 'string';
}

function readSettings(block: Readonly<Record<string, unknown>>): NodeSettings {
  const read = (key: string): unknown => block[key] ?? undefined;
  return {
    retry_on_fail: read('retry_on_fail') === true,
    max_tries: read('max_tries') as number | undefined,
    wait_between_tries_ms: read('wait_between_tries_ms') as number | undefined,
    on_error: (read('on_error') as NodeOnError | undefined) ?? 'stop',
    error_transition: read('error_transition') as string | undefined,
    always_output_data: read('always_output_data') === true,
    execute_once: read('execute_once') === true,
    timeout_ms: read('timeout_ms') as number | undefined,
  };
}

const outside = (value: number | undefined, low: number, high: number): boolean => value !== undefined && (value < low || value > high);

/** The bounds and couplings, in the Worker's order (first match wins). */
const SETTINGS_CHECKS: readonly (readonly [(settings: NodeSettings) => boolean, NodeSettingsRefusal])[] = [
  [(s) => outside(s.max_tries, NodeSettingsLimits.min_node_tries, NodeSettingsLimits.max_node_tries), 'max_tries_range'],
  [(s) => outside(s.wait_between_tries_ms, NodeSettingsLimits.min_wait_between_tries_ms, NodeSettingsLimits.max_wait_between_tries_ms), 'wait_range'],
  [(s) => outside(s.timeout_ms, NodeSettingsLimits.min_node_timeout_ms, NodeSettingsLimits.max_node_timeout_ms), 'timeout_range'],
  [(s) => !s.retry_on_fail && (s.max_tries !== undefined || s.wait_between_tries_ms !== undefined), 'retry_fields_need_retry'],
  [(s) => s.on_error === 'error_output' && s.error_transition === undefined, 'error_transition_required'],
  [(s) => s.on_error !== 'error_output' && s.error_transition !== undefined, 'error_transition_unexpected'],
  [(s) => s.error_transition !== undefined && s.error_transition !== 'END' && !GRAPH_ID.test(s.error_transition), 'error_transition_target'],
];

/** Mirrors `NodeSettings::parse`: strict keys, plain values, the bounds and the couplings. */
export function parseNodeSettings(block: unknown): { settings?: NodeSettings; refusal?: NodeSettingsRefusal } {
  if (!isRecord(block) || Object.entries(block).some(([key, value]) => !validSettingValue(key, value))) return { refusal: 'malformed' };
  const settings = readSettings(block);
  const refused = SETTINGS_CHECKS.find(([check]) => check(settings));
  return refused === undefined ? { settings } : { refusal: refused[1] };
}

/**
 * The Worker's `normalize_legacy_label`: a value that is not a graph id (a
 * legacy UI label such as `My Handler`) keeps only ASCII letters, digits,
 * `_`, `-` and `.`, with `.` as `_`; a valid id, an empty value, one over 128
 * bytes, or one that would become empty or too long is kept as written.
 */
export function legacyGraphId(value: string): string {
  const bytes = new TextEncoder().encode(value);
  if (GRAPH_ID.test(value) || value === '' || bytes.length > 128) return value;
  let out = '';
  for (const byte of bytes) {
    const char = String.fromCharCode(byte);
    if (char === '.') out += '_';
    else if (/^[A-Za-z0-9_-]$/.test(char)) out += char;
  }
  return out === '' || out.length > 128 ? value : out;
}

const viewId = (value: unknown): unknown => (typeof value === 'string' ? legacyGraphId(value) : value);

/** The ids the settings check reads, as the Worker normalizes them (legacy labels). */
function settingsView(document: YamlPipelineDocument): YamlPipelineDocument {
  const nodes = (document.nodes ?? []).map((node) => {
    const settings = node[NODE_SETTINGS_KEY];
    const branches = node['branches'];
    const view: YamlPipelineNode = {
      ...node,
      id: legacyGraphId(node.id),
      ...(node['worker'] === undefined ? {} : { worker: viewId(node['worker']) }),
      ...(Array.isArray(branches) ? { branches: branches.map((branch: unknown) => (isRecord(branch) ? { ...branch, node: viewId(branch['node']) } : branch)) } : {}),
      ...(isRecord(settings) && settings['error_transition'] !== undefined
        ? { [NODE_SETTINGS_KEY]: { ...settings, error_transition: viewId(settings['error_transition']) } }
        : {}),
    };
    return view;
  });
  return { ...document, nodes };
}

/** `Node <id>: <text>`, or the text alone for the document-level refusal. */
export function nodeSettingsMessage(refusal: NodeSettingsRefusal, nodeId: string | undefined): string {
  const text = NodeSettingsRefusalTexts[refusal];
  if (nodeId === undefined) return text;
  return `${GRAPH_ID.test(nodeId) ? `Node ${nodeId}` : 'A pipeline node'}: ${text}`;
}

const usesNodeErrors = (settings: NodeSettings): boolean => settings.on_error !== 'stop';

/** Whether some node's settings declare the `node_errors` channel (D2): only then is it a declared list. */
export function declaresNodeErrors(document: YamlPipelineDocument | undefined): boolean {
  return (document?.nodes ?? []).some((node) => {
    const parsed = parseNodeSettings(node[NODE_SETTINGS_KEY]);
    return node[NODE_SETTINGS_KEY] !== undefined && parsed.settings !== undefined && usesNodeErrors(parsed.settings);
  });
}

function stateSpec(document: YamlPipelineDocument, key: string): { type?: string | undefined; reducer?: string | undefined } | undefined {
  if (!Object.hasOwn(document.state ?? {}, key)) return undefined;
  const spec = document.state?.[key];
  if (typeof spec === 'string') return { type: spec };
  if (!isRecord(spec)) return {};
  return { type: typeof spec['type'] === 'string' ? spec['type'] : undefined, reducer: typeof spec['reducer'] === 'string' ? spec['reducer'] : undefined };
}

const strings = (value: unknown): readonly string[] => (Array.isArray(value) ? value.filter((item): item is string => typeof item === 'string') : []);

interface Block {
  readonly node: YamlPipelineNode;
  readonly settings: NodeSettings;
}

const issue = (nodeId: string | undefined, refusal: NodeSettingsRefusal, field = NODE_SETTINGS_KEY): GraphAdmissionIssue =>
  admissionIssue('node.settings', 'node_settings_definition.rs', nodeId, field, refusal, nodeSettingsMessage(refusal, nodeId));

function parseBlocks(nodes: readonly YamlPipelineNode[]): { blocks: Block[]; refused?: GraphAdmissionIssue } {
  const blocks: Block[] = [];
  for (const node of nodes) {
    const block = node[NODE_SETTINGS_KEY];
    // Decision 1: a block with no keys is no block, on every family.
    if (block === undefined || (isRecord(block) && Object.keys(block).length === 0)) continue;
    const parsed = parseNodeSettings(block);
    if (parsed.settings === undefined) return { blocks, refused: issue(node.id, parsed.refusal ?? 'malformed') };
    blocks.push({ node, settings: parsed.settings });
  }
  return { blocks };
}

const reducerIn = (document: YamlPipelineDocument) => (key: string): string | undefined => {
  const reducer = stateSpec(document, key)?.reducer;
  return reducer === 'overwrite' ? undefined : reducer;
};

/** C3 and C4: only a document whose nodes continue on error owns `node_errors`. */
function nodeErrorsIssue(document: YamlPipelineDocument): GraphAdmissionIssue | undefined {
  const declared = stateSpec(document, NODE_ERRORS_KEY);
  // Decision 2: a user's own key must say `reducer: append` explicitly.
  if (declared !== undefined && (declared.type !== 'list' || reducerIn(document)(NODE_ERRORS_KEY) !== 'append')) {
    return issue(undefined, 'node_errors_type', `state.${NODE_ERRORS_KEY}`);
  }
  const mapping = (document.nodes ?? []).find((node) => node.type === 'agent'
    && ((isRecord(node['input_mapping']) && Object.hasOwn(node['input_mapping'], NODE_ERRORS_KEY)) || strings(node.output).includes(NODE_ERRORS_KEY)));
  return mapping === undefined ? undefined : issue(mapping.id, 'node_errors_nested_mapping', 'input_mapping');
}

function fanoutOwnedNodes(nodes: readonly YamlPipelineNode[]): ReadonlySet<string> {
  const owned = new Set<string>();
  for (const node of nodes) {
    if (node.type === 'map' && typeof node['worker'] === 'string') owned.add(node['worker']);
    if (node.type === 'parallel') fixedParallelWorkerReferences(node['branches']).forEach((target) => owned.add(target));
  }
  return owned;
}

/**
 * Every `settings:` refusal, in the Worker's order: each block's shape and
 * bounds (document order), the `node_errors` declaration (C3), the nested
 * mapping (C4), then each node by id. Like the compiler, the first refusal
 * stops the document; one issue keeps the three layers' messages identical.
 */
export function graphNodeSettingsIssues(rawDocument: YamlPipelineDocument): readonly GraphAdmissionIssue[] {
  const document = settingsView(rawDocument);
  const nodes = document.nodes ?? [];
  const { blocks, refused } = parseBlocks(nodes);
  if (refused !== undefined) return [refused];
  if (blocks.length === 0) return [];
  const owner = blocks.some((block) => usesNodeErrors(block.settings)) ? nodeErrorsIssue(document) : undefined;
  if (owner !== undefined) return [owner];
  const context: NodeContext = { ids: new Set(nodes.map((node) => node.id)), owned: fanoutOwnedNodes(nodes), reducerOf: reducerIn(document) };
  const sorted = [...blocks].sort((left, right) => (left.node.id < right.node.id ? -1 : left.node.id > right.node.id ? 1 : 0));
  for (const block of sorted) {
    const refusal = NODE_CHECKS.find(([check]) => check(block, context));
    if (refusal !== undefined) return [issue(block.node.id, refusal[1])];
  }
  return [];
}

interface NodeContext {
  readonly ids: ReadonlySet<string>;
  readonly owned: ReadonlySet<string>;
  readonly reducerOf: (key: string) => string | undefined;
}

const familyKeys = (node: YamlPipelineNode): readonly string[] | undefined => NodeSettingsFamilies[node.type ?? ''];

function badErrorTarget({ node, settings }: Block, { ids, owned }: NodeContext): boolean {
  const target = settings.error_transition;
  return target !== undefined && target !== 'END' && (target === node.id || owned.has(target) || !ids.has(target));
}

/** The per-node admission, in the Worker's order (first match wins). */
const NODE_CHECKS: readonly (readonly [(block: Block, context: NodeContext) => boolean, NodeSettingsRefusal])[] = [
  [({ node }) => node['recovery'] !== undefined || node['failure_handler'] !== undefined, 'with_recovery'],
  [({ node }) => NodeSettingsPauseFamilies.includes(node.type ?? ''), 'family_pause'],
  [({ node }) => familyKeys(node) === undefined, 'family_not_yet'],
  [({ node, settings }) => settings.retry_on_fail && !(familyKeys(node) ?? []).includes('retry_on_fail'), 'retry_not_allowed'],
  [({ node, settings }) => settings.timeout_ms !== undefined && !(familyKeys(node) ?? []).includes('timeout_ms'), 'timeout_not_allowed'],
  [({ node }, { owned }) => owned.has(node.id), 'fanout_owned'],
  [badErrorTarget, 'error_transition_target'],
  // C5: the raw key, never the runtime's END default.
  [({ node, settings }) => node.type === 'router' && settings.on_error === 'continue' && node.default_output === undefined, 'router_continue_needs_default'],
  [({ node, settings }, { reducerOf }) => settings.always_output_data && strings(node.output).some((key) => ['sum_int', 'merge'].includes(reducerOf(key) ?? '')),
    'always_output_reducer'],
];

/** The node's parsed settings for the panel; defaults when the block is absent or invalid. */
export function readNodeSettings(node: YamlPipelineNode | undefined): NodeSettings {
  return parseNodeSettings(node?.[NODE_SETTINGS_KEY] ?? {}).settings
    ?? { retry_on_fail: false, on_error: 'stop', always_output_data: false, execute_once: false };
}

/**
 * The block a panel change writes: defaults are dropped, so a node whose
 * settings are all default carries no block at all (byte-identical YAML and
 * digest). `error_transition` survives only with `error_output`.
 */
export function writeNodeSettings(current: Readonly<Record<string, unknown>> | undefined, change: Readonly<Record<string, unknown>>): Readonly<Record<string, unknown>> | undefined {
  const next: Record<string, unknown> = { ...(isRecord(current) ? current : {}), ...change };
  if (next['on_error'] !== 'error_output') delete next['error_transition'];
  for (const [key, value] of Object.entries(next)) {
    if (value === undefined || value === false || (key === 'on_error' && value === 'stop')) delete next[key];
  }
  return Object.keys(next).length === 0 ? undefined : next;
}
