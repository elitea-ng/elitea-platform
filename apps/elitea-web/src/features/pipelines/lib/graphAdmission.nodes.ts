/**
 * The per-NODE half of graph admission — see `./graphAdmission.types.ts` for
 * what admission means and why the editor mirrors it. Six rules live here:
 * `node.type`, `node.id`, `node.required-field`, `node.route-target`,
 * `node.state-reference` and `node.structured-output`.
 *
 * Split from `./graphAdmission.helpers.ts` for the §3.5 400-line file
 * budget only; the two halves share `AdmissionGraph` and are composed into
 * one catalogue by `GRAPH_ADMISSION_RULES` there.
 */
import { FlowEditorConstants, RuntimeContractConstants } from './flow-editor/constants';
import type { RouteTargetRef } from './graphAdmission.nodeReads';
import {
  BUILTIN_STATE_KEYS,
  RESERVED_NODE_ID,
  dataOutputs,
  declaredStateReferences,
  isValidGraphId,
  isValidOutputKey,
  isValidToolIdentity,
  namedRoutes,
  routeTargetsOf,
} from './graphAdmission.nodeReads';
import type { AdmissionGraph, AdmissionNode, GraphAdmissionIssue, GraphAdmissionRule } from './graphAdmission.types';
import { admissionIssue, countNonStringEntries, toStringList } from './graphAdmission.types';

const { MAX_NODE_ID_BYTES, isCompilerAdmittedNodeType } = RuntimeContractConstants;

/** The one route/transition target legal without naming a node — `compiler.rs:484`, `router.rs:331`, `hitl.rs:464`. */
const END_TARGET: string = FlowEditorConstants.PipelineNodeTypes.End;

// ── rules ────────────────────────────────────────────────────────────────

const nodeTypeRule: GraphAdmissionRule = {
  id: 'node.type',
  citation: 'compiler.rs:1242-1270',
  summary: 'every node `type:` has a `parse_pipeline_node` arm',
  check: (graph) =>
    graph.nodes
      .filter((node) => !isCompilerAdmittedNodeType(node.type))
      .map((node) =>
        admissionIssue(
          'node.type',
          'compiler.rs:1267',
          node.id,
          'type',
          node.type,
          `type: "${node.type}" is not a node type this runtime can run — the whole pipeline is refused.`,
        ),
      ),
};

const nodeIdRule: GraphAdmissionRule = {
  id: 'node.id',
  citation: 'yaml.rs:362-369, compiler.rs:1220-1229',
  summary: 'node ids are well-formed, unique, and not the compiler-owned reserved id',
  check: (graph) => graph.nodes.flatMap((node, index) => nodeIdIssues(graph, node, index)),
};

function nodeIdIssues(graph: AdmissionGraph, node: AdmissionNode, index: number): readonly GraphAdmissionIssue[] {
  if (!isValidGraphId(node.id)) {
    return [
      admissionIssue(
        'node.id',
        'yaml.rs:362',
        node.id,
        'id',
        node.id,
        `id: "${node.id}" is not a legal node id — only letters, digits and _ - . : are allowed (a space is not), 1-${String(MAX_NODE_ID_BYTES)} bytes.`,
      ),
    ];
  }
  if (node.id === RESERVED_NODE_ID) {
    return [admissionIssue('node.id', 'compiler.rs:1220', node.id, 'id', node.id, `id: "${node.id}" is reserved by the runtime.`)];
  }
  const isDuplicate = graph.nodes.findIndex((other) => other.id === node.id) !== index;
  if (isDuplicate) {
    return [admissionIssue('node.id', 'compiler.rs:1225', node.id, 'id', node.id, `id: "${node.id}" is used by more than one node — node ids must be unique.`)];
  }
  return [];
}

const requiredFieldRule: GraphAdmissionRule = {
  id: 'node.required-field',
  citation: 'application.rs:53-54/124/146, direct_tool.rs:52-53/159, printer.rs:41, decision.rs:40, hitl.rs:470-478',
  summary: 'each node family carries the fields its serde struct requires',
  check: (graph) => graph.nodes.flatMap((node) => requiredFieldIssues(node)),
};

function requiredFieldIssues(node: AdmissionNode): readonly GraphAdmissionIssue[] {
  if (node.type === 'agent') return agentRequiredFields(node);
  if (node.type === 'toolkit' || node.type === 'mcp') return directToolRequiredFields(node);
  if (node.type === 'printer') return printerRequiredFields(node);
  if (node.type === 'decision') return decisionRequiredFields(node);
  if (node.type === 'hitl') return hitlRequiredFields(node);
  return [];
}

