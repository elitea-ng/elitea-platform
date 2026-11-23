import type { YamlPipelineDocument, YamlPipelineNode } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import type { FlowNode, SetFlowNodes, SetYamlJsonObject } from '../../lib/flow-editor/reactFlowTypes';

const DUPLICATE_OFFSET_PX = 40;

/**
 * Mints a unique compiler-legal node ID based on `baseId` (`foo_copy`, `foo_copy_2`, ...).
 */
export function buildDuplicateNodeId(baseId: string, existingIds: readonly string[]): string {
  const existing = new Set(existingIds);
  const firstCandidate = `${baseId}_copy`;
  if (!existing.has(firstCandidate)) return firstCandidate;
  let index = 2;
  while (existing.has(`${baseId}_copy_${index}`)) {
    index += 1;
  }
  return `${baseId}_copy_${index}`;
}

/**
 * Clones a YAML node with a new ID and strips outgoing transition references so the duplicate
 * starts cleanly without dangling or conflicting edges.
 */
export function cloneYamlNodeForDuplicate(source: YamlPipelineNode, nextId: string): YamlPipelineNode {
  const cloned = structuredClone(source) as Record<string, unknown>;
  cloned['id'] = nextId;
  delete cloned['transition'];
  delete cloned['default_output'];
  delete cloned['routes'];
  delete cloned['nodes'];

  const settings = cloned['settings'];
  if (settings && typeof settings === 'object' && !Array.isArray(settings)) {
    const nextSettings = { ...(settings as Record<string, unknown>) };
    delete nextSettings['error_transition'];
    if (Object.keys(nextSettings).length > 0) {
      cloned['settings'] = nextSettings;
    } else {
      delete cloned['settings'];
    }
  }

  return cloned as unknown as YamlPipelineNode;
}

export interface DuplicateNodeArgs {
  readonly sourceId: string;
  readonly yamlJsonObject: YamlPipelineDocument;
  readonly flowNodes: readonly FlowNode[];
  readonly setYamlJsonObject: SetYamlJsonObject;
  readonly setFlowNodes: SetFlowNodes;
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
}: DuplicateNodeArgs): string | null {
  const sourceYaml = yamlJsonObject.nodes?.find((entry) => entry.id === sourceId);
  const sourceFlow = flowNodes.find((entry) => entry.id === sourceId);
  if (!sourceYaml || !sourceFlow) return null;

  const existingIds = [
    ...(yamlJsonObject.nodes ?? []).map((entry) => entry.id),
    ...flowNodes.map((entry) => entry.id),
  ];
  const nextId = buildDuplicateNodeId(sourceId, existingIds);
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
