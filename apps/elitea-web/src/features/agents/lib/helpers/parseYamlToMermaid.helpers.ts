/**
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/agent/lib/helpers/parseYamlToMermaid.helpers.js`
 * (byte-for-byte diagram-generation logic: Jinja condition parsing, decision/
 * router/HITL edge rendering, the whole `parseYamlToMermaid` entry point).
 *
 * **DISCLOSED REDESIGN — `FlowEditorConstants.PipelineNodeTypes` inlined,
 * not imported.** The baseline imports
 * `@/[fsd]/features/pipelines/flow-editor/lib/constants` (`FlowEditorConstants
 * .PipelineNodeTypes.{Decision,Router,Hitl}`) — a DIFFERENT `features/`
 * slice (`pipelines`). This app's `.dependency-cruiser.cjs` `no-sideways-
 * features` rule forbids any `features/` slice importing another
 * `features/` slice's internals, even via its own public `index.ts` — no
 * carve-out (confirmed: `features/pipelines/lib/flow-editor/constants/
 * flowEditor.constants.ts` exists in this worktree, at
 * `PipelineNodeTypes.{Decision: 'decision', Router: 'router', Hitl:
 * 'hitl'}`, but importing it from here would violate that rule). This
 * function only ever compares `node.type` against exactly THREE of that
 * enum's ~19 values (`Decision`, `Router`, `Hitl`); the other sixteen (Tool,
 * Agent, Pipeline, Function, LLM, Condition, Loop, ...) are never read here.
 * Rather than duplicate the entire pipeline node-type enum feature-locally
 * (which would drift the moment `pipelines` adds/renames a node type this
 * function does not care about), the three literal string values actually
 * used are inlined below as `PARSE_YAML_NODE_TYPES`, with the exact same
 * string values verified against `flowEditor.constants.ts` at the time of
 * this port. If `pipelines` ever renames one of these three wire values,
 * this file's own mermaid-rendering test suite will fail to notice — a
 * known, accepted risk of inlining over duplicating the whole enum,
 * documented rather than silently reinterpreted (house style: "state the
 * constraint, don't silently reinterpret").
 *
 * `js-yaml`'s `load()` (not the default-import `YAML.load()` the baseline
 * uses) — same named-import convention already established by this app's
 * `features/pipelines/lib/yamlLint.ts` (unit A2) for the same dependency,
 * kept consistent rather than reintroducing the default-import style.
 *
 * **Verified dependency-version behaviour difference:** this app's
 * `js-yaml` is `5.2.2` (`package.json`), vs. the baseline's `4.x`. `load('')`
 * THROWS `YAMLException: expected a document, but the input is empty` on
 * 5.2.2 (confirmed by direct `node -e` repro against this app's installed
 * `node_modules/js-yaml`), where 4.x resolved to `undefined` without
 * throwing. This function's pre-existing try/catch (baseline
 * `parseYamlToMermaid.helpers.js:185-191`, "swallow parse error, return
 * ''") already covers the new failure mode with no code change — the
 * observable difference is that an empty-string input now returns `''`
 * (parse-error path) instead of `'graph TD\n'` (empty-but-valid-document
 * path). Exercised directly by this file's own test suite.
 */
import { load } from 'js-yaml';

import { normalizePipelineNodeIdentifiers } from '@/shared/lib/pipelineNodeIdentifiers';

/**
 * The three `PipelineNodeTypes` wire values this diagram renderer actually
 * branches on — see the module doc comment's DISCLOSED REDESIGN note.
 * Verified byte-for-byte against
 * `features/pipelines/lib/flow-editor/constants/flowEditor.constants.ts`'s
 * `PipelineNodeTypes.{Decision,Router,Hitl}` at port time.
 */
const PARSE_YAML_NODE_TYPES = {
  Decision: 'decision',
  Router: 'router',
  Hitl: 'hitl',
} as const;

// ── Loose YAML pipeline shape ───────────────────────────────────────────────
// Only the fields this renderer actually reads. The real shape is owned by
// `entities/pipeline`/`features/pipelines`, out of this file's scope.

