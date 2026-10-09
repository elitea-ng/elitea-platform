/**
 * How the compiler READS one stored node: which fields each family
 * contributes to route checking and state-key checking, plus the three
 * grammar predicates (`valid_graph_id`, `valid_output_key`,
 * `valid_tool_identity`) the rules in `./graphAdmission.nodes.ts` apply to
 * what comes back.
 *
 * Split out of `./graphAdmission.nodes.ts` for the §3.5 400-line file
 * budget only — nothing here decides anything, it only reports what a node
 * says.
 */
import { unsupportedIdentifierText } from '@/shared/lib/pipelineNodeIdentifiers';

import { RuntimeContractConstants } from './flow-editor/constants';
import type { YamlPipelineNode } from './flow-editor/helpers/pipelineFlow.types';
import type { AdmissionNode } from './graphAdmission.types';
import { toStringList } from './graphAdmission.types';

const { MAX_NODE_ID_BYTES, NODE_ID_PATTERN } = RuntimeContractConstants;

/** `compiler.rs:1220` / `compiler.rs:56` — the one node id the compiler owns for itself. */
export const RESERVED_NODE_ID = '__elitea_subgraph_result_v1';

/**
 * `compiler.rs:1436-1454` (`builtin_state_key`). A node may name any of
 * these as an input/output/clean key WITHOUT declaring it in `state:` —
 * the runtime supplies the channel. Note the asymmetry the HITL arm below
 * relies on: `edit_state_key` gets NO such escape (`compiler.rs:1344-1346`
 * calls `state.contains_key` bare).
 */
export const BUILTIN_STATE_KEYS: ReadonlySet<string> = new Set([
  'input',
  'messages',
  'output',
  'result',
  'router_output',
  'elitea_response',
  'printer_output',
  'state_types',
  'context_info',
  'hitl_decisions',
  'hitl_interrupt',
  'parallel_tasks',
  '_pipeline_blocked',
  'session_id',
]);

/**
 * Families whose `input:` list the compiler reads VERBATIM as its
 * `input_keys()` — `compiler.rs:371-382`. Printer contributes none
 * (`compiler.rs:378`), and `decision` is deliberately NOT a member: its
 * `input_keys()` is a resolved list, not `raw.input`. See
 * `decisionInputKeys` below.
 */
const READS_INPUT: ReadonlySet<string> = new Set(['agent', 'toolkit', 'mcp', 'hitl', 'llm', 'router', 'state_modifier']);

/** Families whose `output:` list the compiler reads — `compiler.rs:384-392`. */
const READS_OUTPUT: ReadonlySet<string> = new Set(['agent', 'toolkit', 'mcp', 'llm', 'state_modifier']);

/** Families whose `input_mapping:` values can name a state variable — `compiler.rs:1302-1338`. */
const READS_INPUT_MAPPING: ReadonlySet<string> = new Set(['agent', 'toolkit', 'mcp', 'llm', 'printer']);

/**
 * `yaml.rs:371-378` (`valid_output_key`) — non-empty, ≤256 bytes, no NUL/CR/LF.
 *
 * The three forbidden bytes are tested with `includes` rather than a
 * character class: a regex literal holding them trips `no-control-regex`,
 * and spelling them as `\u0000` escapes would not change that.
 */
export function isValidOutputKey(value: string): boolean {
  return value.length > 0 && value.length <= 256 && !containsForbiddenControl(value);
}

/** NUL, CR or LF — the bytes `valid_output_key`/`valid_tool_identity` both reject. */
function containsForbiddenControl(value: string): boolean {
  return value.includes('\u0000') || value.includes('\r') || value.includes('\n');
}

/** `yaml.rs:362-369` (`valid_graph_id`) — non-empty, ≤128 bytes, `[A-Za-z0-9_.:-]` only. */
export function isValidGraphId(value: string): boolean {
  return value.length > 0 && value.length <= MAX_NODE_ID_BYTES && NODE_ID_PATTERN.test(value);
}

/** `direct_tool.rs:1175-1179` (`valid_tool_identity`) / `application.rs:609-613` (`valid_application_alias`) — the same shape. */
export function isValidToolIdentity(value: unknown): boolean {
  return typeof value === 'string' && value.length > 0 && !containsForbiddenControl(value);
}

/** One route/transition target the compiler will run `validate_target` over. */
export interface RouteTargetRef {
  readonly field: string;
  readonly target: string;
}

/** Every present route/transition target on `node`, with the YAML field it came from. */
export function routeTargetsOf(node: AdmissionNode): readonly RouteTargetRef[] {
  const raw = node.raw;
  if (node.type === 'decision') return routeListTargets('nodes', raw.nodes).concat(optionalTarget('default_output', raw.default_output));
  if (node.type === 'router') return routeListTargets('routes', raw.routes).concat(optionalTarget('default_output', raw.default_output));
  if (node.type === 'hitl') return hitlTargets(raw);
  return optionalTarget('transition', raw.transition);
}

function listTargets(field: string, value: unknown): readonly RouteTargetRef[] {
  return toStringList(value).map((target, index) => ({ field: `${field}[${String(index)}]`, target }));
}

/**
 * A target that is present but not a string (a fraction, boolean, object, or an
 * integer too large to be a safe one) is NOT dropped: it is carried as text
 * that can never pass the id grammar, so the route-target rule refuses it by
 * name instead of the editor quietly ignoring a field the compiler would
 * refuse. Integer ids that are safe have already been normalized to strings.
 */
