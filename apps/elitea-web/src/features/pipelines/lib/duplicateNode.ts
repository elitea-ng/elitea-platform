import { PipelineNodeTypes } from './flow-editor/constants/flowEditor.constants';
import { isDeprecatedNodeType } from './flow-editor/helpers/node.helpers';
import type { YamlPipelineNode } from './flow-editor/helpers/pipelineFlow.types';
import type { FlowNode } from './flow-editor/reactFlowTypes';

const NON_DUPLICABLE_TYPES: readonly string[] = [PipelineNodeTypes.End, PipelineNodeTypes.Ghost];

const DEFAULT_DUPLICATE_OFFSET = { x: 40, y: 40 } as const;

export interface DuplicateNodeIdOptions {
  readonly name: string;
  readonly existingNodeIds?: readonly string[] | undefined;
  readonly toolNames?: readonly string[] | undefined;
}

export interface DuplicatePositionOffset {
  readonly x: number;
  readonly y: number;
}

/**
 * Returns whether a node of the given type may be duplicated.
 * Rejects synthetic `END` / `ghost` nodes and deprecated legacy node types.
 */
export function canDuplicateNodeType(type: string | undefined): boolean {
  if (!type) return false;
  if (NON_DUPLICABLE_TYPES.includes(type)) return false;
  return !isDeprecatedNodeType(type);
}

/**
 * Generates `<name>_copy`, `<name>_copy_2`, `<name>_copy_3`, ... avoiding collisions
 * with existing node IDs and pipeline tool names (case-insensitive check on reserved names).
 */
export function getDuplicateNodeId({
  name,
  existingNodeIds = [],
  toolNames = [],
}: DuplicateNodeIdOptions): string {
  const taken = new Set<string>([...existingNodeIds, ...toolNames]);
  const baseName = `${name}_copy`;
  if (!taken.has(baseName)) return baseName;

  for (let index = 2; ; index += 1) {
    const candidate = `${baseName}_${index}`;
    if (!taken.has(candidate)) return candidate;
  }
}

function stripErrorTransition(node: Record<string, unknown>): Record<string, unknown> {
  const settings = node['settings'];
  if (!settings || typeof settings !== 'object' || Array.isArray(settings)) return node;
  const settingsRecord = settings as Record<string, unknown>;
  if (!('error_transition' in settingsRecord)) return node;

  const { error_transition: _removed, ...restSettings } = settingsRecord;
  if (Object.keys(restSettings).length === 0) {
    const { settings: _removedSettings, ...restNode } = node;
    return restNode;
  }
  return { ...node, settings: restSettings };
}

/**
 * Clears all outgoing connection fields so a duplicated node starts disconnected:
 * - `transition`
 * - `settings.error_transition` (5b-1 error-output route)
 * - `Router` `routes` + `default_output`
 * - `Decision` `nodes` + `default_output`
 * - `Hitl` `routes` (`approve`, `edit`, `reject`)
 */
export function clearNodeConnections(yamlNode: YamlPipelineNode): YamlPipelineNode {
  const { transition: _transition, ...withoutTransition } = yamlNode;
  const cleaned = stripErrorTransition(withoutTransition);

  switch (yamlNode.type ?? '') {
    case PipelineNodeTypes.Router:
      return { ...cleaned, routes: [], default_output: '' } as unknown as YamlPipelineNode;
    case PipelineNodeTypes.Decision:
      return { ...cleaned, nodes: [], default_output: '' } as unknown as YamlPipelineNode;
    case PipelineNodeTypes.Hitl:
      return { ...cleaned, routes: { approve: '', edit: '', reject: '' } } as unknown as YamlPipelineNode;
    default:
      return cleaned as unknown as YamlPipelineNode;
  }
}

/**
 * Deep-clones a YAML pipeline node under `newId` with all outgoing connections cleared.
 */
export function duplicateYamlNode(yamlNode: YamlPipelineNode, newId: string): YamlPipelineNode {
  const cloned = structuredClone(yamlNode);
  return clearNodeConnections({ ...cloned, id: newId });
}

/**
 * Clones a React Flow node under `newId`, offsetting its position so the copy is visibly distinct.
 */
export function duplicateFlowNode(
  flowNode: FlowNode,
  newId: string,
  offset: DuplicatePositionOffset = DEFAULT_DUPLICATE_OFFSET,
): FlowNode {
  const clonedData = structuredClone(flowNode.data);
  return {
    ...flowNode,
    id: newId,
    selected: true,
    position: {
      x: flowNode.position.x + offset.x,
      y: flowNode.position.y + offset.y,
    },
    data: {
      ...clonedData,
      label: newId,
    },
  };
}