interface PipelineYamlDecision {
  readonly nodes?: readonly string[];
  readonly default_output?: string;
}

interface PipelineYamlRouter {
  readonly routes?: unknown;
  readonly default_output?: string;
}

interface PipelineYamlCondition {
  readonly condition_definition?: string;
}

interface PipelineYamlNode {
  readonly id?: string;
  readonly type?: string;
  readonly transition?: string;
  readonly condition?: PipelineYamlCondition;
  /** Legacy decision shape: a `decision` property directly on the node (pre node-type-based decisions). */
  readonly decision?: PipelineYamlDecision;
  readonly nodes?: readonly string[];
  readonly default_output?: string;
  /** Router nodes: `readonly string[]`. HITL nodes: `Record<action, target>`. Narrowed per-branch (both wire shapes share this one field, like the baseline's untyped `node.routes`). */
  readonly routes?: unknown;
}

interface PipelineYamlDocument {
  readonly entry_point?: string;
  readonly nodes?: readonly PipelineYamlNode[];
}

// ── Jinja condition parsing (Mermaid label rendering) ───────────────────────

type JinjaToken =
  | { readonly type: 'block'; readonly value: string }
  | { readonly type: 'variable'; readonly value: string }
  | { readonly type: 'text'; readonly value: string }
  | { readonly type: 'comment'; readonly value: string };

type JinjaTreeNode =
  | { readonly type: 'text'; readonly value: string }
  | { readonly type: 'variable'; readonly value: string }
  | { type: 'if' | 'elif' | 'else'; condition?: string; children: JinjaTreeNode[] };

/** Sanitize ID for Mermaid compatibility (alphanumeric and underscore only). `parseYamlToMermaid.helpers.js:5`. */
function sanitizeId(id: unknown): string {
  // Integer ids are normalized to strings on load; any other non-string (a fraction, boolean, ...) is not a node id.
  return typeof id === 'string' ? id.replace(/[^a-zA-Z0-9_]/g, '_') : '';
}

/** Generate a mermaid diagram edge line. `parseYamlToMermaid.helpers.js:9-12`. */
function addMermaidLine(from: string, to: string, label = ''): string {
  const labelPart = label ? `|${label}|` : '';
  return `  ${from} -->${labelPart} ${to}\n`;
}

function goThroughJinjaNodeTree(
  conditionId: string,
  conditionExpression: string,
  tree: readonly JinjaTreeNode[],
  result: { mermaidDiagram: string },
): void {
  if (tree.length === 1) {
    const only = tree[0];
    if (only?.type === 'text') {
      const branchId = sanitizeId(only.value.trim());
      const realCondition = conditionExpression.trim().replaceAll('|', ' or \n').replaceAll('"', '');
      result.mermaidDiagram += addMermaidLine(conditionId, branchId, realCondition);
    }
  } else {
    tree.forEach((node) => {
      if (node.type === 'if' || node.type === 'elif') {
        const newConditionExpression = conditionExpression
          ? `${conditionExpression} AND ${node.condition ?? ''}`
          : (node.condition ?? '');
        goThroughJinjaNodeTree(conditionId, newConditionExpression, node.children, result);
      } else if (node.type === 'else') {
        goThroughJinjaNodeTree(conditionId, conditionExpression, node.children, result);
      }
    });
  }
}

function parseJinjaCondition(input: string, conditionId: string): string {
  const tokens = tokenize(input);
  const tree = parseTokens(tokens);
  const result = { mermaidDiagram: '' };
  goThroughJinjaNodeTree(conditionId, '', tree, result);
  return result.mermaidDiagram;
}