/** `application.rs:53` (`tool` required), `:124` (`valid_application_alias`), `from_raw` (required task and bounded child mappings). */
function agentRequiredFields(node: AdmissionNode): readonly GraphAdmissionIssue[] {
  const issues: GraphAdmissionIssue[] = [];
  if (!isValidToolIdentity(node.raw.tool)) {
    issues.push(
      admissionIssue('node.required-field', 'application.rs:124', node.id, 'tool', String(node.raw.tool ?? ''), 'tool: an Agent node must name the agent it runs; this one is empty.'),
    );
  }
  const mappingKeys = Object.keys(node.raw.input_mapping ?? {});
  if (mappingKeys.length > 64 || !mappingKeys.includes('task')) {
    issues.push(
      admissionIssue(
        'node.required-field',
        'application.rs:146',
        node.id,
        'input_mapping',
        mappingKeys.join(', '),
        `input_mapping: an Agent node needs an entry keyed "task" and at most 64 mappings — found ${mappingKeys.length === 0 ? 'none' : `[${mappingKeys.join(', ')}]`}.`,
      ),
    );
  }
  for (const key of mappingKeys.filter((key) => key !== 'task')) {
    if (!isValidOutputKey(key) || RuntimeContractConstants.isReservedStateKey(key) || key === 'input' || key === 'messages') {
      issues.push(admissionIssue('node.required-field', 'application.rs::from_raw', node.id, `input_mapping.${key}`, key,
        `input_mapping: "${key}" is not an allowed child variable name.`));
    }
  }
  return issues;
}

/** `direct_tool.rs:52-53` (both required), `:159` (`valid_tool_identity` on both). */
function directToolRequiredFields(node: AdmissionNode): readonly GraphAdmissionIssue[] {
  return (['toolkit_name', 'tool'] as const)
    .filter((field) => !isValidToolIdentity(node.raw[field]))
    .map((field) =>
      admissionIssue(
        'node.required-field',
        'direct_tool.rs:159',
        node.id,
        field,
        String(node.raw[field] ?? ''),
        `${field}: required by a ${node.type} node, and currently empty — pick a toolkit and an action.`,
      ),
    );
}

/** `printer.rs:41` — `transition` carries no `#[serde(default)]`, so a Printer without one never deserializes. */
function printerRequiredFields(node: AdmissionNode): readonly GraphAdmissionIssue[] {
  if (typeof node.raw.transition === 'string') return [];
  return [admissionIssue('node.required-field', 'printer.rs:41', node.id, 'transition', '', 'transition: a Printer node must name where it goes next (a node id, or END).')];
}

/**
 * `decision.rs:40` — `nodes` carries no `#[serde(default)]`, so the field
 * must be present. An EMPTY list is legal (`router.rs:70` bounds the length
 * from above only) and is left alone here on purpose: a fresh Decision node
 * is seeded `nodes: []` and would otherwise be unsaveable the moment it is
 * added.
 */
function decisionRequiredFields(node: AdmissionNode): readonly GraphAdmissionIssue[] {
  if (Array.isArray(node.raw.nodes)) return [];
  return [admissionIssue('node.required-field', 'decision.rs:40', node.id, 'nodes', '', 'nodes: a Decision node must declare its branch list (it may be empty, but it must exist).')];
}

/** `hitl.rs:155-157` — `from_raw` runs `validate_message` and `validate_routes` in turn, and EITHER refusal takes the whole document. */
function hitlRequiredFields(node: AdmissionNode): readonly GraphAdmissionIssue[] {
  return [...hitlUserMessageIssues(node), ...hitlRouteActionIssues(node)];
}

/**
 * `hitl.rs:439-447` — `validate_message` refuses an EMPTY
 * `user_message.value` outright, and `parse_pipeline_node`
 * (`compiler.rs:1252`) turns that into "a HITL node is invalid" for the
 * whole pipeline.
 *
 * An ABSENT `user_message`, and an absent `value` inside a present one, are
 * both legal: the struct carries `#[serde(default)]` and the field carries
 * `#[serde(default = "default_message")]` (`hitl.rs:60-64`), so serde
 * substitutes `default_message` (`hitl.rs:625-627`). Only a value the user
 * has actually cleared is an issue — which is exactly what the node card
 * writes (`HITLNode.parts.tsx`'s `handleUserMessageMappingChange` stores the
 * field verbatim) and what js-yaml then dumps as `value: ''`.
 *
 * The REST of `validate_message` — the 8 KiB bound and the NUL byte
 * (`hitl.rs:441-442`), and the `variable`/`fstring` sub-checks
 * (`hitl.rs:448-455`) — is deliberately not mirrored. This module may only
 * be as strict as the compiler, and a byte-length or template grammar
 * transcribed approximately would refuse a legal save; they stay
 * compiler-side until each can be cited exactly.
 */
