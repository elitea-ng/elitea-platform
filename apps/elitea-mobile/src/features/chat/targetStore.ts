import { useCallback, useSyncExternalStore } from 'react';

import { registerWipeHook } from '@/security/endSession';

/**
 * The agent a conversation's messages go to, as the user last picked it
 * ("To:" chip or "#"), per conversation. Memory only (a relaunch falls back
 * to the agent that answered last) and wiped with the session.
 */

const targets = new Map<string, number>();
const listeners = new Set<() => void>();

function emit() {
  for (const listener of [...listeners]) listener();
}

export function targetKey(projectId: number, conversationRef: string): string {
  return `${projectId}:${conversationRef}`;
}

export function getTarget(key: string): number | null {
  return targets.get(key) ?? null;
}

export function setTarget(key: string, participantId: number | null): void {
  if (participantId === null) targets.delete(key);
  else targets.set(key, participantId);
  emit();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function useTarget(key: string | null): number | null {
  const read = useCallback(() => (key ? getTarget(key) : null), [key]);
  return useSyncExternalStore(subscribe, read, read);
}

export function clearTargets(): void {
  targets.clear();
  emit();
}

registerWipeHook('chat_targets', clearTargets);
