/**
 * The error output handle of a node with `settings.on_error: error_output`
 * (5b-1, PLAN §3.9). Its edge is the node's `settings.error_transition` and
 * nothing else (C6): connecting writes only that key, never `transition`;
 * deleting the edge clears it; deleting the target node clears it.
 */
import { NODE_SETTINGS_KEY, readNodeSettings, writeNodeSettings } from '../../nodeSettings.helpers';
import { EDGE_PREFIX, PipelineNodeTypes } from '../constants/flowEditor.constants';
import { checkAndAddEdge } from './parsePipelineGraphPrimitives.helpers';
import type { FlowGraphEdge, YamlPipelineDocument, YamlPipelineNode } from './pipelineFlow.types';

/** The source handle id of the error output, on every node that shows one. */
export const ERROR_OUTPUT_HANDLE = 'node_error_output';

export const errorRouteEdgeId = (source: string, target: string): string => `${EDGE_PREFIX}${source}error_output---${target}`;

/** The node's error route target, when it routes failures (`error_output`). */
export function errorTransitionOf(node: YamlPipelineNode | undefined): string | undefined {
  const settings = readNodeSettings(node);
  return settings.on_error === 'error_output' ? settings.error_transition : undefined;
}

/** Adds the error route edge while the flow is built from YAML; answers the branch to walk. */
export function addErrorRouteEdge(node: YamlPipelineNode, edges: FlowGraphEdge[]): readonly string[] {
  const target = errorTransitionOf(node);
  if (target === undefined) return [];
  checkAndAddEdge({ edges, edgeId: errorRouteEdgeId(node.id, target), source: node.id, sourceHandle: ERROR_OUTPUT_HANDLE, target });
  return target === PipelineNodeTypes.End ? [] : [target];
}

function withSettings(node: YamlPipelineNode, change: Readonly<Record<string, unknown>>): YamlPipelineNode {
  const next: Record<string, unknown> = { ...node };
  const settings = writeNodeSettings(node[NODE_SETTINGS_KEY] as Readonly<Record<string, unknown>> | undefined, change);
  if (settings === undefined) delete next[NODE_SETTINGS_KEY];
  else next[NODE_SETTINGS_KEY] = settings;
  return next as unknown as YamlPipelineNode;
}

function updateNode(document: YamlPipelineDocument, id: string, update: (node: YamlPipelineNode) => YamlPipelineNode): YamlPipelineDocument {
  if (document.nodes === undefined) return document;
  return { ...document, nodes: document.nodes.map((node) => (node.id === id ? update(node) : node)) };
}

/** Connecting the error handle: `settings.error_transition` only (C6). */
export function connectErrorRoute(document: YamlPipelineDocument, source: string, target: string): YamlPipelineDocument {
  return updateNode(document, source, (node) => withSettings(node, { on_error: 'error_output', error_transition: target }));
}

/** Deleting the error edge clears the target; the panel then names the missing route. */
export function clearErrorRoute(document: YamlPipelineDocument, source: string): YamlPipelineDocument {
  return updateNode(document, source, (node) => withSettings(node, { error_transition: undefined }));
}

/** Deleting a node clears every error route that targeted it (C6). */
export function clearDeletedErrorTarget(node: YamlPipelineNode, deletedId: string): YamlPipelineNode {
  return errorTransitionOf(node) === deletedId ? withSettings(node, { error_transition: undefined }) : node;
}
