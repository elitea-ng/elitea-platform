import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { describe, expect, it } from 'vitest';

import { parallelYaml } from '../__tests__/parallelTestUtils';
import type { YamlPipelineDocument, YamlPipelineNode } from './flow-editor/helpers/pipelineFlow.types';
import { FANOUT_CHILD_FAMILIES, ROUTER_CHILD_TEXT } from './graphExtensions.types';
import { graphMapIssues, MAP_WORKER_TYPES } from './graphMapAdmission.helpers';
import { graphParallelIssues } from './graphParallelAdmission.helpers';
import { parallelOwnedAgent } from './graphParallel.helpers';
import { parsePipelineYamlDocument } from './pipelineYamlDocument.helpers';

/**
 * N2 (W3-2): the fan-out child families parity. `libs/proto/contracts/
 * pipeline-fanout-child-families-v1.json` lists the node types each family
 * runs directly; the Worker compiler and Main's save check assert it too.
 */
interface FamiliesContract {
  readonly parallel_branch: readonly string[];
  readonly map_worker: readonly string[];
  readonly refused_with_own_text: readonly string[];
}
const CONTRACT_PATH = resolve(import.meta.dirname, '../../../../../../libs/proto/contracts/pipeline-fanout-child-families-v1.json');
const contract = JSON.parse(readFileSync(CONTRACT_PATH, 'utf8')) as FamiliesContract;

const parallel = (): YamlPipelineDocument => parsePipelineYamlDocument(parallelYaml).yamlJsonObject;
const withLeftType = (type: string): YamlPipelineDocument => {
  const value = parallel();
  return { ...value, nodes: value.nodes?.map((node) => node.id === 'Agent_left' ? { ...node, type } : node) ?? [] };
};
const map: YamlPipelineNode = { id: 'map_items', type: 'map', worker: 'worker', source: 'items', item: 'entity', index: 'item_index',
  outputs: ['rendered'], destination: 'mapped', max_items: 64, max_concurrency: 4, reduction: 'ordered_collection', transition: 'END' };
const mapWith = (worker: YamlPipelineNode): YamlPipelineDocument =>
  ({ entry_point: 'map_items', state: { items: 'list', mapped: 'list', rendered: 'str' }, nodes: [map, worker] });

describe('fan-out child families match the shared contract', () => {
  it('lists exactly the contract families, one constant for both helpers', () => {
    expect([...FANOUT_CHILD_FAMILIES.parallelBranch].sort()).toEqual([...contract.parallel_branch].sort());
    expect([...FANOUT_CHILD_FAMILIES.mapWorker].sort()).toEqual([...contract.map_worker].sort());
    expect(MAP_WORKER_TYPES).toBe(FANOUT_CHILD_FAMILIES.mapWorker);
    expect(contract.refused_with_own_text).toEqual(['router']);
  });

  it('admits a Code node as a Parallel branch and owns it', () => {
    expect(graphParallelIssues(withLeftType('code'))).toEqual([]);
    expect(parallelOwnedAgent(withLeftType('code'), 'extension', 0)?.type).toBe('code');
  });

  it('admits a Code node as a Map worker', () => {
    const worker: YamlPipelineNode = { id: 'worker', type: 'code', code: { type: 'fixed', value: 'print(1)' }, input: ['entity', 'item_index'], output: ['rendered'] };
    expect(graphMapIssues(mapWith(worker))).toEqual([]);
  });

  it('refuses a direct Router child in either family with its own text', () => {
    expect(graphParallelIssues(withLeftType('router')).map((issue) => issue.message)).toContain(
      `branches[0].node: Node "Agent_left" is a "router" node, but ${ROUTER_CHILD_TEXT}.`,
    );
    const router: YamlPipelineNode = { id: 'worker', type: 'router', routes: ['END'], default_output: 'END', input: ['entity'] };
    expect(graphMapIssues(mapWith(router)).map((issue) => issue.message)).toContain(
      `worker: Node "worker" is a "router" node, but ${ROUTER_CHILD_TEXT}.`,
    );
  });
});
