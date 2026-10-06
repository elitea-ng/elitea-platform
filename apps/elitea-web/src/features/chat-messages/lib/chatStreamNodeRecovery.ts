import { isRootNodeRecoveryOwner, nodeRecoveryFromEvent, type NodeRecoveryBinding } from '@/shared/lib/nodeRecovery';
import { createAssistantMessage, findTarget, replaceAt, type ChatStreamContext } from './chatStreamShared';
import type { ChatMessage } from './convertMessagesToChatHistory.types';
import type { ChatStreamFrame } from './chatStreamFrame';

function staleReceipt(previous: NodeRecoveryBinding | undefined, next: NodeRecoveryBinding): boolean {
  if (!previous) return false;
  if (next.receipt.step < previous.receipt.step) return true;
  if (next.receipt.activation_id !== previous.receipt.activation_id) return false;
  return next.receipt.journal_revision < previous.receipt.journal_revision
    || (next.receipt.journal_revision === previous.receipt.journal_revision && JSON.stringify(next.receipt) !== JSON.stringify(previous.receipt));
}
/** Preserve the suspended response and Stop. A recovery pause is not a terminal answer. */
export function applyNodeRecoveryFrame(history: readonly ChatMessage[], frame: ChatStreamFrame, context: ChatStreamContext): readonly ChatMessage[] {
  const index = findTarget(history, frame), current = history[index];
  if (frame.type === 'agent_node_recovery_required') {
    const binding = nodeRecoveryFromEvent(frame);
    if (!binding || (!current && history.some(message => message.role === 'assistant' && message.isStreaming)) || (current && (current.id !== binding.responseMessageId || !current.isStreaming || current.exception || current.failureCode))
      || (current?.executionGeneration && current.executionGeneration !== binding.executionGeneration)
      || staleReceipt(current?.nodeRecoveryRequired, binding)) return history;
    const update = { nodeRecoveryRequired: binding, executionGeneration: binding.executionGeneration,
      isStreaming: true, isLoading: false, isRegenerating: false };
    return current ? replaceAt(history, index, update) : [...history, { ...createAssistantMessage(frame, context), ...update }];
  }
  if (current?.nodeRecoveryRequired && isRootNodeRecoveryOwner(frame.response_metadata)
    && (frame.type !== 'full_message' || !current.isStreaming)
    && ['agent_start', 'start_task', 'agent_llm_start', 'agent_response', 'full_message', 'pipeline_finish', 'error', 'exception', 'agent_exception', 'task_failed'].includes(frame.type ?? '')) {
    return replaceAt(history, index, { nodeRecoveryRequired: undefined });
  }
  return history;
}