function tokenize(input: string): JinjaToken[] {
  const tokenRegex = /({%.*?%})|({{.*?}})|([^{]+)|({#.*?#})/gs;
  const tokens: JinjaToken[] = [];
  let match: RegExpExecArray | null;

  while ((match = tokenRegex.exec(input)) !== null) {
    if (match[1] !== undefined) {
      tokens.push({ type: 'block', value: match[1].trim() });
    } else if (match[2] !== undefined) {
      tokens.push({ type: 'variable', value: match[2].trim() });
    } else if (match[3] !== undefined) {
      const text = match[3].trim();
      if (text) {
        tokens.push({ type: 'text', value: text });
      }
    } else if (match[4] !== undefined) {
      tokens.push({ type: 'comment', value: match[4].trim() });
    }
  }

  return tokens;
}

function parseTokens(tokens: readonly JinjaToken[]): JinjaTreeNode[] {
  const children: JinjaTreeNode[] = [];
  const stack: JinjaTreeNode[][] = [children];

  const getCurrentLevel = (): JinjaTreeNode[] => stack[stack.length - 1] ?? children;

  const addConditionalNode = (type: 'if' | 'elif' | 'else', condition?: string): void => {
    const node: JinjaTreeNode = { type, children: [], ...(condition !== undefined ? { condition } : {}) };
    if (type !== 'if') {
      stack.pop(); // Pop for elif/else
    }
    getCurrentLevel().push(node);
    stack.push(node.children);
  };

  for (const token of tokens) {
    if (token.type === 'block') {
      const blockContent = token.value.slice(2, -2).trim();
      if (blockContent.startsWith('if')) {
        addConditionalNode('if', blockContent.slice(3).trim());
      } else if (blockContent.startsWith('elif')) {
        addConditionalNode('elif', blockContent.slice(5).trim());
      } else if (blockContent.startsWith('else')) {
        addConditionalNode('else');
      } else if (blockContent.startsWith('endif')) {
        stack.pop();
      }
    } else if (token.type === 'variable') {
      getCurrentLevel().push({ type: 'variable', value: token.value.slice(2, -2).trim() });
    } else if (token.type === 'text') {
      getCurrentLevel().push({ type: 'text', value: token.value });
    }
  }

  return children;
}

// ── Decision / router edge rendering ────────────────────────────────────────

/** Handles both the legacy (`node.decision`) and type-based decision node shapes. `parseYamlToMermaid.helpers.js:143-165`. */
function handleDecisionNode(
  mermaidDiagram: string,
  nodeId: string,
  decisionData: PipelineYamlDecision,
  isLegacy: boolean,
): string {
  let diagram = mermaidDiagram;
  const sourceId = isLegacy ? `${nodeId}_decision` : nodeId;

  if (isLegacy) {
    diagram += `  ${sourceId}{"Decision"}\n`;
    diagram += addMermaidLine(nodeId, sourceId);
  }

  const { nodes: destNodes, default_output } = decisionData;
  destNodes?.forEach((destNode) => {
    const destNodeId = sanitizeId(destNode);
    diagram += addMermaidLine(sourceId, destNodeId);
  });

  if (default_output) {
    const defaultOutputId = sanitizeId(default_output);
    diagram += addMermaidLine(sourceId, defaultOutputId);
  }

  return diagram;
}

/** `parseYamlToMermaid.helpers.js:169-182`. */
function handleRouterNode(mermaidDiagram: string, nodeId: string, routerData: PipelineYamlRouter): string {
  let diagram = mermaidDiagram;
  const sourceId = nodeId;

  const { routes, default_output } = routerData;
  const routeTargets: readonly string[] = Array.isArray(routes) ? (routes as readonly string[]) : [];
  routeTargets.forEach((destNode) => {
    const destNodeId = sanitizeId(destNode);
    diagram += addMermaidLine(sourceId, destNodeId);
  });

  if (default_output) {
    const defaultOutputId = sanitizeId(default_output);
    diagram += addMermaidLine(sourceId, defaultOutputId);
  }

  return diagram;
}

// ── Entry point ──────────────────────────────────────────────────────────────

/**
 * Renders a pipeline YAML definition as a Mermaid flowchart (`graph TD`)
 * string. Returns `''` on a YAML parse error (matches the baseline's
 * swallow-and-return-empty behaviour, `parseYamlToMermaid.helpers.js:185-
 * 191`) — a caller that wants to distinguish "empty pipeline" from "invalid
 * YAML" cannot from this return value alone, same limitation as the
 * baseline. `parseYamlToMermaid.helpers.js:184-244`, ported line-for-line.
 */
export function parseYamlToMermaid(yamlString: string): string {
  let yamlJson: PipelineYamlDocument = {};
  let mermaidDiagram = 'graph TD\n';
  try {
    // `id: 1` loads as a number; the diagram is keyed by the same decimal strings the runtime uses.
    yamlJson = normalizePipelineNodeIdentifiers((load(yamlString) as PipelineYamlDocument | undefined) ?? {});
  } catch {
    // Parse error: swallow and return '', matching the baseline exactly.
    return '';
  }
  if (!yamlJson) {
    return mermaidDiagram;
  }

  const nodes = yamlJson.nodes;
  const entryPoint = sanitizeId(yamlJson.entry_point);

  if (entryPoint) {
    mermaidDiagram += `  start((Start))\n`;
    mermaidDiagram += addMermaidLine('start', entryPoint);
  }

  nodes?.forEach((node) => {
    mermaidDiagram = processNode(mermaidDiagram, node);
  });

  return mermaidDiagram;
}

/** `node.decision` (legacy) / type-based Decision / type-based Router edge rendering — one node's worth. Extracted from `parseYamlToMermaid`'s per-node loop purely to keep that function's `complexity` under this codebase's gate. */
function renderNodeDecisionOrRouter(mermaidDiagram: string, nodeId: string, node: PipelineYamlNode): string {
  if (node.decision) {
    return handleDecisionNode(mermaidDiagram, nodeId, node.decision, true);
  }
  if (node.type === PARSE_YAML_NODE_TYPES.Decision) {
    return handleDecisionNode(mermaidDiagram, nodeId, node, false);
  }
  if (node.type === PARSE_YAML_NODE_TYPES.Router) {
    return handleRouterNode(mermaidDiagram, nodeId, node);
  }
  return mermaidDiagram;
}

/** HITL nodes' action-labelled `routes` (an object, not an array — see `PipelineYamlNode.routes`'s own doc comment). */
function renderHitlRoutes(mermaidDiagram: string, nodeId: string, node: PipelineYamlNode): string {
  if (node.type !== PARSE_YAML_NODE_TYPES.Hitl || !node.routes || typeof node.routes !== 'object' || Array.isArray(node.routes)) {
    return mermaidDiagram;
  }
  let diagram = mermaidDiagram;
  Object.entries(node.routes as Record<string, string | undefined>).forEach(([action, target]) => {
    if (target) {
      diagram += addMermaidLine(nodeId, sanitizeId(target), action);
    }
  });
  return diagram;
}

/** One node's full contribution to the diagram — id label, transition edge, condition block, decision/router edges, HITL routes. Extracted from `parseYamlToMermaid`'s `forEach` body purely to keep that function's own `complexity` under this codebase's gate. */
function processNode(mermaidDiagram: string, node: PipelineYamlNode): string {
  const nodeId = sanitizeId(node.id);
  let diagram = mermaidDiagram + `  ${nodeId}["${node.id ?? ''}"]\n`;

  if (node.transition) {
    diagram += addMermaidLine(nodeId, sanitizeId(node.transition));
  }
  if (node.condition?.condition_definition && node.type !== PARSE_YAML_NODE_TYPES.Router) {
    const conditionId = `${nodeId}_condition`;
    diagram += `  ${conditionId}{"Condition"}\n`;
    diagram += addMermaidLine(nodeId, conditionId);
    diagram += parseJinjaCondition(node.condition.condition_definition, conditionId);
  }

  diagram = renderNodeDecisionOrRouter(diagram, nodeId, node);
  diagram = renderHitlRoutes(diagram, nodeId, node);

  return diagram;
}
