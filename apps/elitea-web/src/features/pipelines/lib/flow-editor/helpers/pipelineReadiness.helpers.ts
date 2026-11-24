/**
 * Ported from `EliteaUI` `features/pipelines/flow-editor/lib/helpers/pipelineReadiness.helpers.js`.
 * Computes the disabled-reason tooltip for the `"Test Pipeline"` toolbar button.
 */
import { t } from '@/shared/i18n';
import { normalizePipelineNodeIdentifiers } from '@/shared/lib/pipelineNodeIdentifiers';

import { blockingIssues, collectGraphAdmissionIssues } from '../../graphAdmission.helpers';
import { FlowEditorConstants } from '../constants';
import { parseYaml } from './parsePipeline.helpers';
import type { YamlPipelineDocument } from './pipelineFlow.types';

const { PipelineNodeTypes } = FlowEditorConstants;

export function getUnsavedTestReason(): string {
  return t('features.pipelines.testPanel.blockReason.unsaved', 'Save the pipeline to test it');
}

export function getNoNodesTestReason(): string {
  return t('features.pipelines.testPanel.blockReason.noNodes', 'Add and configure at least one node connected to End.');
}

export function getNoEntryPointTestReason(): string {
  return t('features.pipelines.testPanel.blockReason.noEntryPoint', 'Set an entry point to test the pipeline.');
}

export function getNoPathToEndTestReason(): string {
  return t('features.pipelines.testPanel.blockReason.noPathToEnd', 'Connect the entry point to End to test the pipeline.');
}

export function getNodeNotConfiguredReason(nodeId: string, issue: string): string {
  return t('features.pipelines.testPanel.blockReason.nodeNotConfigured', 'Configure node "{{nodeId}}": {{issue}}', {
    nodeId,
    issue,
  });
}

interface EdgeLike {
  readonly source?: string | undefined;
  readonly target?: string | undefined;
}

export function isEndReachable(edges: readonly EdgeLike[], startId: string): boolean {
  const targetsBySource = new Map<string, string[]>();
  for (const edge of edges) {
    if (!edge.source || !edge.target) continue;
    const list = targetsBySource.get(edge.source);
    if (list) list.push(edge.target);
    else targetsBySource.set(edge.source, [edge.target]);
  }

  const visited = new Set<string>([startId]);
  const queue: string[] = [startId];
  while (queue.length > 0) {
    const current = queue.shift() as string;
    if (current === PipelineNodeTypes.End) return true;
    const targets = targetsBySource.get(current);
    if (!targets) continue;
    for (const target of targets) {
      if (!visited.has(target)) {
        visited.add(target);
        queue.push(target);
      }
    }
  }
  return false;
}

interface YamlNodeLike {
  readonly id?: string | undefined;
  readonly type?: string | undefined;
  readonly condition?: unknown;
  readonly decision?: unknown;
  readonly default_output?: unknown;
}

function getBranchingConfig(node: YamlNodeLike): Readonly<Record<string, unknown>> | null {
  if (node.type === PipelineNodeTypes.Router || node.type === PipelineNodeTypes.Hitl) return null;
  if (node.condition && typeof node.condition === 'object') return node.condition as Readonly<Record<string, unknown>>;
  if (node.decision && typeof node.decision === 'object') return node.decision as Readonly<Record<string, unknown>>;
  if (node.type === PipelineNodeTypes.Decision) return node as unknown as Readonly<Record<string, unknown>>;
  return null;
}

function getImplicitEndEdges(yamlNodes: readonly YamlNodeLike[]): readonly EdgeLike[] {
  const edges: EdgeLike[] = [];
  for (const node of yamlNodes) {
    if (typeof node.id !== 'string' || node.id.length === 0) continue;
    const branchingConfig = getBranchingConfig(node);
    if (branchingConfig && !Object.hasOwn(branchingConfig, 'default_output')) {
      edges.push({ source: node.id, target: PipelineNodeTypes.End });
    }
  }
  return edges;
}

function parseEdgesSafely(yamlJsonObject: YamlPipelineDocument): readonly EdgeLike[] | null {
  try {
    return parseYaml(yamlJsonObject).edges;
  } catch {
    return null;
  }
}

function findFirstNodeIssue(
  yamlNodes: readonly YamlNodeLike[],
  nodeIssuesById: Readonly<Record<string, readonly string[]>>,
): string {
  for (const node of yamlNodes) {
    if (typeof node.id !== 'string' || node.id.length === 0) continue;
    if (!Object.hasOwn(nodeIssuesById, node.id)) continue;
    const firstIssue = nodeIssuesById[node.id]?.[0];
    if (firstIssue) return getNodeNotConfiguredReason(node.id, firstIssue);
  }
  return '';
}

/** Collects blocking graph-admission issues grouped by node ID into a null-prototype lookup map. */
export function collectBlockingNodeIssuesById(
  yamlJsonObject: YamlPipelineDocument | null | undefined,
): Readonly<Record<string, readonly string[]>> {
  const grouped = Object.create(null) as Record<string, string[]>;
  if (!yamlJsonObject) return grouped;
  for (const issue of blockingIssues(collectGraphAdmissionIssues(yamlJsonObject))) {
    if (!issue.nodeId) continue;
    const list = grouped[issue.nodeId];
    if (list) list.push(issue.message);
    else grouped[issue.nodeId] = [issue.message];
  }
  return grouped;
}

/**
 * Returns `''` when the pipeline flow can be tested, otherwise a human-readable
 * disabled-reason tooltip string for the `"Test Pipeline"` button.
 */
export function getTestBlockReason(
  yamlJsonObject: YamlPipelineDocument | null | undefined,
  nodeIssuesById: Readonly<Record<string, readonly string[]>> = {},
): string {
  if (!yamlJsonObject) return getNoNodesTestReason();
  const normalized = normalizePipelineNodeIdentifiers(yamlJsonObject);
  const rawNodes = (normalized as { readonly nodes?: readonly (YamlNodeLike | null | undefined)[] }).nodes ?? [];
  const yamlNodes = rawNodes.filter((node): node is YamlNodeLike => Boolean(node));
  const entryPoint = (normalized as { readonly entry_point?: unknown }).entry_point;

  if (yamlNodes.length === 0) return getNoNodesTestReason();
  if (typeof entryPoint !== 'string' || !yamlNodes.some((node) => node.id === entryPoint)) {
    return getNoEntryPointTestReason();
  }

  const edges = parseEdgesSafely(normalized);
  if (edges === null || !isEndReachable([...edges, ...getImplicitEndEdges(yamlNodes)], entryPoint)) {
    return getNoPathToEndTestReason();
  }

  return findFirstNodeIssue(yamlNodes, nodeIssuesById);
}
