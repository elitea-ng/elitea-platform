/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/lib/
 * helpers/parsePipeline.helpers.js` (694 lines, unit A2c) — YAML pipeline
 * document <-> React-Flow graph conversion, plus the legacy-decision
 * migration and a lightweight node-type-histogram extractor used by
 * pipeline-list summaries.
 *
 * Split across five files purely to keep each under the §3.5 400-line
 * budget: `parsePipelineState.helpers.ts` (`parseState`/`getNodePosition`),
 * `parsePipelineGraphPrimitives.helpers.ts` (dedup node/edge builders),
 * `parsePipelineLegacyBranches.helpers.ts` /
 * `parsePipelineModernBranches.helpers.ts` (per-node-shape branch builders),
 * `parsePipelineTraversal.helpers.ts` (`goThroughNodesTree`/`parseNodes`,
 * the recursive walker). This file re-exports `parseState`/`parseNodes` so
 * callers keep the baseline's single `parsePipeline.helpers` import surface,
 * and owns the three functions that don't need splitting:
 * `parseYaml`/`migerateLegacyNodes`/`extractPipelineNodeTypes`.
 *
 * **Disclosed deviation:** the baseline imports js-yaml's default export
 * (`import YAML from 'js-yaml'`, then `YAML.load(...)`). This app's pinned
 * `js-yaml@5.2.2` (`package.json`) ships NO default export at all — verified
 * against `node_modules/js-yaml/dist/js-yaml.d.ts`, a single `export { ...,
 * load, ... }` block, no `export default`. Uses the equivalent named import
 * (`import { load } from 'js-yaml'`) instead; behaviour is unchanged.
 */
import { load } from 'js-yaml';

import { normalizePipelineNodeIdentifiers } from '@/shared/lib/pipelineNodeIdentifiers';

import { PipelineNodeTypes, DECISION_NODE_ID_SUFFIX, ORIENTATION } from '../constants/flowEditor.constants';
import { getInitialNodeId } from './flowEditor.helpers';
import { parseState } from './parsePipelineState.helpers';
import { parseNodes } from './parsePipelineTraversal.helpers';
import type { FlowGraphEdge, FlowGraphNode, YamlPipelineDocument, YamlPipelineNode } from './pipelineFlow.types';
import type { Orientation } from '../constants/flowEditor.constants';

export { parseState } from './parsePipelineState.helpers';
export { parseNodes } from './parsePipelineTraversal.helpers';

export const parseYaml = (
  rawYamlJson: YamlPipelineDocument | undefined,
  orientation: Orientation = ORIENTATION.vertical,
): {
  readonly nodes: FlowGraphNode[];
  readonly state: FlowGraphNode | null;
  readonly edges: FlowGraphEdge[];
} => {
  // Unquoted `id: 1` / `transition: 2` load as numbers; the graph below is keyed by string ids.
  const yamlJson = normalizePipelineNodeIdentifiers(rawYamlJson);
  const state = parseState(yamlJson);
  const { nodes, edges } = parseNodes(yamlJson, orientation);
  const types = Object.values(PipelineNodeTypes) as readonly string[];
  const mappedNodes = nodes.map(node =>
    types.find(type => node.type === type)
      ? node
      : {
          ...node,
          type: PipelineNodeTypes.Default,
          originalEliteAType: node.type,
          data: { ...node.data, type: node.type },
        },
  );

  return {
    nodes: [...mappedNodes],
    state,
    edges: [...edges],
  };
};

interface LegacyDecisionYamlNode extends YamlPipelineNode {
  readonly decision?: { readonly decisional_inputs?: readonly unknown[]; readonly [key: string]: unknown };
}

/**
 * Migrates every legacy inline `decision` sub-object into a standalone
 * Decision-type node, returning the ids of the (now-redundant) synthetic
 * `~~~DecisionNode` flow nodes the caller should also remove.
 *
 * **Faithfully preserved baseline inconsistency (NOT a bug fix):** the
 * baseline returns THREE different shapes depending on the branch —
 * `yamlJson` bare (no `nodes`/not-array/no legacy-decision-node-shape early
 * exit does NOT apply here, only the first guard does), `{ yamlJson,
 * flowNodesToRemove: [] }` (nothing to migrate), or `{ yamlJson:
 * {...migrated}, flowNodesToRemove }` (migrated). `parsePipeline.
 * helpers.js:626-669` — every caller in the baseline destructures
 * `{ yamlJson, flowNodesToRemove }` from the result regardless of branch,
 * which means the bare-`yamlJson`-return branch silently produces
 * `flowNodesToRemove: undefined` at every real call site. Reproduced here
 * exactly, not redesigned — a "fix" would diverge from the baseline's
 * actual (if inconsistent) runtime behaviour.
 */
export const migerateLegacyNodes = (
  rawYamlJson: YamlPipelineDocument | undefined,
):
  | YamlPipelineDocument
  | undefined
  | { readonly yamlJson: YamlPipelineDocument | undefined; readonly flowNodesToRemove: readonly string[] } => {
  // Every YAML -> flow parse passes through here, so this is where integer ids become strings
  // (before `getInitialNodeId` below, which string-compares ids, can see them).
  const yamlJson = normalizePipelineNodeIdentifiers(rawYamlJson);
  const legacyNodes: readonly LegacyDecisionYamlNode[] | undefined = Array.isArray(yamlJson?.nodes)
    ? (yamlJson.nodes as readonly LegacyDecisionYamlNode[])
    : undefined;
  if (!yamlJson || !legacyNodes) {
    return yamlJson;
  }
  const migratedNodes: YamlPipelineNode[] = [];
  const flowNodesToRemove: string[] = [];
  if (!legacyNodes.find(node => node.decision)) {
    return { yamlJson, flowNodesToRemove };
  }
  legacyNodes.forEach(legacyNode => {
    if (legacyNode.decision) {
      const { decision, ...rest } = legacyNode;
      flowNodesToRemove.push(`${legacyNode.id}${DECISION_NODE_ID_SUFFIX}`);
      const decisionNodeId = getInitialNodeId(PipelineNodeTypes.Decision, [...legacyNodes, ...migratedNodes]);
      migratedNodes.push({ ...rest, transition: decisionNodeId });
      const { decisional_inputs, ...left } = decision;
      migratedNodes.push({
        ...left,
        // YAML holds no `undefined`: without legacy inputs the node gets no `input` key (the strict serializer refuses one).
        ...(decisional_inputs === undefined ? {} : { input: decisional_inputs }),
        type: PipelineNodeTypes.Decision,
        id: decisionNodeId,
      });
    } else {
      migratedNodes.push(legacyNode);
    }
  });

  return {
    yamlJson: { ...yamlJson, nodes: migratedNodes },
    flowNodesToRemove,
  };
};

export interface PipelineNodeTypeHistogram {
  readonly nodeTypes: Record<string, number>;
  readonly totalNodeCount: number;
}

/** Parses a pipeline's raw YAML `instructions` and counts nodes by `type`, for list-view summaries. Returns `null` on any parse failure or malformed shape. */
export const extractPipelineNodeTypes = (instructions: string | null | undefined): PipelineNodeTypeHistogram | null => {
  if (!instructions) return null;

  try {
    const parsedYaml = load(instructions) as YamlPipelineDocument | undefined;
    const parsedNodes: readonly YamlPipelineNode[] | undefined = Array.isArray(parsedYaml?.nodes)
      ? parsedYaml.nodes
      : undefined;
    if (!parsedNodes) return null;

    const nodeTypeCounts: Record<string, number> = {};
    let totalNodeCount = 0;

    parsedNodes.forEach(node => {
      // `node?.type` — the parsed YAML `nodes` array is cast, not validated; a `null` entry
      // (`nodes: [null, ...]`) is a real possibility and `node.type` would throw inside this
      // `try`, discarding the whole histogram instead of just skipping that one entry. Matches
      // baseline `parsePipeline.helpers.js:670`'s `node?.type`.
      if (node?.type) {
        nodeTypeCounts[node.type] = (nodeTypeCounts[node.type] ?? 0) + 1;
        totalNodeCount++;
      }
    });

    return { nodeTypes: nodeTypeCounts, totalNodeCount };
  } catch {
    return null;
  }
};
