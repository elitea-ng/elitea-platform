import { describe, expect, it } from 'vitest';
import type { YamlPipelineDocument, YamlPipelineNode } from './flow-editor/helpers/pipelineFlow.types';
import { graphMapIssues, MAP_WORKER_TYPES, mapOwnedWorkerChannels } from './graphMapAdmission.helpers';

// T-2A3: the Map v2 shape (On Error, Join) and the worker families a Map item
// runs on the shared fan-out adapter, mirroring map_yaml.rs, compiler.rs
// validate_fanout_joins and Main's PIPELINE_MAP_FORM_UNSUPPORTED.

const v1: YamlPipelineNode = { id: 'map_items', type: 'map', worker: 'worker', source: 'items', item: 'entity', index: 'item_index',
  outputs: ['rendered'], destination: 'mapped', max_items: 64, max_concurrency: 4, reduction: 'ordered_collection', transition: 'END' };
const { reduction: _reduction, ...v2 } = v1;
const worker: YamlPipelineNode = { id: 'worker', type: 'state_modifier', input: ['entity', 'item_index'], output: ['rendered'], template: '{{ entity }}' };
const state = { items: 'list', mapped: 'list', keyedResults: 'dict', rendered: 'str' };
const graph = (node: YamlPipelineNode, child: YamlPipelineNode = worker): YamlPipelineDocument =>
  ({ entry_point: node.id, state, nodes: [node, child] });
const fields = (document: YamlPipelineDocument): readonly string[] => graphMapIssues(document).map((issue) => issue.field);

describe('Map v2 policy and worker families', () => {
  it('admits a State Modifier, a saved Agent and an LLM worker, and owns their item channels', () => {
    expect(MAP_WORKER_TYPES).toEqual(['state_modifier', 'agent', 'llm']);
    for (const type of MAP_WORKER_TYPES) {
      const child = { ...worker, type, input_mapping: { task: { type: 'fixed', value: 'render' } } };
      expect(fields(graph(v1, child))).toEqual([]);
      expect(mapOwnedWorkerChannels(graph(v1, child), 'worker')).toEqual(['entity', 'item_index']);
    }
  });
  it('accepts the v2 shape, its defaults and every join that can land', () => {
    expect(fields(graph(v2))).toEqual([]);
    expect(fields(graph({ ...v2, on_error: 'continue', join: 'append' }))).toEqual([]);
    expect(fields(graph({ ...v2, on_error: 'continue', join: 'keyed', destination: 'keyedResults' }))).toEqual([]);
    expect(fields(graph({ ...v2, on_error: 'stop', join: 'keyed', destination: 'keyedResults' }))).toEqual([]);
  });
  it.each([
    [{ ...v1, join: 'append' }, 'reduction'],
    [{ ...v1, on_error: 'stop' }, 'reduction'],
    [{ ...v2, on_error: 'stop', join: 'merge', destination: 'keyedResults' }, 'join'],
    [{ ...v2, join: 'merge', destination: 'keyedResults' }, 'join'],
    [{ ...v2, join: 'keyed' }, 'destination'],
    [{ ...v2, join: 'append', destination: 'keyedResults' }, 'destination'],
    [{ ...v2, on_error: 'retry' }, 'on_error'],
    [{ ...v2, join: 'zip' }, 'join'],
  ])('refuses a Map policy that cannot land (%#) at %s', (node, field) => {
    expect(fields(graph(node))).toContain(field);
  });
  it('lets an LLM worker map its task only from the item, index or broadcasts', () => {
    const llm = { ...worker, type: 'llm', input_mapping: { task: { type: 'variable', value: 'rendered' } } };
    expect(fields(graph(v1, llm))).toContain('worker.input_mapping.task');
  });
});
