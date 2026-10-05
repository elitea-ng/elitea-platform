import { describe, expect, it } from 'vitest';
import { parallelYaml } from '../__tests__/parallelTestUtils';
import { collectGraphAdmissionIssues } from './graphAdmission.helpers';
import { graphParallelIssues } from './graphParallelAdmission.helpers';
import { parallelOwnedAgent } from './graphParallel.helpers';
import { parsePipelineYamlDocument } from './pipelineYamlDocument.helpers';
import type { YamlPipelineDocument } from './flow-editor/helpers/pipelineFlow.types';

function document(yaml = parallelYaml): YamlPipelineDocument {
  return parsePipelineYamlDocument(yaml).yamlJsonObject;
}
function changed(patch: Record<string, unknown>): YamlPipelineDocument {
  const value = document();
  return { ...value, nodes: value.nodes?.map((node) => node.id === 'extension' ? { ...node, ...patch } : node) ?? [] };
}
const refs = [{ id: 'left', node: 'Agent_left' }, { id: 'right', node: 'Agent_right' }];
describe('strict fixed Parallel admission', () => {
  it('admits ordered declared Agent branches and an explicit list output without parent input or reducers', () => {
    expect(graphParallelIssues(document())).toEqual([]);
    expect(collectGraphAdmissionIssues(document())).toEqual([]);
    expect(parallelOwnedAgent(document(), 'extension', 0)?.id).toBe('Agent_left');
  });
  it.each([
    { branches: null }, { branches: [refs[0]] }, { branches: Array.from({ length: 17 }, (_, index) => ({ id: `b${String(index)}`, node: 'Agent_left' })) },
    { branches: [refs[0], null] }, { branches: [refs[0], { ...refs[1], worker: 'Agent_right' }] },
    { branches: [refs[0], { node: 'Agent_right' }] }, { branches: [refs[0], { id: 'left', node: 'Agent_right' }] },
    { branches: [refs[0], { id: 'bad space', node: 'Agent_right' }] },
    { branches: [refs[0], { id: 'x'.repeat(129), node: 'Agent_right' }] },
    { branches: [refs[0], { id: 'right', node: 'Agent_left' }] },
    { branches: [refs[0], { id: 'right', node: 'extension' }] },
    { branches: [refs[0], { id: 'right', node: 'right_participant' }] },
    { max_concurrency: 0 }, { max_concurrency: 9 }, { max_concurrency: 3 }, { max_concurrency: 1.5 }, { max_concurrency: '2' },
    { wait: 'any' }, { wait: null }, { error_policy: 'fail_fast' }, { error_policy: null },
    { output: [] }, { output: ['10', '2'] }, { output: ['note'] }, { output: ['undeclared'] }, { output: ['messages'] },
    { input: [] }, { input_mapping: {} }, { reduction: 'ordered_collection' }, { transition: 7 },
  ])('refuses malformed or foreign Parallel configuration %j', (patch) => {
    expect(graphParallelIssues(changed(patch)).length).toBeGreaterThan(0);
  });
  it('refuses the private Parallel output channels even when declared as lists', () => {
    for (const key of ['__elitea_parallel_resume_v1', '__elitea_parallel_agent_inputs_v1']) {
      const value = changed({ output: [key] });
      expect(graphParallelIssues({ ...value, state: { ...value.state, [key]: 'list' } }).length).toBeGreaterThan(0);
    }
  });
  it.each(['END', 'extension', '', 8])('refuses every present owned Agent transition %j', (transition) => {
    const value = document();
    const nodes = value.nodes?.map((node) => node.id === 'Agent_left' ? { ...node, transition } : node);
    expect(graphParallelIssues({ ...value, nodes } as YamlPipelineDocument).some((issue) => issue.message.includes('transition'))).toBe(true);
  });
  it('accepts null as an absent owned transition, matching the optional runtime field', () => {
    const value = document(parallelYaml.replace('    tool: left_participant', '    transition: null\n    tool: left_participant'));
    expect(graphParallelIssues(value)).toEqual([]);
  });
  it.each([
    { entry_point: 'Agent_left' }, { interrupt_before: ['Agent_left'] }, { interrupt_after: ['Agent_left'] },
  ])('refuses parent entry or static ownership conflicts %j', (patch) => {
    expect(graphParallelIssues({ ...document(), ...patch }).length).toBeGreaterThan(0);
  });
  it.each([
    { id: 'parent', type: 'printer', transition: 'Agent_left' },
    { id: 'parent', type: 'router', routes: ['Agent_left'], default_output: 'END' },
    { id: 'parent', type: 'hitl', routes: { approve: 'Agent_left' } },
    { id: 'parent', type: 'decision', nodes: ['Agent_left'], default_output: 'END' },
  ])('refuses every parent route into an owned Agent %j', (parent) => {
    const value = document();
    expect(graphParallelIssues({ ...value, nodes: [...(value.nodes ?? []), parent] }).some((issue) => issue.message.includes('parent route'))).toBe(true);
  });
  it.each([
    { id: 'other', type: 'parallel', branches: [{ id: 'another', node: 'Agent_left' }] },
    { id: 'other', type: 'map', worker: 'Agent_left' },
  ])('refuses shared ownership and withholds child mutation controls %j', (other) => {
    const value = document();
    const conflicting = { ...value, nodes: [...(value.nodes ?? []), other] };
    expect(graphParallelIssues(conflicting).some((issue) => issue.message.includes('exactly one'))).toBe(true);
    expect(parallelOwnedAgent(conflicting, 'extension', 0)).toBeUndefined();
  });
  it('rejects a declared sibling with the correct identity but a non-Agent type', () => {
    const value = document();
    expect(graphParallelIssues({ ...value, nodes: value.nodes?.map((node) => node.id === 'Agent_left' ? { ...node, type: 'llm' } : node) ?? [] }).length).toBeGreaterThan(0);
  });
  it('withholds child mutation when another node reuses the selected exact Agent identity', () => {
    const value = document();
    const invalid = { ...value, nodes: [...(value.nodes ?? []), { id: 'Agent_left', type: 'printer' }] };
    expect(graphParallelIssues(invalid).length).toBeGreaterThan(0);
    expect(parallelOwnedAgent(invalid, 'extension', 0)).toBeUndefined();
  });
});
