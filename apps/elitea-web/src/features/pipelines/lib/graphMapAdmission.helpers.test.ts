import { describe, expect, it } from 'vitest';
import type { YamlPipelineDocument, YamlPipelineNode } from './flow-editor/helpers/pipelineFlow.types';
import { graphMapIssues, mapOwnedWorkerChannels } from './graphMapAdmission.helpers';
import { collectGraphAdmissionIssues } from './graphAdmission.helpers';

const map: YamlPipelineNode = { id: 'map_items', type: 'map', worker: 'worker', source: 'items', item: 'entity', index: 'item_index',
  outputs: ['rendered'], destination: 'mapped', max_items: 64, max_concurrency: 4, reduction: 'ordered_collection', broadcast: ['prefix'], transition: 'END' };
const worker: YamlPipelineNode = { id: 'worker', type: 'state_modifier', input: ['entity', 'item_index', 'prefix'], output: ['rendered'], template: '{{ entity }}' };
const state = { items: { type: 'list', value: [null, 1, { arbitrary: [] }] }, mapped: 'list', rendered: 'str', prefix: 'str', foreign: 'str' };
const graph = (value: unknown = map, child: unknown = worker): YamlPipelineDocument => {
  const node = value as YamlPipelineNode;
  return { entry_point: node.id, state, nodes: [node, child as YamlPipelineNode] };
};
const fields = (document: YamlPipelineDocument): readonly string[] => graphMapIssues(document).map((issue) => issue.field);

describe('Map explicit ownership and channel contract', () => {
  it('accepts opaque list items without inventing global local-channel descriptors', () => {
    expect(fields(graph())).toEqual([]);
    expect(mapOwnedWorkerChannels(graph(), 'worker')).toEqual(['entity', 'item_index']);
    expect(graph().state).not.toHaveProperty('entity');
  });
  it('permits a frozen source to be the destination and approved broadcasts to overlap business roots', () => {
    expect(fields(graph({ ...map, destination: 'items', broadcast: ['items', 'mapped', 'rendered'] }, { ...worker, input: ['entity', 'item_index', 'items', 'mapped', 'rendered'] }))).toEqual([]);
  });
  it('keeps production type refusal and allows local reads only on the exact owned worker', () => {
    const issues = collectGraphAdmissionIssues(graph());
    expect(issues.filter((issue) => issue.rule === 'node.type').map((issue) => issue.nodeId)).toEqual(['map_items']);
    expect(issues.filter((issue) => issue.rule === 'node.state-reference')).toEqual([]);
    const foreign = { id: 'foreign_reader', type: 'state_modifier', input: ['entity'], template: 'x' };
    expect(collectGraphAdmissionIssues({ ...graph(), nodes: [map, worker, foreign] })
      .some((issue) => issue.nodeId === 'foreign_reader' && issue.rule === 'node.state-reference')).toBe(true);
  });
  it.each([
    [{ item: 'prefix' }, 'item'], [{ index: 'entity' }, 'index'], [{ item: 'messages' }, 'item'],
    [{ item: '__elitea_hitl_resume_v1' }, 'item'], [{ source: 'prefix' }, 'source'], [{ destination: 'missing' }, 'destination'],
    [{ outputs: [] }, 'outputs'], [{ outputs: ['rendered', 'rendered'] }, 'outputs[1]'],
    [{ outputs: ['foreign'] }, 'outputs'], [{ outputs: [null] }, 'outputs[0]'],
    [{ broadcast: ['missing'] }, 'broadcast[0]'], [{ broadcast: ['prefix', 'prefix'] }, 'broadcast[1]'],
    [{ max_items: 0 }, 'max_items'], [{ max_items: 65 }, 'max_items'], [{ max_items: 1.5 }, 'max_items'],
    [{ max_concurrency: 9 }, 'max_concurrency'], [{ reduction: 'concat' }, 'reduction'],
    [{ worker: 'missing' }, 'worker'], [{ worker: 'map_items' }, 'worker'], [{ inferred_schema: {} }, 'inferred_schema'],
  ])('refuses exact Map field %s at %s', (patch, field) => {
    expect(fields(graph({ ...map, ...patch }))).toContain(field);
  });
  it.each(['code', 'llm', 'toolkit', 'hitl', 'aggregate', 'parallel'])('refuses unreviewed %s worker effects', (type) => {
    expect(fields(graph(map, { ...worker, type }))).toContain('worker');
  });
  it('rejects duplicate Map ownership and local-channel exposure', () => {
    const document = { ...graph(), nodes: [map, worker, { ...map, id: 'other_map' }] };
    expect(fields(document)).toContain('worker');
    expect(mapOwnedWorkerChannels(document, 'worker')).toEqual([]);
  });
  it('rejects fixed Parallel sharing the same owned worker', () => {
    const document = { ...graph(), nodes: [map, worker, { id: 'parallel', type: 'parallel', branches: [{ id: 'branch_identity', node: 'worker' }] }] };
    expect(fields(document)).toContain('worker');
    expect(mapOwnedWorkerChannels(document, 'worker')).toEqual([]);
  });
  it('rejects worker transition, static pause, parent entry and parent routes', () => {
    expect(fields(graph(map, { ...worker, transition: 'END' }))).toContain('worker');
    expect(fields({ ...graph(), interrupt_after: ['worker'] })).toContain('worker');
    expect(fields({ ...graph(), entry_point: 'worker' })).toContain('worker');
    expect(fields({ ...graph(), nodes: [map, worker, { id: 'route', type: 'printer', transition: 'worker' }] })).toContain('worker');
  });
  it('allows a null worker transition and rejects reads or clean keys outside approved scope', () => {
    expect(fields(graph(map, { ...worker, transition: null }))).toEqual([]);
    expect(fields(graph(map, { ...worker, input: ['foreign'] }))).toContain('worker.input');
    expect(fields(graph(map, { ...worker, variables_to_clean: ['prefix'] }))).toContain('worker.variables_to_clean');
  });
  it('checks Agent variable mappings while keeping saved participant effect proof runtime-owned', () => {
    const child: YamlPipelineNode = { id: 'worker', type: 'agent', tool: 'saved_agent', input: [], output: ['rendered'],
      input_mapping: { task: { type: 'variable', value: 'entity' } } };
    expect(fields(graph(map, child))).toEqual([]);
    expect(fields(graph(map, { ...child, input_mapping: { task: { type: 'variable', value: 'foreign' } } }))).toContain('worker.input_mapping.task');
  });
});
