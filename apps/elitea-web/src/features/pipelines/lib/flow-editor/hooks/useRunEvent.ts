/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/lib/
 * hooks/useRunEvent.hooks.js` (159 lines, unit A2d) — accumulates streamed
 * socket run events into the "Run N details" timeline node shown on the
 * canvas while a pipeline executes.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { isRootNodeRecoveryOwner, nodeRecoveryFromEvent } from '@/shared/lib/nodeRecovery';
import { PipelineStatus } from '../constants/flowEditor.constants';
import { parseRunEvent } from '../helpers/parseRunsByEvent.helpers';
import type { RunPipelineStatus, RunSocketEvent } from '../helpers/parseRunsByEvent.support';
import type { YamlPipelineDocument } from '../helpers/pipelineFlow.types';
import type { FlowNode, SetFlowNodes } from '../reactFlowTypes';

export interface UseRunEventResult {
  readonly deleteRunNode: (id: string) => void;
  readonly deleteAllRunNodes: () => void;
  readonly onStopRun: (id: string) => void;
  readonly onRcvAgentEvent: (event: RunSocketEvent) => void;
  readonly onResetRunParseStatus: () => void;
  readonly isRunningPipeline: boolean;
  readonly pipelineRunNodes: readonly RunPipelineStatus[];
}

export function useRunEvent(setFlowNodes: SetFlowNodes, yamlJsonObject: YamlPipelineDocument): UseRunEventResult {
  const [pipelineRunNodes, setPipelineRunNodes] = useState<readonly RunPipelineStatus[]>([]);
  const [isRunningPipeline, setIsRunningPipeline] = useState(false);
  const runPipelineStatusNodeIdRef = useRef<string | undefined>('');
  const activeNodeIdRef = useRef<string | undefined>('');
  const runIdentity = useRef<{ responseId: string; generation: string } | undefined>(undefined);
  const runPipelineStatus = useRef<RunPipelineStatus | null>(null);

  const nextRunName = useMemo(() => {
    for (let index = 0; index < pipelineRunNodes.length + 1; index++) {
      const newName = `Run ${index + 1} details`;
      if (!pipelineRunNodes.find(node => node.data.label === newName)) {
        return newName;
      }
    }
    return 'Run 1 details';
  }, [pipelineRunNodes]);

  const deleteRunNode = useCallback((id: string) => {
    setPipelineRunNodes(prevNodes => prevNodes.filter(node => node.id !== id));
  }, []);

  const clearRunParseStatus = useCallback(() => {
    runIdentity.current = undefined;
    activeNodeIdRef.current = '';
    runPipelineStatusNodeIdRef.current = '';
    runPipelineStatus.current = null;
  }, []);

  const onResetRunParseStatus = useCallback(() => {
    setIsRunningPipeline(false);
    clearRunParseStatus();
  }, [clearRunParseStatus]);

  const onStopRun = useCallback(
    (id: string) => {
      onResetRunParseStatus();
      setPipelineRunNodes(prev =>
        prev.map(node =>
          id !== node.id ? node : { ...node, data: { ...node.data, status: PipelineStatus.Stopped } },
        ),
      );
      setFlowNodes(prev => prev.map(node => ({ ...node, data: { ...node.data, isPerforming: undefined } }) as unknown as FlowNode));
    },
    [onResetRunParseStatus, setFlowNodes],
  );

  const deleteAllRunNodes = useCallback(() => {
    setPipelineRunNodes([]);
    onResetRunParseStatus();
    setFlowNodes(prev => prev.map(node => ({ ...node, data: { ...node.data, isPerforming: undefined } }) as unknown as FlowNode));
  }, [onResetRunParseStatus, setFlowNodes]);

  const applyRecoveryPause = useCallback((event: RunSocketEvent) => {
    const binding = nodeRecoveryFromEvent(event), identity = runIdentity.current;
    if (!binding || !identity || identity.responseId !== binding.responseMessageId
      || identity.generation !== binding.executionGeneration || !runPipelineStatus.current) return;
    const suspended = runPipelineStatus.current;
    activeNodeIdRef.current = '';
    suspended.data.status = PipelineStatus.Interrupt;
    suspended.data.recoveryPaused = true;
    suspended.data.timeline = suspended.data.timeline.map(step =>
      step.id === binding.receipt.node_id && step.status === PipelineStatus.InProgress
        ? { ...step, status: PipelineStatus.Interrupt } : step);
    setFlowNodes(prev => prev.map(node => ({ ...node, data: { ...node.data, isPerforming: undefined } }) as unknown as FlowNode));
    setPipelineRunNodes(prev => prev.map(node => node.id !== suspended.id ? node
      : { ...node, data: { ...suspended.data, timeline: [...suspended.data.timeline] } }));
  }, [setFlowNodes]);

  const onRcvAgentEvent = useCallback(
    (event: RunSocketEvent) => {
      if ((event.type === 'agent_start' || event.type === 'start_task') && isRootNodeRecoveryOwner(event.response_metadata)) {
        runIdentity.current = event.message_id && event.execution_generation
          ? { responseId: event.message_id, generation: event.execution_generation } : undefined;
      }
      if (event.type === 'agent_node_recovery_required') {
        applyRecoveryPause(event);
        return;
      }
      parseRunEvent(
        event,
        yamlJsonObject.nodes ?? [],
        yamlJsonObject.interrupt_before ?? [],
        yamlJsonObject.interrupt_after ?? [],
        isRunningPipeline,
        setIsRunningPipeline,
        runPipelineStatusNodeIdRef,
        activeNodeIdRef,
        runPipelineStatus as { current: RunPipelineStatus },
        nextRunName,
      );
      if (runPipelineStatus.current) {
        setFlowNodes(prev => {
          const foundActiveNode = prev.find(
            node => node.id === activeNodeIdRef.current || node.id.replaceAll(' ', '') === activeNodeIdRef.current,
          );
          return prev.map(
            node =>
              ({
                ...node,
                data: { ...node.data, isPerforming: node.id === foundActiveNode?.id ? true : undefined },
              }) as unknown as FlowNode,
          );
        });
        setPipelineRunNodes(prev => {
          const status = runPipelineStatus.current;
          if (!status) return prev;
          if (prev.find(node => node.id === status.id)) {
            return prev.map(node =>
              node.id !== status.id ? node : { ...node, ...status, data: { ...status.data, timeline: [...status.data.timeline] } },
            );
          }
          return [...prev, { ...status, data: { ...status.data, timeline: [...status.data.timeline] } }];
        });
      }
    },
    [applyRecoveryPause, isRunningPipeline, nextRunName, setFlowNodes, yamlJsonObject.interrupt_after, yamlJsonObject.interrupt_before, yamlJsonObject.nodes],
  );

  useEffect(() => {
    if (!isRunningPipeline) {
      const timer = setTimeout(() => {
        runPipelineStatus.current = null;
      }, 300);
      return () => clearTimeout(timer);
    }
    return undefined;
  }, [isRunningPipeline]);

  return {
    deleteRunNode,
    deleteAllRunNodes,
    onStopRun,
    onRcvAgentEvent,
    onResetRunParseStatus,
    isRunningPipeline,
    pipelineRunNodes,
  };
}
