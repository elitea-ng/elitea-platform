import { randomUUID } from 'expo-crypto';

import { registerWipeHook } from '@/security/endSession';

/**
 * What a Siri / Shortcuts intent asks of /new (an agent to start, a question
 * for the composer), handed over IN MEMORY under a random one-time id
 * (`?intent=<id>`), never as route params. Any web page or app can open
 * `agentzefir://new?...`: only an intent the app itself routed
 * (src/features/intents/useAppIntentHandler.ts, which checks the workspace)
 * can start a conversation or put text in a composer.
 */

/**
 * What the new conversation does once open, asked by a Home card the user tapped (never by a
 * URL): start dictation, or open the photo or file picker. Done once.
 */
export type ThenAction = 'dictate' | 'photos' | 'files';

export interface IntentHandoff {
  agent: { projectId: number; agentId: number } | null;
  prefill: string | null;
  then?: ThenAction | null;
}

/** A hand-over is used within seconds; an older one is gone. */
export const HANDOFF_MAX_AGE_MS = 10 * 60_000;

const handoffs = new Map<string, { handoff: IntentHandoff; at: number }>();

export function handOff(handoff: IntentHandoff, now: number = Date.now()): string {
  for (const [id, entry] of handoffs) if (now - entry.at > HANDOFF_MAX_AGE_MS) handoffs.delete(id);
  const id = randomUUID();
  const then = handoff.then ? { then: handoff.then } : {};
  handoffs.set(id, { handoff: { agent: handoff.agent, prefill: handoff.prefill, ...then }, at: now });
  return id;
}

/** The hand-over under `id`, or null (unknown, expired, not a string: a URL someone else built). */
export function readHandoff(id: unknown, now: number = Date.now()): IntentHandoff | null {
  if (typeof id !== 'string') return null;
  const entry = handoffs.get(id);
  if (!entry) return null;
  if (now - entry.at > HANDOFF_MAX_AGE_MS) {
    handoffs.delete(id);
    return null;
  }
  return entry.handoff;
}

/** The agent was started: a second visit of the same hand-over starts nothing. */
export function markAgentStarted(id: unknown): void {
  if (typeof id !== 'string') return;
  const entry = handoffs.get(id);
  if (entry) entry.handoff = { ...entry.handoff, agent: null };
}

/** The "then" action ran: a second visit of the same hand-over (Back, then forward) runs nothing. */
export function markThenDone(id: unknown): void {
  if (typeof id !== 'string') return;
  const entry = handoffs.get(id);
  if (entry) entry.handoff = { agent: entry.handoff.agent, prefill: entry.handoff.prefill };
}

export function clearHandoffs(): void {
  handoffs.clear();
}

registerWipeHook('intent_handoffs', clearHandoffs);
