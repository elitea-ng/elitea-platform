/**
 * Siri / Shortcuts invocations (app-intents/ZefirIntents.swift) to app
 * screens. Every intent is OPEN-APP only: it opens a screen behind the device
 * lock and returns nothing to Siri. "Ask <agent>" puts the question typed into
 * a Shortcut in the composer; it is never sent without the user tapping Send.
 * Siri never collects the question by voice (app-intents/ZefirIntents.swift):
 * by voice the intent carries no text and the composer opens empty.
 *
 * Agent ids are `<workspace slot>:<project id>:<agent id>`
 * (src/features/intents/agentCatalog.ts). An agent of another workspace (a
 * shortcut saved before a switch) is not opened: the intent falls back to the
 * agent picker of the active workspace.
 */

export const INTENT_NAMES = ['newChat', 'askAgent', 'openNotifications'] as const;
export type IntentName = (typeof INTENT_NAMES)[number];

/**
 * How old an invocation may be when the app handles it. An intent opens the
 * app at once, so a live one is handled within seconds (an unlock included);
 * an older one was queued while nothing could handle it (signed out, on the
 * connect screen) and may be meant for another workspace: it is dropped.
 */
export const MAX_INVOCATION_AGE_MS = 5 * 60_000;

/** True when an invocation (createdAt: ms since the epoch) is too old to act on. */
export function isStaleInvocation(invocation: { createdAt?: unknown }, now: number): boolean {
  const at = invocation.createdAt;
  if (typeof at !== 'number' || !Number.isFinite(at)) return true;
  return now - at > MAX_INVOCATION_AGE_MS;
}

/** The longest question an intent may put in the composer. */
export const MAX_PREFILL = 4000;

export interface AgentRef {
  workspaceId: string;
  projectId: number;
  agentId: number;
}

export function agentEntityId(ref: AgentRef): string {
  return `${ref.workspaceId}:${ref.projectId}:${ref.agentId}`;
}

export function parseAgentEntityId(raw: unknown): AgentRef | null {
  if (typeof raw !== 'string') return null;
  const match = /^([0-9A-Za-z_-]{1,64}):(\d{1,15}):(\d{1,15})$/.exec(raw);
  if (!match) return null;
  const projectId = Number(match[2]);
  const agentId = Number(match[3]);
  if (!Number.isSafeInteger(projectId) || projectId <= 0 || !Number.isSafeInteger(agentId) || agentId <= 0) return null;
  return { workspaceId: match[1]!, projectId, agentId };
}

export type IntentAction =
  | { kind: 'open'; href: '/notifications' }
  | { kind: 'newChat'; agent: { projectId: number; agentId: number } | null; prefill: string | null }
  /** Not an intent this build knows, or a malformed one: removed, nothing opens. */
  | { kind: 'discard' };

function cleanPrefill(raw: unknown): string | null {
  if (typeof raw !== 'string') return null;
  const text = raw.replace(/\r\n?/g, '\n').trim();
  if (text === '') return null;
  return text.length > MAX_PREFILL ? text.slice(0, MAX_PREFILL) : text;
}

export function routeIntent(invocation: { name: string; params: Record<string, unknown> }, activeWorkspaceId: string | null): IntentAction {
  const params = invocation.params ?? {};
  switch (invocation.name) {
    case 'openNotifications':
      return { kind: 'open', href: '/notifications' };
    case 'newChat':
    case 'askAgent': {
      const ref = parseAgentEntityId(params.agentId);
      const agent = ref !== null && ref.workspaceId === activeWorkspaceId ? { projectId: ref.projectId, agentId: ref.agentId } : null;
      const prefill = invocation.name === 'askAgent' ? cleanPrefill(params.text) : null;
      return { kind: 'newChat', agent, prefill };
    }
    default:
      return { kind: 'discard' };
  }
}
