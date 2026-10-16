import { isRootAnswerFrame } from './chatStreamFinalResult';
import { findTarget, replaceAt } from './chatStreamShared';
import type { ChatMessage } from './convertMessagesToChatHistory.types';
import type { ChatStreamFrame } from './chatStreamFrame';
import { parseSkippedCapabilities } from './skippedCapabilities';

/**
 * Keep what the turn could not serve on the message the moment the worker
 * says so (`agent_start`, a pause or the final message), so a paused reply
 * shows its notice live too, not only after a reload.
 */
export function applySkippedCapabilitiesFrame(history: readonly ChatMessage[], frame: ChatStreamFrame): readonly ChatMessage[] {
  const raw = frame.response_metadata?.['skipped_capabilities'];
  if (raw === undefined || parseSkippedCapabilities(raw).items.length === 0 || !isRootAnswerFrame(frame)) return history;
  const index = findTarget(history, frame);
  const current = history[index];
  if (!current || current.skippedCapabilities === raw) return history;
  return replaceAt(history, index, { skippedCapabilities: raw });
}
