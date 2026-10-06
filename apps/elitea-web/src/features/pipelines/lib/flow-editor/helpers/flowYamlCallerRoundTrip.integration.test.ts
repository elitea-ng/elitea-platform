import { describe, expect, it, vi } from 'vitest';
import { load } from 'js-yaml';
import { serializePipelineYaml } from '../../dumpYaml.helpers';
import { parsePipelineYamlDocument } from '../../pipelineYamlDocument.helpers';
import { createPipelineYamlStore } from '../../../model/pipelineYamlStore';
import type { YamlPipelineDocument, YamlPipelineNode } from './pipelineFlow.types';
import { batchUpdateYamlNode } from './flowEditor.helpers';
import {
  handleFromConditionNodeConnection,
  handleFromDecisionNodeConnection,
  handleFromHitlNodeConnection,
} from './connectionOperations.helpers';

function loaded(node: YamlPipelineNode) {
  const doc = {
    state: { '10': 'list', '2': { type: 'dict', value: null, opaque: { keep: true } } },
    nodes: [node, { id: 'T', type: 'printer' }],
  };
  const source = serializePipelineYaml(doc, { stateKeyOrder: ['10', '2'] });
  const store = createPipelineYamlStore();
  store.getState().initPipelineYaml(parsePipelineYamlDocument(source));
  return {
    store,
    ref: { current: store.getState().yamlJsonObject as YamlPipelineDocument },
    setYamlJsonObject: store.getState().editPipelineYamlDocument,
  };
}
function savedNode(store: ReturnType<typeof createPipelineYamlStore>): Record<string, unknown> {
  const parsed = load(store.getState().yamlCode) as { nodes: Record<string, unknown>[] };
  expect(parsePipelineYamlDocument(store.getState().yamlCode).stateKeyOrder).toEqual(['10', '2']);
  expect(store.getState().stateKeyOrder).toEqual(['10', '2']);
  const node = parsed.nodes[0]!;
  expect(Object.hasOwn(node, 'transition')).toBe(false);
  return node;
}

describe('production route edits through ordered strict serialization', () => {
  it('connects a Condition output and omits its old transition', () => {
    const { store, ref, setYamlJsonObject } = loaded({ id: 'C', type: 'condition', condition: {}, transition: 'END' });
    expect(() =>
      handleFromConditionNodeConnection({
        connection: { source: 'C', target: 'T', sourceHandle: 'conditional_outputs', targetHandle: null },
        yamlJsonObjectRef: ref,
        setYamlJsonObject,
        setFlowNodes: vi.fn(),
      }),
    ).not.toThrow();
    expect(savedNode(store)['condition']).toEqual({ conditional_outputs: ['T'] });
  });
  it('connects a legacy Decision output and omits its old transition', () => {
    const { store, ref, setYamlJsonObject } = loaded({ id: 'D', decision: {}, transition: 'END' });
    expect(() =>
      handleFromDecisionNodeConnection({
        connection: { source: 'D~~~DecisionNode', target: 'T', sourceHandle: 'nodes', targetHandle: null },
        yamlJsonObjectRef: ref,
        setYamlJsonObject,
        setFlowNodes: vi.fn(),
      }),
    ).not.toThrow();
    expect(savedNode(store)['decision']).toEqual({ nodes: ['T'] });
  });
  it('connects a HITL route and omits its old transition', () => {
    const { store, ref, setYamlJsonObject } = loaded({
      id: 'H',
      type: 'hitl',
      routes: { approve: '', reject: 'END' },
      transition: 'END',
    });
    expect(() =>
      handleFromHitlNodeConnection({
        connection: { source: 'H', target: 'T', sourceHandle: 'hitlNode_approve', targetHandle: null },
        yamlJsonObjectRef: ref,
        setYamlJsonObject,
      }),
    ).not.toThrow();
    expect(savedNode(store)['routes']).toEqual({ approve: 'T', reject: 'END' });
  });
  it('preserves strict rejection of unrelated undefined fields and nested unsupported values atomically', () => {
    const { store, ref, setYamlJsonObject } = loaded({ id: 'A', type: 'printer', transition: 'END' });
    const before = store.getState();
    expect(() => batchUpdateYamlNode('A', { opaque: undefined }, ref.current, setYamlJsonObject)).toThrow();
    expect(store.getState()).toBe(before);
    expect(() =>
      batchUpdateYamlNode('A', { transition: undefined, opaque: { bad: undefined } }, ref.current, setYamlJsonObject),
    ).toThrow();
    expect(store.getState()).toBe(before);
  });
});
