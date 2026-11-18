import { renderHook } from '@testing-library/react';
import { load } from 'js-yaml';
import { describe, expect, it, vi } from 'vitest';

import { serializePipelineYaml } from '../../dumpYaml.helpers';
import { collectGraphAdmissionIssues } from '../../graphAdmission.helpers';
import { parsePipelineYamlDocument } from '../../pipelineYamlDocument.helpers';
import { useConnectNodes } from '../hooks/useConnectNodes';
import type { FlowEdge, FlowNode, SetFlowEdges } from '../reactFlowTypes';
import { handleNormalNodeDeletion, processEdgeDeletion } from './deletionOperations.helpers';
import { batchUpdateYamlNode } from './flowEditor.helpers';
import { ERROR_OUTPUT_HANDLE, connectErrorRoute } from './nodeErrorRoute.helpers';
import { parseNodes } from './parsePipelineTraversal.helpers';
import type { YamlPipelineDocument, YamlPipelineNode } from './pipelineFlow.types';

const DOCUMENT: YamlPipelineDocument = {
  state: { out: 'str' },
  entry_point: 'shape',
  nodes: [
    { id: 'shape', type: 'state_modifier', template: 'x', output: ['out'], transition: 'after', settings: { on_error: 'error_output', error_transition: 'handler' } },
    { id: 'after', type: 'state_modifier', template: 'y', output: ['out'], transition: 'END' },
    { id: 'handler', type: 'state_modifier', template: 'z', output: ['out'], transition: 'END' },
  ],
};

const nodeOf = (document: YamlPipelineDocument, id: string): Record<string, unknown> =>
  (document.nodes ?? []).find((node) => node.id === id) as unknown as Record<string, unknown>;

describe('the error output route (C6)', () => {
  it('is drawn from the error handle, beside the normal transition', () => {
    const { edges } = parseNodes(DOCUMENT);
    const error = edges.filter((edge) => edge.sourceHandle === ERROR_OUTPUT_HANDLE);
    expect(error).toEqual([expect.objectContaining({ source: 'shape', target: 'handler' })]);
    expect(edges.some((edge) => edge.source === 'shape' && edge.target === 'after' && edge.sourceHandle !== ERROR_OUTPUT_HANDLE)).toBe(true);
  });

  it('connecting the handle writes only settings.error_transition, never a transition', () => {
    const start = { ...DOCUMENT, nodes: [{ ...nodeOf(DOCUMENT, 'shape'), settings: { on_error: 'error_output', error_transition: 'handler' } } as unknown as YamlPipelineNode, ...(DOCUMENT.nodes ?? []).slice(1)] };
    const next = connectErrorRoute(start, 'shape', 'after');
    expect(nodeOf(next, 'shape')['transition']).toBe('after');
    expect(nodeOf(next, 'shape')['settings']).toEqual({ on_error: 'error_output', error_transition: 'after' });
  });

  it('routes a connection from the error handle through the hook without touching the transition', () => {
    let edges: FlowEdge[] = [{ id: 'old-error', source: 'shape', target: 'handler', sourceHandle: ERROR_OUTPUT_HANDLE }];
    const setFlowEdges = vi.fn<SetFlowEdges>((updater) => {
      edges = typeof updater === 'function' ? updater(edges) : updater;
    });
    const setYamlJsonObject = vi.fn();
    const { result } = renderHook(() => useConnectNodes({
      flowNodes: [], yamlJsonObjectRef: { current: DOCUMENT }, setFlowNodes: vi.fn(), setYamlJsonObject, setFlowEdges, disabled: false,
    }));
    result.current({ source: 'shape', target: 'END', sourceHandle: ERROR_OUTPUT_HANDLE, targetHandle: null });
    const written = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    expect(nodeOf(written, 'shape')['transition']).toBe('after');
    expect(nodeOf(written, 'shape')['settings']).toEqual({ on_error: 'error_output', error_transition: 'END' });
    expect(edges.filter((edge) => edge.sourceHandle === ERROR_OUTPUT_HANDLE).map((edge) => edge.target)).toEqual(['END']);
  });

  it('deleting the error edge clears only the error transition, and the editor names the gap', () => {
    const flowNodes = ['shape', 'after', 'handler', 'END'].map((id) => ({ id, type: id === 'END' ? 'END' : 'state_modifier', position: { x: 0, y: 0 }, data: {} })) as FlowNode[];
    const edge = { id: 'e', source: 'shape', target: 'handler', sourceHandle: ERROR_OUTPUT_HANDLE } as FlowEdge;
    const { yamlJsonObject } = processEdgeDeletion(edge, flowNodes, DOCUMENT, flowNodes);
    expect(nodeOf(yamlJsonObject, 'shape')['transition']).toBe('after');
    expect(nodeOf(yamlJsonObject, 'shape')['settings']).toEqual({ on_error: 'error_output' });
    expect(collectGraphAdmissionIssues(yamlJsonObject).map((issue) => issue.subject)).toContain('error_transition_required');
  });

  it('deleting the target node clears the error transition', () => {
    const next = handleNormalNodeDeletion({ id: 'handler', type: 'state_modifier', position: { x: 0, y: 0 }, data: {} }, DOCUMENT);
    expect(nodeOf(next, 'shape')['settings']).toEqual({ on_error: 'error_output' });
    expect(nodeOf(next, 'shape')['transition']).toBe('after');
  });
});

describe('settings survive the YAML round trip', () => {
  it('serializes, parses and edits a node without dropping its settings', () => {
    const source = serializePipelineYaml(DOCUMENT, { stateKeyOrder: ['out'] });
    const parsed = parsePipelineYamlDocument(source).yamlJsonObject as YamlPipelineDocument;
    expect(nodeOf(parsed, 'shape')['settings']).toEqual({ on_error: 'error_output', error_transition: 'handler' });
    let edited: YamlPipelineDocument = parsed;
    batchUpdateYamlNode('shape', { template: 'changed' }, parsed, (next) => { edited = next; });
    const again = load(serializePipelineYaml(edited, { stateKeyOrder: ['out'] })) as YamlPipelineDocument;
    expect(nodeOf(again, 'shape')['settings']).toEqual({ on_error: 'error_output', error_transition: 'handler' });
    expect(nodeOf(again, 'shape')['template']).toBe('changed');
  });

  it('removes the block when the panel returns it to defaults', () => {
    let edited: YamlPipelineDocument = DOCUMENT;
    batchUpdateYamlNode('shape', { settings: undefined }, DOCUMENT, (next) => { edited = next; });
    expect(Object.hasOwn(nodeOf(edited, 'shape'), 'settings')).toBe(false);
  });
});
