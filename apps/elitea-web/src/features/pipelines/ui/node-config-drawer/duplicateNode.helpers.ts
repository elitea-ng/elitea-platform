import { PipelineNodeTypes } from '../../lib/flow-editor/constants/flowEditor.constants';
import { isCompilerLegalNodeId } from '../../lib/flow-editor/helpers/flowEditor.helpers';
import type { YamlPipelineDocument, YamlPipelineNode } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import type { FlowNode, SetFlowNodes, SetYamlJsonObject } from '../../lib/flow-editor/reactFlowTypes';

const DUPLICATE_OFFSET_PX = 40;
const MAX_NODE_ID_BYTES = 128;
const encoder = new TextEncoder();

function normalizeIdBase(baseId: string): string {
  const sanitized = baseId.replace(/[^A-Za-z0-9_.:-]+/g, '_').replace(/^_+|_+$/g, '');
  return sanitized || 'node';
}

function fitIdWithSuffix(base: string, suffix: string): string {
  const suffixBytes = encoder.encode(suffix).length;
  const maxBaseBytes = Math.max(1, MAX_NODE_ID_BYTES - suffixBytes);
  let trimmed = base;
  while (trimmed.length > 1 && encoder.encode(trimmed).length > maxBaseBytes) {
    trimmed = trimmed.slice(0, -1);
  }
  return `${trimmed}${suffix}`;
}

function hasIdCollision(
  candidate: string,
  existingSet: ReadonlySet<string>,
  toolNameSet: ReadonlySet<string>,
): boolean {
  const normalized = candidate.replace(/\s/g, '');
  return existingSet.has(normalized) || toolNameSet.has(normalized);
}

/**
 * Mints a unique, compiler-legal (`<= 128` bytes) node ID based on `baseId`
 * (`foo_copy`, `foo_copy_2`, ...), avoiding collisions with existing node IDs and tool names.
 */
export function buildDuplicateNodeId(
  baseId: string,
  existingIds: readonly string[],
  toolNames: readonly string[] = [],
): string {
  const stem = normalizeIdBase(baseId);
  const existingSet = new Set(existingIds.map((id) => id.replace(/\s/g, '')));
  const toolNameSet = new Set(toolNames.map((name) => name.replace(/\s/g, '')));

  const firstCandidate = fitIdWithSuffix(stem, '_copy');
  if (isCompilerLegalNodeId(firstCandidate) && !hasIdCollision(firstCandidate, existingSet, toolNameSet)) {
    return firstCandidate;
  }

  let index = 2;
  while (index < 10_000) {
    const candidate = fitIdWithSuffix(stem, `_copy_${index}`);
    if (isCompilerLegalNodeId(candidate) && !hasIdCollision(candidate, existingSet, toolNameSet)) {
      return candidate;
    }
    index += 1;
  }
  return fitIdWithSuffix('node', `_copy_${Date.now()}`);
}

function stripErrorTransitionSetting(cloned: Record<string, unknown>): void {
  const settings = cloned['settings'];
  if (!settings || typeof settings !== 'object' || Array.isArray(settings)) return;
  const nextSettings = { ...(settings as Record<string, unknown>) };
  delete nextSettings['error_transition'];
  if (Object.keys(nextSettings).length > 0) {
    cloned['settings'] = nextSettings;
  } else {
    delete cloned['settings'];
  }
}

function resetConnectionsForNodeType(cloned: Record<string, unknown>, nodeType: string): void {
  delete cloned['transition'];
  delete cloned['default_output'];
  delete cloned['routes'];
  delete cloned['nodes'];

  switch (nodeType) {
    case PipelineNodeTypes.Printer:
      cloned['transition'] = PipelineNodeTypes.End;
      break;
    case PipelineNodeTypes.Decision:
      cloned['nodes'] = [];
      cloned['default_output'] = PipelineNodeTypes.End;
      break;
    case PipelineNodeTypes.Hitl:
      cloned['routes'] = { approve: PipelineNodeTypes.End, reject: PipelineNodeTypes.End };
      delete cloned['edit_state_key'];
      break;
    case PipelineNodeTypes.Router:
      cloned['routes'] = [];
      cloned['default_output'] = PipelineNodeTypes.End;
      break;
    case PipelineNodeTypes.Parallel:
      cloned['branches'] = [];
      break;
    case PipelineNodeTypes.Map:
      delete cloned['worker'];
      break;
    default:
      break;
  }
}

/**
 * Clones a YAML node with a new ID and resets outgoing connection fields per node type
 * so the duplicate satisfies graph admission rules without dangling or conflicting edges.
 */
export function cloneYamlNodeForDuplicate(source: YamlPipelineNode, nextId: string): YamlPipelineNode {
  const cloned = structuredClone(source) as Record<string, unknown>;
  cloned['id'] = nextId;
  resetConnectionsForNodeType(cloned, typeof source.type === 'string' ? source.type : '');
  stripErrorTransitionSetting(cloned);
  return cloned as unknown as YamlPipelineNode;
}

export interface DuplicateNodeArgs {
  readonly sourceId: string;
  readonly yamlJsonObject: YamlPipelineDocument;
  readonly flowNodes: readonly FlowNode[];
  readonly setYamlJsonObject: SetYamlJsonObject;
  readonly setFlowNodes: SetFlowNodes;
  readonly toolNames?: readonly string[] | undefined;
}

/**
 * Duplicates a node in both the YAML document and React Flow canvas, selecting the new node.
 * Returns the duplicated node's ID, or `null` if the source node was not found or write was refused.
 */
export function duplicatePipelineNode({
  sourceId,
  yamlJsonObject,
  flowNodes,
  setYamlJsonObject,
  setFlowNodes,
  toolNames = [],
}: DuplicateNodeArgs): string | null {
  const sourceYaml = yamlJsonObject.nodes?.find((entry) => entry.id === sourceId);
  const sourceFlow = flowNodes.find((entry) => entry.id === sourceId);
  if (!sourceYaml || !sourceFlow) return null;

  const existingIds = [
    ...(yamlJsonObject.nodes ?? []).map((entry) => entry.id),
    ...flowNodes.map((entry) => entry.id),
  ];
  const nextId = buildDuplicateNodeId(sourceId, existingIds, toolNames);
  const nextYamlNode = cloneYamlNodeForDuplicate(sourceYaml, nextId);

  const stored = setYamlJsonObject({
    ...yamlJsonObject,
    nodes: [...(yamlJsonObject.nodes ?? []), nextYamlNode],
  });
  if (stored === false) return null;

  const nextFlowNode: FlowNode = {
    ...sourceFlow,
    id: nextId,
    position: {
      x: sourceFlow.position.x + DUPLICATE_OFFSET_PX,
      y: sourceFlow.position.y + DUPLICATE_OFFSET_PX,
    },
    data: {
      ...sourceFlow.data,
      label: nextId,
      isPerforming: false,
    },
    selected: true,
  };

  setFlowNodes((prevNodes) => [
    ...prevNodes.map((node) => ({ ...node, selected: false })),
    nextFlowNode,
  ]);

  return nextId;
}
