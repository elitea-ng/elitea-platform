import { describe, expect, it } from 'vitest';

import { PipelineNodeTypes } from './flow-editor/constants/flowEditor.constants';
import type { YamlPipelineNode } from './flow-editor/helpers/pipelineFlow.types';
import type { FlowNode } from './flow-editor/reactFlowTypes';
import {
  canDuplicateNodeType,
  clearNodeConnections,
  duplicateFlowNode,
  duplicateYamlNode,
  getDuplicateNodeId,
} from './duplicateNode';

describe('duplicateNode helpers', () => {
  it('allows duplicating admitted node types and rejects END, ghost, and deprecated types', () => {
    expect(canDuplicateNodeType(PipelineNodeTypes.LLM)).toBe(true);
    expect(canDuplicateNodeType(PipelineNodeTypes.Map)).toBe(true);
    expect(canDuplicateNodeType(PipelineNodeTypes.End)).toBe(false);
    expect(canDuplicateNodeType(PipelineNodeTypes.Ghost)).toBe(false);
    expect(canDuplicateNodeType(PipelineNodeTypes.Loop)).toBe(false);
    expect(canDuplicateNodeType(undefined)).toBe(false);
  });

  it('generates collision-free copy IDs across existing nodes and tool names', () => {
    expect(getDuplicateNodeId({ name: 'LLM_1', existingNodeIds: ['LLM_1'] })).toBe('LLM_1_copy');
    expect(
      getDuplicateNodeId({
        name: 'LLM_1',
        existingNodeIds: ['LLM_1', 'LLM_1_copy'],
        toolNames: ['LLM_1_copy_2'],
      }),
    ).toBe('LLM_1_copy_3');
  });

  it('clears transition, settings.error_transition, and type-specific outgoing routes', () => {
    const llmNode: YamlPipelineNode = {
      id: 'LLM_1',
      type: PipelineNodeTypes.LLM,
      transition: 'Next_1',
      settings: { on_error: 'error_output', error_transition: 'ErrorHandler_1' },
      input_mapping: { task: { type: 'fixed', value: 'hello' } },
    };
    const copy = duplicateYamlNode(llmNode, 'LLM_1_copy');
    expect(copy.id).toBe('LLM_1_copy');
    expect(copy.transition).toBeUndefined();
    expect(copy['settings']).toEqual({ on_error: 'error_output' });
    // Deep clone check: mutating original does not mutate copy
    (llmNode.input_mapping as Record<string, { value: string }>)['task']!.value = 'changed';
    expect((copy.input_mapping as Record<string, { value: string }>)['task']?.value).toBe('hello');

    const routerCopy = clearNodeConnections({
      id: 'Router_1',
      type: PipelineNodeTypes.Router,
      routes: ['A', 'B'],
      default_output: 'END',
    });
    expect(routerCopy['routes']).toEqual([]);
    expect(routerCopy['default_output']).toBe('');

    const hitlCopy = clearNodeConnections({
      id: 'HITL_1',
      type: PipelineNodeTypes.Hitl,
      routes: { approve: 'A', edit: 'B', reject: 'END' },
    });
    expect(hitlCopy['routes']).toEqual({ approve: '', edit: '', reject: '' });
  });

  it('offsets flow node position and assigns the new ID and label', () => {
    const original: FlowNode = {
      id: 'Code_1',
      type: PipelineNodeTypes.Code,
      position: { x: 100, y: 200 },
      data: { label: 'Code_1', type: PipelineNodeTypes.Code },
    };
    const copy = duplicateFlowNode(original, 'Code_1_copy');
    expect(copy.id).toBe('Code_1_copy');
    expect(copy.position).toEqual({ x: 140, y: 240 });
    expect(copy.data.label).toBe('Code_1_copy');
    expect(copy.selected).toBe(true);
  });
});
