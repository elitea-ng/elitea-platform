import { act, renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { installWebStorageShim } from '../../../../../test/webstorage';

import receipt from '@/shared/lib/fixtures/node-recovery-required.json';
import { PipelineStatus } from '../constants/flowEditor.constants';
import type { YamlPipelineDocument } from '../helpers/pipelineFlow.types';
import type { FlowNode, SetFlowNodes } from '../reactFlowTypes';
import { useRunEvent } from './useRunEvent';

installWebStorageShim();

function makeStatefulSetFlowNodes(initial: readonly FlowNode[]) {
  let nodes: readonly FlowNode[] = initial;
  const setFlowNodes = vi.fn<SetFlowNodes>(updater => {
    nodes = typeof updater === 'function' ? updater(nodes as FlowNode[]) : updater;
  });
  return { setFlowNodes, getNodes: () => nodes };
}

describe('useRunEvent', () => {
  it('clears performing only for the exact suspended response while retaining Stop state', () => {
    const response = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';
    const { setFlowNodes, getNodes } = makeStatefulSetFlowNodes([{ id: 'python', type: 'code', position: { x: 0, y: 0 }, data: {} }]);
    const { result } = renderHook(() => useRunEvent(setFlowNodes, { nodes: [{ id: 'python' }] }));
    act(() => result.current.onRcvAgentEvent({ type: 'agent_start', message_id: response, execution_generation: '1', response_metadata: {} }));
    act(() => result.current.onRcvAgentEvent({ type: 'agent_llm_start', response_metadata: { metadata: { langgraph_node: 'python' } } }));
    const pause = { type: 'agent_node_recovery_required', message_id: response, execution_generation: '1', response_metadata: { node_recovery_required_v1: receipt } };
    for (const bad of [
      { ...pause, execution_generation: '2' }, { ...pause, message_id: 'bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee' },
      { ...pause, response_metadata: { ...pause.response_metadata, parent_agent_path: 'malformed' } },
      { ...pause, response_metadata: { node_recovery_required_v1: { ...receipt, schema: 'unknown' } } },
    ]) {
      act(() => result.current.onRcvAgentEvent(bad));
      expect(getNodes()[0]?.data.isPerforming).toBe(true);
    }
    act(() => result.current.onRcvAgentEvent(pause));
    expect(getNodes()[0]?.data.isPerforming).toBeUndefined();
    expect(result.current.isRunningPipeline).toBe(true);
    expect(result.current.pipelineRunNodes[0]?.data).toMatchObject({ status: PipelineStatus.Interrupt, recoveryPaused: true });
    expect(result.current.pipelineRunNodes[0]?.data.timeline[0]?.status).toBe(PipelineStatus.Interrupt);
  });

  it('agent_start creates a running pipeline entry and marks the running flag', () => {
    const { setFlowNodes } = makeStatefulSetFlowNodes([{ id: 'Agent 1', type: 'agent', position: { x: 0, y: 0 }, data: {} }]);
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Agent 1' }] };
    const { result } = renderHook(() => useRunEvent(setFlowNodes, yamlJsonObject));

    act(() => {
      result.current.onRcvAgentEvent({ type: 'agent_start', response_metadata: {} });
    });

    expect(result.current.isRunningPipeline).toBe(true);
    expect(result.current.pipelineRunNodes).toHaveLength(1);
    expect(result.current.pipelineRunNodes[0]).toMatchObject({
      id: 'EliteA_Pipeline__State_Run 1 details',
      data: { label: 'Run 1 details', status: PipelineStatus.InProgress, timeline: [] },
    });
  });

  it('agent_llm_start after agent_start extends the timeline and marks the matching node as performing', () => {
    const { setFlowNodes, getNodes } = makeStatefulSetFlowNodes([{ id: 'Agent 1', type: 'agent', position: { x: 0, y: 0 }, data: {} }]);
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Agent 1' }] };
    const { result } = renderHook(() => useRunEvent(setFlowNodes, yamlJsonObject));

    act(() => {
      result.current.onRcvAgentEvent({ type: 'agent_start', response_metadata: {} });
    });
    act(() => {
      result.current.onRcvAgentEvent({
        type: 'agent_llm_start',
        response_metadata: { metadata: { langgraph_node: 'Agent 1' }, tool_run_id: 'r1' },
      });
    });

    expect(result.current.pipelineRunNodes[0]?.data.timeline).toEqual([
      expect.objectContaining({ id: 'Agent 1', status: PipelineStatus.InProgress, tool_run_id: 'r1' }),
    ]);
    expect(getNodes().find(n => n.id === 'Agent 1')?.data.isPerforming).toBe(true);
  });

  it('onStopRun marks the run Stopped, resets the running flag, and clears isPerforming on every node', () => {
    const { setFlowNodes, getNodes } = makeStatefulSetFlowNodes([{ id: 'Agent 1', type: 'agent', position: { x: 0, y: 0 }, data: {} }]);
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Agent 1' }] };
    const { result } = renderHook(() => useRunEvent(setFlowNodes, yamlJsonObject));

    act(() => {
      result.current.onRcvAgentEvent({ type: 'agent_start', response_metadata: {} });
    });
    act(() => {
      result.current.onRcvAgentEvent({
        type: 'agent_llm_start',
        response_metadata: { metadata: { langgraph_node: 'Agent 1' }, tool_run_id: 'r1' },
      });
    });
    const runId = result.current.pipelineRunNodes[0]!.id;

    act(() => {
      result.current.onStopRun(runId);
    });

    expect(result.current.isRunningPipeline).toBe(false);
    expect(result.current.pipelineRunNodes[0]?.data.status).toBe(PipelineStatus.Stopped);
    expect(getNodes().every(n => n.data.isPerforming === undefined)).toBe(true);
  });

  it('deleteRunNode removes only the matching run entry', () => {
    const { setFlowNodes } = makeStatefulSetFlowNodes([]);
    const yamlJsonObject: YamlPipelineDocument = { nodes: [] };
    const { result } = renderHook(() => useRunEvent(setFlowNodes, yamlJsonObject));

    act(() => {
      result.current.onRcvAgentEvent({ type: 'agent_start', response_metadata: {} });
    });
    const runId = result.current.pipelineRunNodes[0]!.id;

    act(() => {
      result.current.deleteRunNode(runId);
    });

    expect(result.current.pipelineRunNodes).toEqual([]);
  });

  it('deleteAllRunNodes clears every run, resets the running flag, and clears isPerforming', () => {
    const { setFlowNodes, getNodes } = makeStatefulSetFlowNodes([{ id: 'Agent 1', type: 'agent', position: { x: 0, y: 0 }, data: { isPerforming: true } }]);
    const yamlJsonObject: YamlPipelineDocument = { nodes: [] };
    const { result } = renderHook(() => useRunEvent(setFlowNodes, yamlJsonObject));

    act(() => {
      result.current.onRcvAgentEvent({ type: 'agent_start', response_metadata: {} });
    });

    act(() => {
      result.current.deleteAllRunNodes();
    });

    expect(result.current.pipelineRunNodes).toEqual([]);
    expect(result.current.isRunningPipeline).toBe(false);
    expect(getNodes().every(n => n.data.isPerforming === undefined)).toBe(true);
  });

  it('onResetRunParseStatus resets isRunningPipeline without touching the accumulated run list', () => {
    const { setFlowNodes } = makeStatefulSetFlowNodes([]);
    const yamlJsonObject: YamlPipelineDocument = { nodes: [] };
    const { result } = renderHook(() => useRunEvent(setFlowNodes, yamlJsonObject));

    act(() => {
      result.current.onRcvAgentEvent({ type: 'agent_start', response_metadata: {} });
    });
    act(() => {
      result.current.onResetRunParseStatus();
    });

    expect(result.current.isRunningPipeline).toBe(false);
    expect(result.current.pipelineRunNodes).toHaveLength(1);
  });

  it('picks the next free "Run N details" name, skipping labels already taken by a finished (but not deleted) run', () => {
    const { setFlowNodes } = makeStatefulSetFlowNodes([]);
    const yamlJsonObject: YamlPipelineDocument = { nodes: [] };
    const { result } = renderHook(() => useRunEvent(setFlowNodes, yamlJsonObject));

    act(() => {
      result.current.onRcvAgentEvent({ type: 'agent_start', response_metadata: {} });
    });
    act(() => {
      result.current.onRcvAgentEvent({ type: 'pipeline_finish', response_metadata: {} });
    });
    expect(result.current.pipelineRunNodes).toHaveLength(1);
    expect(result.current.pipelineRunNodes[0]?.data.label).toBe('Run 1 details');

    act(() => {
      result.current.onRcvAgentEvent({ type: 'agent_start', response_metadata: {} });
    });

    expect(result.current.pipelineRunNodes).toHaveLength(2);
    expect(result.current.pipelineRunNodes[1]).toMatchObject({
      id: 'EliteA_Pipeline__State_Run 2 details',
      data: { label: 'Run 2 details' },
    });
  });

  it('a run-progress event fired before any agent_start resolves no run-status update, so flow nodes and run state stay untouched', () => {
    const { setFlowNodes } = makeStatefulSetFlowNodes([{ id: 'Agent 1', type: 'agent', position: { x: 0, y: 0 }, data: {} }]);
    const yamlJsonObject: YamlPipelineDocument = { nodes: [] };
    const { result } = renderHook(() => useRunEvent(setFlowNodes, yamlJsonObject));

    act(() => {
      result.current.onRcvAgentEvent({ type: 'agent_llm_start', response_metadata: { metadata: { langgraph_node: 'Agent 1' } } });
    });

    expect(result.current.isRunningPipeline).toBe(false);
    expect(result.current.pipelineRunNodes).toEqual([]);
    expect(setFlowNodes).not.toHaveBeenCalled();
  });
});