function hitlUserMessageIssues(node: AdmissionNode): readonly GraphAdmissionIssue[] {
  const message = node.raw.user_message;
  if (message === undefined || message.value !== '') return [];
  return [
    admissionIssue(
      'node.required-field',
      'hitl.rs:440',
      node.id,
      'user_message.value',
      '',
      'User message: a HITL node must ask the reviewer something — an empty message refuses the whole pipeline, not just this node.',
    ),
  ];
}

/**
 * `hitl.rs:469-478` (`validate_routes`) — approve, reject, or an edit route
 * that goes somewhere other than END with an `edit_state_key` present;
 * otherwise the node can do nothing.
 *
 * `typeof node.raw.edit_state_key === 'string'` is the right test here even
 * for `''`, and deliberately not tightened: Rust asks `edit_state_key
 * .is_some()` (`hitl.rs:477`), and `Some("")` IS some — an empty key
 * satisfies THIS check and is refused by the separate one at
 * `hitl.rs:158-165`, which `hitlEditStateKeyIssues` mirrors below. Merging
 * the two would report the wrong field and the wrong line.
 */
function hitlRouteActionIssues(node: AdmissionNode): readonly GraphAdmissionIssue[] {
  const named = namedRoutes(node.raw);
  const editIsReal = typeof named['edit'] === 'string' && named['edit'] !== END_TARGET && typeof node.raw.edit_state_key === 'string';
  if (typeof named['approve'] === 'string' || typeof named['reject'] === 'string' || editIsReal) return [];
  return [
    admissionIssue('node.required-field', 'hitl.rs:474', node.id, 'routes', '', 'routes: a HITL node needs at least one action — an approve route, a reject route, or an edit route with an edit_state_key.'),
  ];
}

const routeTargetRule: GraphAdmissionRule = {
  id: 'node.route-target',
  citation: 'router.rs:330-335, compiler.rs:482-490',
  summary: 'every route/transition target is END or a declared node id',
  check: (graph) => graph.nodes.flatMap((node) => routeTargetsOf(node).flatMap((ref) => routeTargetIssues(graph, node, ref))),
};

function routeTargetIssues(graph: AdmissionGraph, node: AdmissionNode, ref: RouteTargetRef): readonly GraphAdmissionIssue[] {
  if (ref.target === END_TARGET) return [];
  if (!isValidGraphId(ref.target)) {
    return [
      admissionIssue(
        'node.route-target',
        'router.rs:331',
        node.id,
        ref.field,
        ref.target,
        `${ref.field}: "${ref.target}" is not a legal target — it must be END or a node id (letters, digits, _ - . :).`,
      ),
    ];
  }
  if (!graph.nodeIds.has(ref.target)) {
    return [admissionIssue('node.route-target', 'compiler.rs:484', node.id, ref.field, ref.target, `${ref.field}: "${ref.target}" does not name any node in this pipeline.`)];
  }
  return [];
}

const stateReferenceRule: GraphAdmissionRule = {
  id: 'node.state-reference',
  citation: 'compiler.rs:1277-1290, 1293-1338, 1344-1351',
  summary: 'every state key a node reads or writes is declared in `state:` (or is a runtime built-in)',
  check: (graph) => graph.nodes.flatMap((node) => stateReferenceIssues(graph, node)),
};

function stateReferenceIssues(graph: AdmissionGraph, node: AdmissionNode): readonly GraphAdmissionIssue[] {
  const undeclared = declaredStateReferences(node)
    .filter((ref) => !BUILTIN_STATE_KEYS.has(ref.target) && !graph.stateKeys.has(ref.target))
    .map((ref) =>
      admissionIssue(
        'node.state-reference',
        'compiler.rs:1284',
        node.id,
        ref.field,
        ref.target,
        `${ref.field}: "${ref.target}" is not declared in this pipeline's state — declare it in State, or point this field at a key that exists.`,
      ),
    );
  return [...undeclared, ...malformedListEntries(node), ...hitlEditStateKeyIssues(graph, node)];
}

