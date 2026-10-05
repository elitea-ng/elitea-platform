import { isFixedParallelAuthoringAllowed } from '../constants/parallel.constants';
/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/lib/
 * helpers/flowEditor.helpers.js` (512 lines, unit A2c). This is the
 * HIGH-FAN-IN foundation of the pipelines flow editor — the baseline is
 * imported by ~29 other pipelines files (`batchUpdateYamlNode`,
 * `getDefaultInputMappingOfTool`, `canConnectToTarget`, node-id generation).
 *
 * Split into two files purely to keep each under the §3.5 400-line budget:
 * the input-mapping-default computation (`getDefaultInputMappingOfTool`,
 * `getRequiredInputsAndTooltips`, `getInputMappingDefaultValue`,
 * `getEnumList`) lives in `./flowEditorInputMapping.helpers.ts` and is
 * re-exported below, so callers keep the baseline's single
 * `flowEditor.helpers` import surface.
 *
 * **Disclosed deviation:** the baseline's `removeYamlNodeVariablesMapping`
 * uses `deepClone` from `@mui/x-data-grid/internals`
 * (`flowEditor.helpers.js:1,97`). That subpath export does not exist in
 * this app's pinned `@mui/x-data-grid@9.10.1` (confirmed:
 * `node_modules/@mui/x-data-grid/internals/index.d.ts` has no `deepClone`
 * export at all — it was never a public MUI X API, just something the
 * baseline's older MUI X version happened to re-export). The platform-native
 * `structuredClone` is the faithful behavioural equivalent (both are
 * generic structured deep-clones, not data-grid-specific) and is used here
 * instead.
 */
import {
  CONDITION_NODE_ID_SUFFIX,
  DECISION_NODE_ID_SUFFIX,
  HITL_HANDLE_ID_SUFFIX,
  NodeHeightMap,
  PipelineNodeTypes,
  ROUTER_HANDLE_ID_SUFFIX,
} from '../constants/flowEditor.constants';
import { isNullOrUndefined } from '@/shared/lib/object';

import type { FlowGraphNode, YamlInputMappingEntry } from './pipelineFlow.types';

export {
  getDefaultInputMappingOfTool,
  getEnumList,
  getInputMappingDefaultValue,
  getRequiredInputsAndTooltips,
} from './flowEditorInputMapping.helpers';

/**
 * Node-id minting and the compiler's id grammar live in
 * `./nodeIdentity.helpers.ts` — split out for the same §3.5 400-line reason
 * as `flowEditorInputMapping.helpers` above, and re-exported here so the
 * ~29 callers keep the baseline's single `flowEditor.helpers` import
 * surface.
 */
export { generateNodeIdByType, getInitialNodeId, isCompilerLegalNodeId } from './nodeIdentity.helpers';

interface MeasurableFlowNode {
  readonly id: string;
  readonly measured?: unknown;
  readonly style?: unknown;
  readonly [key: string]: unknown;
}

export const measureNodes = <TNode extends MeasurableFlowNode>(
  nodes: readonly TNode[],
  zoom: number,
  editorRef: { readonly current: HTMLElement | null },
): TNode[] =>
  nodes.map(node => {
    const nodeEl = editorRef.current?.querySelector(`[data-id="${node.id}"]`);
    if (!nodeEl) return node;

    const rect = nodeEl.getBoundingClientRect();
    return {
      ...node,
      measured: {
        width: Math.ceil(rect.width / zoom),
        height: Math.ceil(rect.height / zoom),
      },
    };
  });

export const convertNode = <TNode extends MeasurableFlowNode>(
  node: TNode,
  layoutVersion: string | undefined,
): TNode => {
  if (!layoutVersion) {
    return { ...node, measured: undefined, style: undefined };
  }
  return node;
};

interface YamlDocLike {
  readonly nodes?: readonly { readonly id: string; readonly [key: string]: unknown }[];
  readonly [key: string]: unknown;
}

export const updateNode = <TDoc extends YamlDocLike>(
  id: string,
  yamlJsonObject: TDoc | undefined,
  setYamlJsonObject: (next: TDoc) => void,
  updateCallback: (node: NonNullable<TDoc['nodes']>[number]) => NonNullable<TDoc['nodes']>[number],
): void => {
  const oldNodes = [...(yamlJsonObject?.nodes ?? [])];
  const index = oldNodes.findIndex(node => node.id === id);

  if (index !== -1) {
    const newNodes = oldNodes.filter(node => node.id !== id);
    const target = oldNodes[index];
    if (target) {
      const updatedNode = updateCallback(target);
      newNodes.splice(index, 0, updatedNode);
    }
    setYamlJsonObject({
      ...(yamlJsonObject ?? ({} as TDoc)),
      nodes: newNodes,
    });
  }
};

export const updateYamlNode = <TDoc extends YamlDocLike>(
  id: string,
  field: string,
  value: unknown,
  yamlJsonObject: TDoc | undefined,
  setYamlJsonObject: (next: TDoc) => void,
): void => {
  updateNode(id, yamlJsonObject, setYamlJsonObject, node => ({ ...node, [field]: value }));
};

/** Only verified node-edit deletion markers are normalized; nested YAML values remain strict. */
const NODE_DELETION_FIELDS: readonly string[] = ['transition', 'toolkit_name', 'tool', 'input_mapping', 'loop_toolkit_name', 'loop_tool', 'variables_mapping'];

export const batchUpdateYamlNode = <TDoc extends YamlDocLike>(
  id: string,
  value: Record<string, unknown> = {},
  yamlJsonObject: TDoc | undefined,
  setYamlJsonObject: (next: TDoc) => void,
  replace = false,
): void => {
  updateNode(id, yamlJsonObject, setYamlJsonObject, node => {
    const updated = { ...(!replace ? node : {}), ...value };
    for (const key of NODE_DELETION_FIELDS) {
      if (Object.hasOwn(value, key) && value[key] === undefined) delete updated[key];
    }
    return updated as typeof node;
  });
};

export const updateYamlNodeInputMappingVariable = <TDoc extends YamlDocLike>(
  id: string,
  variable: string,
  value: YamlInputMappingEntry,
  yamlJsonObject: TDoc | undefined,
  setYamlJsonObject: (next: TDoc) => void,
  dataType?: string,
): void => {
  const v: { type: string; value: unknown } = { type: value.type, value: value.value };

  if (v.type === 'fixed') {
    if (dataType === 'integer' || dataType === 'number') {
      try {
        v.value = JSON.parse(v.value as string) as unknown;
      } catch {
        /* do nothing */
      }
    } else if (dataType === 'boolean') {
      v.value = v.value === true || v.value === 'true';
    }
  }

  updateNode(id, yamlJsonObject, setYamlJsonObject, node => ({
    ...node,
    input_mapping: {
      ...(node['input_mapping'] as Record<string, unknown> | undefined),
      [variable]: v,
    },
  }));
};

export const removeYamlNodeVariablesMapping = <TDoc extends YamlDocLike>(
  id: string,
  output: string,
  yamlJsonObject: TDoc | undefined,
  setYamlJsonObject: (next: TDoc) => void,
): void => {
  updateNode(id, yamlJsonObject, setYamlJsonObject, node => {
    const clonedVariablesMapping = structuredClone(
      (node['variables_mapping'] as Record<string, unknown> | undefined) ?? {},
    );
    delete clonedVariablesMapping[output];

    return { ...node, variables_mapping: { ...clonedVariablesMapping } };
  });
};

interface YamlNodeShape {
  readonly transition?: string;
  readonly condition?: unknown;
  readonly decision?: unknown;
}

export const getNodeTypeFlags = (
  nodeId: string,
  sourceHandle: string | null | undefined,
  yamlNode: YamlNodeShape | undefined,
): {
  isFromConditionNode: boolean;
  isFromDecisionNode: boolean;
  isFromRouterHandle: boolean;
  isFromHitlHandle: boolean;
} => ({
  isFromConditionNode: nodeId.endsWith(CONDITION_NODE_ID_SUFFIX) || Boolean(yamlNode?.condition),
  isFromDecisionNode: nodeId.endsWith(DECISION_NODE_ID_SUFFIX) || Boolean(yamlNode?.decision),
  isFromRouterHandle: Boolean(sourceHandle?.startsWith(ROUTER_HANDLE_ID_SUFFIX)),
  isFromHitlHandle: Boolean(sourceHandle?.startsWith(HITL_HANDLE_ID_SUFFIX)),
});

export const getTargetNodeTypeFlags = (
  node: FlowGraphNode,
): { isTargetConditionNode: boolean; isTargetDecisionNode: boolean; isTargetEndNode: boolean } => ({
  isTargetConditionNode: node.id.endsWith(CONDITION_NODE_ID_SUFFIX),
  isTargetDecisionNode: node.id.endsWith(DECISION_NODE_ID_SUFFIX),
  isTargetEndNode: node.id === PipelineNodeTypes.End,
});

/** Checks if source is a special node type (condition, decision, or router/HITL handle). */
const isSpecialSourceNode = ({
  isFromConditionNode,
  isFromDecisionNode,
  isFromRouterHandle,
  isFromHitlHandle,
}: {
  readonly isFromConditionNode: boolean;
  readonly isFromDecisionNode: boolean;
  readonly isFromRouterHandle: boolean;
  readonly isFromHitlHandle: boolean;
}): boolean => isFromConditionNode || isFromDecisionNode || isFromRouterHandle || isFromHitlHandle;

/** Checks if target is a special node type (condition or decision). */
const isSpecialTargetNode = ({
  isTargetConditionNode,
  isTargetDecisionNode,
}: {
  readonly isTargetConditionNode: boolean;
  readonly isTargetDecisionNode: boolean;
}): boolean => isTargetConditionNode || isTargetDecisionNode;

export const canConnectToTarget = (
  sourceFlags: ReturnType<typeof getNodeTypeFlags>,
  targetFlags: ReturnType<typeof getTargetNodeTypeFlags>,
  sourceYamlNode: YamlNodeShape | undefined,
): boolean => {
  const { isTargetEndNode } = targetFlags;

  // Special nodes cannot connect to other special nodes
  if (isSpecialSourceNode(sourceFlags) && isSpecialTargetNode(targetFlags)) {
    return false;
  }

  // To End nodes: only if source has no existing transition/condition/decision or transition = END
  if (isTargetEndNode && sourceYamlNode) {
    if (
      (sourceYamlNode.transition && sourceYamlNode.transition !== PipelineNodeTypes.End) ||
      sourceYamlNode.condition ||
      sourceYamlNode.decision
    ) {
      return false;
    }
  }

  return true;
};

export const canCreateNodeType = (
  nodeType: string,
  sourceFlags: ReturnType<typeof getNodeTypeFlags>,
): boolean => {
  if (!isFixedParallelAuthoringAllowed(nodeType)) return false;
  // Cannot create special node types (Condition/Decision) from special source nodes
  if (
    (nodeType === PipelineNodeTypes.Condition || nodeType === PipelineNodeTypes.Decision) &&
    isSpecialSourceNode(sourceFlags)
  ) {
    return false;
  }

  return true;
};

export const getAllowedNodeTypes = (): readonly string[] =>
  (Object.keys(PipelineNodeTypes) as (keyof typeof PipelineNodeTypes)[])
    .sort()
    .filter(
      key =>
        PipelineNodeTypes[key] !== PipelineNodeTypes.End &&
        PipelineNodeTypes[key] !== PipelineNodeTypes.Ghost &&
        PipelineNodeTypes[key] !== PipelineNodeTypes.Default &&
        PipelineNodeTypes[key] !== PipelineNodeTypes.Function,
    )
    .map(key => PipelineNodeTypes[key]);

/**
 * Uses `||`, not `??`, deliberately: a tool object with an explicit
 * empty-string `name` (or `description`) must still fall through to the
 * next candidate, or the tool renders with a blank label instead of its
 * description/path.
 */
export const getToolName = (tool: string | { readonly name?: string; readonly description?: string; readonly path?: string }): string =>
  typeof tool === 'string' ? tool : (tool.name || tool.description || tool.path || '');

/**
 * Canvas width every node card is laid out at — the same 460 the dagre
 * auto-arrange pass feeds `g.setNode` with (`layout.helpers.ts`), so
 * placement and auto-arrange agree on a card's footprint.
 */
export const NODE_CARD_WIDTH = 460;
/** Vertical breathing room left between a newly added card and the one above it. */
export const NEW_NODE_VERTICAL_GAP = 40;
/** Height assumed for a node type `NodeHeightMap` does not cover — the same fallback `layout.helpers.ts` uses. */
const DEFAULT_NODE_CARD_HEIGHT = 500;

interface PlacedFlowNode {
  readonly type?: string | undefined;
  readonly position: { readonly x: number; readonly y: number };
  readonly measured?: { readonly height?: number | undefined } | null | undefined;
}

/**
 * A card's rendered height is not knowable before React Flow has rendered
 * it, so this uses the same two sources the dagre auto-arrange pass uses
 * (`layout.helpers.ts:computeNodeHeight`): the node's own `measured.height`
 * once React Flow has measured it on the canvas, and otherwise the
 * per-type `NodeHeightMap` estimate (500 for unknown types). The node being
 * added has no rendered card yet by definition, so only the estimate is
 * available for it.
 */
const estimatedNodeHeight = (node: PlacedFlowNode): number =>
  node.measured?.height ?? NodeHeightMap[node.type ?? ''] ?? DEFAULT_NODE_CARD_HEIGHT;

/**
 * Places a newly added node at `xStartPos`/`yStartPos` (the viewport
 * centre) when that leaves its card clear of every existing card, and
 * otherwise stacks it below the lowest existing card.
 *
 * The previous rule only dodged an EXACT position collision (a 60px
 * diagonal nudge per coincident top-left), which is far smaller than a
 * card — several hundred px tall, `NODE_CARD_WIDTH` wide — so two or three
 * nodes added from the menu were drawn on top of each other.
 */
export const calculatePositionForNewNode = (
  xStartPos: number,
  yStartPos: number,
  flowNodes: readonly PlacedFlowNode[],
  newNodeType?: string,
): { xPos: number; yPos: number } => {
  const newHeight = NodeHeightMap[newNodeType ?? ''] ?? DEFAULT_NODE_CARD_HEIGHT;
  const overlaps = flowNodes.some(node => {
    const height = estimatedNodeHeight(node);
    return (
      xStartPos < node.position.x + NODE_CARD_WIDTH &&
      xStartPos + NODE_CARD_WIDTH > node.position.x &&
      yStartPos < node.position.y + height &&
      yStartPos + newHeight > node.position.y
    );
  });

  if (!overlaps) {
    return { xPos: xStartPos, yPos: yStartPos };
  }

  // Below every existing card, so the result cannot overlap any of them.
  const lowestBottom = Math.max(...flowNodes.map(node => node.position.y + estimatedNodeHeight(node)));
  return { xPos: xStartPos, yPos: lowestBottom + NEW_NODE_VERTICAL_GAP };
};


export const formatFStringValue = (value: unknown): unknown => {
  try {
    if (typeof value === 'string' || isNullOrUndefined(value)) {
      return value;
    }
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
};

