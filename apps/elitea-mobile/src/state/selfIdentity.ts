import { useSyncExternalStore } from 'react';

import { registerWipeHook } from '@/security/endSession';

/**
 * Who "me" is in a shared conversation: the signed-in user's USER id.
 *
 * The client contract has no "who am I" read, so the id is learned from what
 * the server already tells this caller:
 *  - every notification row's `user_id` is its recipient, the caller;
 *  - a user message group whose uuid is a `question_id` this device sent was
 *    written by the caller (the send route keys the user turn on it).
 *
 * Kept in memory only and wiped with the session (another account on the
 * same device must never inherit it).
 */

let selfUserId: number | null = null;
/** question_ids sent from this device in this session (bounded). */
const sentQuestions = new Set<string>();
const MAX_SENT_QUESTIONS = 500;
const listeners = new Set<() => void>();

function emit() {
  for (const listener of [...listeners]) listener();
}

export function getSelfUserId(): number | null {
  return selfUserId;
}

/** Records the caller's user id (first source wins; a later different id replaces it). */
export function learnSelfUserId(id: unknown): void {
  if (typeof id !== 'number' || !Number.isSafeInteger(id) || id <= 0 || id === selfUserId) return;
  selfUserId = id;
  emit();
}

export function noteSentQuestion(questionId: string): void {
  if (sentQuestions.has(questionId)) return;
  if (sentQuestions.size >= MAX_SENT_QUESTIONS) {
    const oldest = sentQuestions.values().next().value;
    if (oldest !== undefined) sentQuestions.delete(oldest);
  }
  sentQuestions.add(questionId.toLowerCase());
}

export function isSentQuestion(uuid: string | null | undefined): boolean {
  return typeof uuid === 'string' && sentQuestions.has(uuid.toLowerCase());
}

export function subscribeSelfIdentity(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function useSelfUserId(): number | null {
  return useSyncExternalStore(subscribeSelfIdentity, getSelfUserId, getSelfUserId);
}

export function resetSelfIdentity(): void {
  selfUserId = null;
  sentQuestions.clear();
  emit();
}

registerWipeHook('self_identity', resetSelfIdentity);