/** A non-string entry in `input`/`output`/`variables_to_clean` fails `Vec<String>` deserialization before any rule runs. */
function malformedListEntries(node: AdmissionNode): readonly GraphAdmissionIssue[] {
  return (['input', 'output', 'variables_to_clean'] as const)
    .filter((field) => countNonStringEntries(node.raw[field]) > 0)
    .map((field) => admissionIssue('node.state-reference', 'llm.rs:163', node.id, field, '', `${field}: every entry must be a state-variable name.`))
    .concat(
      toStringList(node.raw.output)
        .filter((key) => !isValidOutputKey(key))
        .map((key) => admissionIssue('node.state-reference', 'yaml.rs:371', node.id, 'output', key, `output: "${key}" is not a legal state-variable name.`)),
    );
}

/**
 * Two runtime checks on the same field, in the order
 * `HitlNodeDefinition::from_raw` and then the compiler run them:
 *
 *  - `hitl.rs:158-165` — `edit_state_key` is `Option<String>`, and a
 *    PRESENT key must pass `valid_output_key`, which rejects the empty
 *    string (`yaml.rs:372`). `Some("")` is present-and-illegal, not absent;
 *    only `None` is legal. That is why the guard below tests `typeof key
 *    !== 'string'` and NOT `key === ''` — treating `''` as absent admitted a
 *    document the compiler refuses outright, and js-yaml really does store
 *    it as `edit_state_key: ''` once the picker has written an empty value.
 *  - `compiler.rs:1344-1351` — the key is then checked against `state:`
 *    with NO built-in escape, unlike every other key.
 */
function hitlEditStateKeyIssues(graph: AdmissionGraph, node: AdmissionNode): readonly GraphAdmissionIssue[] {
  const key = node.raw.edit_state_key;
  if (node.type !== 'hitl' || typeof key !== 'string') return [];
  if (!isValidOutputKey(key)) {
    return [
      admissionIssue(
        'node.state-reference',
        'hitl.rs:161',
        node.id,
        'edit_state_key',
        key,
        `edit_state_key: "${key}" is not a legal state-variable name — pick a state variable, or leave the Edit state key unset.`,
      ),
    ];
  }
  if (graph.stateKeys.has(key)) return [];
  return [
    admissionIssue(
      'node.state-reference',
      'compiler.rs:1348',
      node.id,
      'edit_state_key',
      key,
      `edit_state_key: "${key}" must be declared in this pipeline's state — a HITL edit key gets no built-in fallback.`,
    ),
  ];
}

const structuredOutputRule: GraphAdmissionRule = {
  id: 'node.structured-output',
  citation: 'llm.rs:171-190, direct_tool.rs:182-186',
  summary: 'structured nodes name at least one data output; non-structured LLM nodes name at most one',
  check: (graph) => graph.nodes.flatMap((node) => structuredOutputIssues(node)),
};

function structuredOutputIssues(node: AdmissionNode): readonly GraphAdmissionIssue[] {
  const structured = node.raw.structured_output === true;
  const outputs = dataOutputs(node.raw);
  if (node.type === 'llm') return llmStructuredOutputIssues(node, structured, outputs);
  if ((node.type === 'toolkit' || node.type === 'mcp') && structured && outputs.length === 0) {
    return [
      admissionIssue('node.structured-output', 'direct_tool.rs:182', node.id, 'output', '', 'output: structured output needs a state variable to write into — "messages" alone is not enough.'),
    ];
  }
  return [];
}

function llmStructuredOutputIssues(node: AdmissionNode, structured: boolean, outputs: readonly string[]): readonly GraphAdmissionIssue[] {
  if (structured && outputs.length === 0) {
    return [admissionIssue('node.structured-output', 'llm.rs:186', node.id, 'output', '', 'output: a structured LLM node must name at least one output other than "messages".')];
  }
  if (!structured && outputs.length > 1) {
    return [
      admissionIssue(
        'node.structured-output',
        'llm.rs:182',
        node.id,
        'output',
        outputs.join(', '),
        `output: a non-structured LLM node may write only one data output — found ${String(outputs.length)} (${outputs.join(', ')}). Turn on Structured output, or drop one.`,
      ),
    ];
  }
  return [];
}

/** The six per-node rules, in the order the compiler would hit them. */
export const NODE_ADMISSION_RULES: readonly GraphAdmissionRule[] = [
  nodeTypeRule,
  nodeIdRule,
  requiredFieldRule,
  routeTargetRule,
  stateReferenceRule,
  structuredOutputRule,
];
