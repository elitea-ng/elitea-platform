import { PipelineNodeTypes } from './flow-editor/constants/flowEditor.constants';
import { MAX_NODE_ID_BYTES } from './flow-editor/constants/runtimeContract.constants';
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

function fitNodeIdWithSuffix(base: string, suffix: string): string {
  const maxBaseLength = Math.max(1, MAX_NODE_ID_BYTES - suffix.length);
  return `${base.slice(0, maxBaseLength)}${suffix}`;
}

/**
 * Generates `<name>_copy`, `<name>_copy_2`, `<name>_copy_3`, ... avoiding collisions
 * with existing node IDs and pipeline tool names, clamped to `MAX_NODE_ID_BYTES` (128).
 */
export function getDuplicateNodeId({
  name,
  existingNodeIds = [],
  toolNames = [],
}: DuplicateNodeIdOptions): string {
  const taken = new Set<string>([...existingNodeIds, ...toolNames]);
  const firstCandidate = fitNodeIdWithSuffix(name, '_copy');
  if (!taken.has(firstCandidate)) return firstCandidate;

  for (let index = 2; ; index += 1) {
    const candidate = fitNodeIdWithSuffix(name, `_copy_${index}`);
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
 * Resets outgoing connection fields so a duplicated node starts disconnected from
 * sibling nodes while remaining compiler-admitted (`nodeDefaults.constants.ts` /
 * `deletionOperations.helpers.ts`):
 * - `transition` cleared (except `Printer`, which requires `transition: 'END'`)
 * - `settings.error_transition` stripped (5b-1 error-output route)
 * - `Router` `routes: []` + `default_output: 'END'`
 * - `Decision` `nodes: []` + `default_output: 'END'`
 * - `Hitl` `routes: { approve: 'END', reject: 'END' }` (omitting `edit`)
 */
export function clearNodeConnections(yamlNode: YamlPipelineNode): YamlPipelineNode {
  const { transition: _transition, ...withoutTransition } = yamlNode;
  const cleaned = stripErrorTransition(withoutTransition);

  switch (yamlNode.type ?? '') {
    case PipelineNodeTypes.Printer:
      return { ...cleaned, transition: PipelineNodeTypes.End } as unknown as YamlPipelineNode;
    case PipelineNodeTypes.Router:
      return { ...cleaned, routes: [], default_output: PipelineNodeTypes.End } as unknown as YamlPipelineNode;
    case PipelineNodeTypes.Decision:
      return { ...cleaned, nodes: [], default_output: PipelineNodeTypes.End } as unknown as YamlPipelineNode;
    case PipelineNodeTypes.Hitl:
      return {
        ...cleaned,
        routes: { approve: PipelineNodeTypes.End, reject: PipelineNodeTypes.End },
      } as unknown as YamlPipelineNode;
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
 * Clones a React Flow node under `newId`, offsetting its position so the copy is visibly distinct
 * and stripping transient run-execution markers (`isPerforming`).
 */
export function duplicateFlowNode(
  flowNode: FlowNode,
  newId: string,
  offset: DuplicatePositionOffset = DEFAULT_DUPLICATE_OFFSET,
): FlowNode {
  const { isPerforming: _transientPerforming, ...clonedData } = structuredClone(flowNode.data) as Record<string, unknown>;
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
