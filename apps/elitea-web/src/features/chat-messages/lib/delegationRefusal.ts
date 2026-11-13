/**
 * FU-335: the notice of a delegation refused at start.
 *
 * The Rust worker sends it as an `agent_delegation_refused` node event that
 * elitea-main shows only beside the run's FAILED terminal, and persists on the
 * message group's `meta.delegation_refused` so a reload shows it too. It is
 * display data: the node kind and the display name of the toolkit the missing
 * grant belongs to. An author wrote that name, so it is bounded and shown as a
 * plain React text node: never parsed as HTML or markdown, so it is inert and a
 * name such as `Jira <prod>` is shown exactly as written.
 */
import { createAssistantMessage, findTarget, replaceAt, type ChatStreamContext } from './chatStreamShared';
import type { ChatMessage } from './convertMessagesToChatHistory.types';
import type { ChatStreamFrame } from './chatStreamFrame';

/** The longest toolkit display name a notice shows, in characters. */
export const MAX_DELEGATION_TOOLKIT_NAME_CHARS = 128;

export interface DelegationRefusal {
  readonly node: 'agent' | 'pipeline';
  readonly toolkit?: string;
}

/** The name as one line of text, bounded; whitespace runs collapse to one space. */
function displayName(value: unknown): string | undefined {
  if (typeof value !== 'string') return undefined;
  const text = value.replace(/\s+/g, ' ').trim();
  const bounded = Array.from(text).slice(0, MAX_DELEGATION_TOOLKIT_NAME_CHARS).join('');
  return bounded === '' ? undefined : bounded;
}

/** The wire value is `{node: 'agent' | 'pipeline', toolkit?: string | null}`. */
export function parseDelegationRefusal(raw: unknown): DelegationRefusal | undefined {
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) return undefined;
  const record = raw as Record<string, unknown>;
  const node = record['node'];
  if (node !== 'agent' && node !== 'pipeline') return undefined;
  const toolkit = displayName(record['toolkit']);
  return toolkit === undefined ? { node } : { node, toolkit };
}

/** Keep the notice on the answer it belongs to; it can be the run's first frame. */
export function applyDelegationRefusalFrame(
  history: readonly ChatMessage[],
  frame: ChatStreamFrame,
  context: ChatStreamContext,
): readonly ChatMessage[] {
  if (frame.type !== 'agent_delegation_refused') return history;
  const raw = frame.response_metadata?.['delegation_refused'];
  if (parseDelegationRefusal(raw) === undefined) return history;
  const index = findTarget(history, frame);
  const current = history[index];
  return current
    ? replaceAt(history, index, { delegationRefusal: raw })
    : [...history, { ...createAssistantMessage(frame, context), delegationRefusal: raw }];
}
