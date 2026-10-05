import { sha256Hex } from '@/shared/lib/hash/sha256';
import { convertJsonToString } from '@/shared/lib/json';
import { normalizeExecutionHierarchy } from './executionHierarchy';
import { splitWholeResponse } from './chatStreamReasoning';
import { createAssistantMessage, findTarget, replaceAt, threadIdOf, type ChatStreamContext, type ToolAction } from './chatStreamShared';
import type { ChatMessage } from './convertMessagesToChatHistory';
import type { ChatStreamFrame } from './chatStreamFrame';

/** Child completion never closes the root observer or replaces its answer. */
export function isRootAnswerFrame(frame: ChatStreamFrame): boolean {
  const owner = normalizeExecutionHierarchy(frame.response_metadata, frame.response_metadata?.metadata, frame.response_metadata?.tool_meta?.metadata);
  return !owner.parent_agent_name && !owner.parent_agent_call_id && owner.parent_agent_path.length === 0;
}

export function resultReference(frame: ChatStreamFrame): { total_bytes: number; sha256: string } | undefined {
  const reference = frame.response_metadata?.['result_ref_v1'];
  if (!reference || typeof reference !== 'object') return;
  const value = reference as Record<string, unknown>;
  const total = value['total_bytes'], digest = value['sha256'];
  if (typeof total !== 'number' || !Number.isSafeInteger(total) || total <= 0 || total > 4194304
    || typeof digest !== 'string' || !/^[0-9a-f]{64}$/.test(digest)) return;
  return { total_bytes: total, sha256: digest };
}

/** An observer-read failure does not fail the completed durable execution. */
export function settleFinalResultObserver(history: readonly ChatMessage[], frame: ChatStreamFrame): readonly ChatMessage[] {
  const index = findTarget(history, frame);
  return index === -1 ? history : replaceAt(history, index, { isStreaming: false, isLoading: false, isRegenerating: false });
}

/** Main releases this result frame only after its terminal projection commits. */
export function isFullMessageResultFrame(frame: ChatStreamFrame): boolean {
  return frame.type === 'full_message' && isRootAnswerFrame(frame)
    && ((frame.content !== undefined && frame.content !== null) || resultReference(frame) !== undefined);
}

/** Fold the persisted final snapshot into the same live answer. */
export function reduceFinalResultFrame(
  history: readonly ChatMessage[], frame: ChatStreamFrame, context: ChatStreamContext, index: number,
): readonly ChatMessage[] | undefined {
  if (frame.type !== 'full_message') return undefined;
  if (!isFullMessageResultFrame(frame)) return history;
  const current = history[index] ?? createAssistantMessage(frame, context);
  let text: string;
  if (frame.content !== undefined && frame.content !== null) {
    text = convertJsonToString(frame.content, true);
  } else {
    const reference = resultReference(frame);
    const assembled = current.assembledResult;
    if (!reference || typeof assembled !== 'string') return history;
    const bytes = new TextEncoder().encode(assembled);
    if (bytes.length !== reference.total_bytes || sha256Hex(assembled) !== reference.sha256) return history;
    text = (current.continuedResultPrefix ?? '') + assembled;
  }
  if (frame.response_metadata?.should_continue === true && frame.content !== null) {
    const prefix = current.continuedResultPrefix;
    text = prefix !== undefined ? (text.startsWith(prefix) ? text : prefix + text)
      : current.content.endsWith(text) ? current.content : current.content + text;
  }
  const actions = (current.toolActions ?? []) as readonly ToolAction[];
  const split = splitWholeResponse(current.id, text, actions, frame.created_at);
  const threadId = threadIdOf(frame);
  const update: Partial<ChatMessage> = {
    ...(frame.message_id ? { id: frame.message_id } : {}),
    ...(frame.question_id ? { questionId: frame.question_id } : {}),
    ...(frame.execution_generation ? { executionGeneration: frame.execution_generation } : {}),
    ...(threadId ? { threadId } : {}),
    content: split.answer, toolActions: split.actions,
    responseMetadata: frame.response_metadata,
    ...(frame.references?.length ? { references: frame.references } : {}),
    requiresConfirmation: frame.response_metadata?.['output_limit_reached'] === true ? {
      message: "Token limit reached mid-response. Press 'Continue' to see more.", buttonText: 'Continue',
    } : undefined,
    isStreaming: false, isLoading: false, isRegenerating: false,
    hitlInterrupt: undefined, hitlInterrupts: undefined, exception: undefined,
  };
  return index === -1 ? [...history, { ...current, ...update }] : replaceAt(history, index, update);
}
