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

export interface IntentHandoff {
  agent: { projectId: number; agentId: number } | null;
  prefill: string | null;
}

/** A hand-over is used within seconds; an older one is gone. */
export const HANDOFF_MAX_AGE_MS = 10 * 60_000;

const handoffs = new Map<string, { handoff: IntentHandoff; at: number }>();

export function handOff(handoff: IntentHandoff, now: number = Date.now()): string {
  for (const [id, entry] of handoffs) if (now - entry.at > HANDOFF_MAX_AGE_MS) handoffs.delete(id);
  const id = randomUUID();
  handoffs.set(id, { handoff: { agent: handoff.agent, prefill: handoff.prefill }, at: now });
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

export function clearHandoffs(): void {
  handoffs.clear();
}

registerWipeHook('intent_handoffs', clearHandoffs);
