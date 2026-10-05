import { nodeRecoveryBinding, type NodeRecoveryBinding } from '@/shared/lib/nodeRecovery';
import type { ChatMessage } from './convertMessagesToChatHistory.types';

/** Only the active response can block a new turn or display a suspended run. */
export function messageNodeRecoveryBinding(message: ChatMessage | undefined): NodeRecoveryBinding | undefined {
  const binding = message?.nodeRecoveryRequired;
  if (!message || message.role !== 'assistant' || !message.isStreaming || message.exception || message.failureCode
    || !binding || message.id !== binding.responseMessageId || message.executionGeneration !== binding.executionGeneration) return;
  return nodeRecoveryBinding(binding.responseMessageId, binding.executionGeneration, binding.receipt);
}
export function currentNodeRecoveryBinding(messages: readonly ChatMessage[]): NodeRecoveryBinding | undefined {
  return messageNodeRecoveryBinding(messages.at(-1));
}