const targetOf = (value: unknown): string => (typeof value === 'string' ? value : unsupportedIdentifierText(value));

/** Node-id lists (`nodes`, `routes`): null entries stay ignored, as before; any other non-string is reported. */
function routeListTargets(field: string, value: unknown): readonly RouteTargetRef[] {
  if (!Array.isArray(value)) return [];
  const entries: readonly unknown[] = value;
  return entries.flatMap((entry, index) => (entry === null || entry === undefined ? [] : [{ field: `${field}[${String(index)}]`, target: targetOf(entry) }]));
}

function optionalTarget(field: string, value: unknown): readonly RouteTargetRef[] {
  return value === null || value === undefined ? [] : [{ field, target: targetOf(value) }];
}

/** `hitl.rs:77-85` — three optional named routes; a PRESENT one is validated even when empty (`hitl.rs:96-100`). */
function hitlTargets(raw: YamlPipelineNode): readonly RouteTargetRef[] {
  const declared: unknown = raw.routes;
  const routes = declared !== null && typeof declared === 'object' && !Array.isArray(declared) ? (declared as Readonly<Record<string, unknown>>) : {};
  return (['approve', 'reject', 'edit'] as const).flatMap((action) => optionalTarget(`routes.${action}`, routes[action]));
}

/** A HITL node's `routes:` mapping — `{}` when the field is absent or (illegally) a list. */
export function namedRoutes(raw: YamlPipelineNode): Readonly<Record<string, string>> {
  const routes = raw.routes;
  if (routes === undefined || Array.isArray(routes)) return {};
  // Rebuilt rather than asserted: `Array.isArray`'s `arg is any[]` predicate
  // does not narrow a `readonly string[]` out of the union, and the entries
  // filter drops a non-string route value (which the compiler would refuse
  // at deserialize time anyway) instead of carrying it into a route check.
  return Object.fromEntries(Object.entries(routes).filter((entry): entry is [string, string] => typeof entry[1] === 'string'));
}

/** `input_mapping` entries whose `type` is `variable` — those name a state key (`compiler.rs:1304`, `1319`, `1330`). */
function mappedVariables(raw: YamlPipelineNode): readonly RouteTargetRef[] {
  const mapping = raw.input_mapping;
  if (!mapping) return [];
  return Object.entries(mapping).flatMap(([key, entry]) => mappedVariable(key, entry));
}

/** One `input_mapping` entry, read defensively — the YAML tab can put anything here. */
function mappedVariable(key: string, entry: unknown): readonly RouteTargetRef[] {
  if (entry === null || typeof entry !== 'object') return [];
  const record = entry as { readonly type?: unknown; readonly value?: unknown };
  if (record.type !== 'variable' || typeof record.value !== 'string') return [];
  return [{ field: `input_mapping.${key}`, target: record.value }];
}

/**
 * `decision.rs:97-101` — a Decision node's `input_keys()` is
 * `decisional_inputs` when that field holds a NON-EMPTY list, otherwise
 * `input`, otherwise the built-in `["messages"]`. The losing field is
 * dropped before `validate_node_state` (`compiler.rs:1277`) ever sees it,
 * because `compiler.rs:374` delegates to `DecisionNodeDefinition::
 * input_keys`, which returns the resolved list rather than `raw.input`.
 *
 * Reading `raw.input` unconditionally here got this wrong in both
 * directions: it REFUSED a legal document (a stale `input` left beside a
 * live `decisional_inputs` — a save the user then cannot make) and ADMITTED
 * an illegal one (an undeclared key in `decisional_inputs`). The runtime's
 * own `routing_tests.rs:310-314` pins the precedence: `input: [ignored]`
 * with `decisional_inputs: [messages]` yields `input_keys() == ["messages"]`.
 *
 * The `["messages"]` fallback needs no branch — it is a built-in
 * (`compiler.rs:1440`), so an empty result is equivalent.
 */
function decisionInputKeys(raw: YamlPipelineNode): readonly RouteTargetRef[] {
  if (toStringList(raw.decisional_inputs).length > 0) return listTargets('decisional_inputs', raw.decisional_inputs);
  return listTargets('input', raw.input);
}

/** The state keys `node` names through whichever field the compiler resolves as its `input_keys()`. */
function inputReferences(node: AdmissionNode): readonly RouteTargetRef[] {
  if (node.type === 'decision') return decisionInputKeys(node.raw);
  if (READS_INPUT.has(node.type)) return listTargets('input', node.raw.input);
  return [];
}

/** State keys `node` names through `input`/`output`/`variables_to_clean`, each tagged with its field. */
export function declaredStateReferences(node: AdmissionNode): readonly RouteTargetRef[] {
  const raw = node.raw;
  const input = inputReferences(node);
  const output = READS_OUTPUT.has(node.type) ? listTargets('output', raw.output) : [];
  const cleaned = node.type === 'state_modifier' ? listTargets('variables_to_clean', raw.variables_to_clean) : [];
  const mapped = READS_INPUT_MAPPING.has(node.type) ? mappedVariables(raw) : [];
  return [...input, ...output, ...cleaned, ...mapped];
}

/** Non-`messages` outputs — the compiler's own filter at `llm.rs:174` / `direct_tool.rs:182`. */
export function dataOutputs(raw: YamlPipelineNode): readonly string[] {
  return toStringList(raw.output).filter((key) => key !== 'messages');
}
