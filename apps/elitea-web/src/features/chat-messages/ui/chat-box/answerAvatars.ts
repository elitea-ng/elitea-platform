/**
 * Which answers of a list show the agent caption and may move (ADR-0050
 * decision 6). Kept out of `ChatMessageList` so the list stays a renderer.
 */
import { MESSAGE_CLASS } from '@elitea/client-core/conversation';

import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';

/** The id of each participant's newest answer in the list. */
export function newestAnswerByParticipant(chatHistory: readonly ChatMessage[]): ReadonlySet<string> {
  const newest = new Map<number, string>();
  for (const message of chatHistory) {
    if (message.role !== 'user' && message.authorParticipantId !== undefined) newest.set(message.authorParticipantId, message.id);
  }
  return new Set(newest.values());
}

/**
 * An answer whose agent has an identity of its own (an icon, a persona role,
 * or a temporary / woke-up marker) shows the caption in any conversation; a
 * plain model answer shows it only when more than one assistant answers.
 */
export function answerHasIdentity(message: ChatMessage): boolean {
  return (
    message.authorIconUrl !== undefined ||
    message.authorRole !== undefined ||
    message.messageClass === MESSAGE_CLASS.Ephemeral ||
    message.messageClass === MESSAGE_CLASS.Heartbeat
  );
}
