import { describe, expect, it } from 'vitest';

import { PipelineNodeTypes } from './flow-editor/constants/flowEditor.constants';
import { MAX_NODE_ID_BYTES } from './flow-editor/constants/runtimeContract.constants';
import type { YamlPipelineNode } from './flow-editor/helpers/pipelineFlow.types';
import type { FlowNode } from './flow-editor/reactFlowTypes';
import { collectGraphAdmissionIssues } from './graphAdmission.helpers';
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

  it('clamps generated copy IDs to MAX_NODE_ID_BYTES (128) while staying collision-free', () => {
    const maxLenName = 'a'.repeat(MAX_NODE_ID_BYTES);
    const first = getDuplicateNodeId({ name: maxLenName, existingNodeIds: [maxLenName] });
    expect(first.length).toBeLessThanOrEqual(MAX_NODE_ID_BYTES);
    expect(first.endsWith('_copy')).toBe(true);

    const second = getDuplicateNodeId({ name: maxLenName, existingNodeIds: [maxLenName, first] });
    expect(second.length).toBeLessThanOrEqual(MAX_NODE_ID_BYTES);
    expect(second.endsWith('_copy_2')).toBe(true);
    expect(second).not.toBe(first);
  });

  it('clears transition, settings.error_transition, and resets type-specific routes to compiler-admitted END defaults', () => {
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

    const printerCopy = clearNodeConnections({
      id: 'Printer_1',
      type: PipelineNodeTypes.Printer,
      transition: 'Next_1',
      content: 'hi',
    });
    expect(printerCopy.transition).toBe('END');

    const routerCopy = clearNodeConnections({
      id: 'Router_1',
      type: PipelineNodeTypes.Router,
      condition: '{{ input }}',
      routes: ['A', 'B'],
      default_output: 'A',
    } as unknown as YamlPipelineNode);
    expect(routerCopy['routes']).toEqual([]);
    expect(routerCopy['default_output']).toBe('END');

    const decisionCopy = clearNodeConnections({
      id: 'Decision_1',
      type: PipelineNodeTypes.Decision,
      decision: { description: 'pick one' },
      nodes: ['A', 'B'],
      default_output: 'A',
    });
    expect(decisionCopy['nodes']).toEqual([]);
    expect(decisionCopy['default_output']).toBe('END');

    const hitlCopy = clearNodeConnections({
      id: 'HITL_1',
      type: PipelineNodeTypes.Hitl,
      user_message: { type: 'fixed', value: 'Approve?' },
      routes: { approve: 'A', edit: 'B', reject: 'END' },
    });
    expect(hitlCopy['routes']).toEqual({ approve: 'END', reject: 'END' });

    // Admission check: duplicated Printer, Router, Decision, and HITL nodes produce zero admission issues.
    const issues = collectGraphAdmissionIssues({
      entry_point: 'Printer_1',
      nodes: [printerCopy, routerCopy, decisionCopy, hitlCopy],
    });
    expect(issues).toEqual([]);
  });

  it('offsets flow node position, assigns the new ID/label, and strips transient isPerforming', () => {
    const original: FlowNode = {
      id: 'Code_1',
      type: PipelineNodeTypes.Code,
      position: { x: 100, y: 200 },
      data: { label: 'Code_1', type: PipelineNodeTypes.Code, isPerforming: true },
    };
    const copy = duplicateFlowNode(original, 'Code_1_copy');
    expect(copy.id).toBe('Code_1_copy');
    expect(copy.position).toEqual({ x: 140, y: 240 });
    expect(copy.data.label).toBe('Code_1_copy');
    expect(copy.data['isPerforming']).toBeUndefined();
    expect(copy.selected).toBe(true);
  });
});
