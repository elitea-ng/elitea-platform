import { conversationDetails, type StopChatTaskParams } from '@/entities/conversation/api/conversationApi';
import type { MessageGroupWire } from '@/entities/message/lib/wire';
import { sha256Hex } from '@/shared/lib/hash/sha256';
import { t } from '@/shared/i18n';
import { resultReference } from '../lib/chatStreamFinalResult';
import type { ChatStreamFrame } from '../lib/chatStreamFrame';

interface ReferencedResultCompletion {
  readonly frame: ChatStreamFrame;
  readonly target: StopChatTaskParams | null;
  readonly conversationUuid: string | undefined;
  readonly generation: string | undefined;
  readonly isCurrent: () => boolean;
  readonly onResult: (frame: ChatStreamFrame) => void;
  readonly onError: (reason: string) => void;
  readonly onSettled: () => void;
}

/** Read a projected referenced result once. Never admit or poll an execution. */
export async function completeReferencedChatResult(params: ReferencedResultCompletion): Promise<void> {
  const { frame, target, conversationUuid, generation, isCurrent, onResult, onError, onSettled } = params;
  try {
    const reference = resultReference(frame);
    if (!target || !conversationUuid || !generation || !reference || !isCurrent()) throw new Error('Missing result authority');
    const snapshot = await conversationDetails({
      projectId: target.projectId, id: conversationUuid, messages_limit: 50, sort_order: 'desc',
    }, AbortSignal.timeout(10_000));
    if (!isCurrent()) return;
    const groups = snapshot['message_groups'];
    if (!Array.isArray(groups)) throw new Error('Missing saved history');
    const matches = (groups as MessageGroupWire[]).filter((group) => group.uuid === target.messageGroupUuid);
    const saved = matches[0];
    if (matches.length !== 1 || !saved || saved.is_streaming !== false
      || saved.meta?.execution_generation !== generation || typeof saved.content !== 'string') throw new Error('Missing saved result authority');
    const bytes = new TextEncoder().encode(saved.content);
    const continued = frame.response_metadata?.should_continue === true;
    if (bytes.length < reference.total_bytes || (!continued && bytes.length !== reference.total_bytes)) throw new Error('Incomplete saved result');
    const result = continued ? new TextDecoder('utf-8', { fatal: true }).decode(bytes.slice(bytes.length - reference.total_bytes)) : saved.content;
    if (sha256Hex(result) !== reference.sha256) throw new Error('Mismatched saved result');
    onResult({ ...frame, content: saved.content });
  } catch {
    if (isCurrent()) onError(t('chatMessages.stream.savedResultReload', 'The run finished, but its saved answer could not be loaded. Reload this chat to view the saved result.'));
  } finally {
    if (isCurrent()) onSettled();
  }
}
