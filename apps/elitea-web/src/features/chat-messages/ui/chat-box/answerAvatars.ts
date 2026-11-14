/**
 * Which answers of a list show the agent caption and may move (ADR-0050
 * decision 6). Kept out of `ChatMessageList` so the list stays a renderer.
 */
import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';

/** The id of each participant's newest answer in the list. */
export function newestAnswerByParticipant(chatHistory: readonly ChatMessage[]): ReadonlySet<string> {
  const newest = new Map<number, string>();
  for (const message of chatHistory) {
    if (message.role !== 'user' && message.authorParticipantId !== undefined) newest.set(message.authorParticipantId, message.id);
  }
  return new Set(newest.values());
}
