/**
 * model/useInterruptLedger.ts — live resolution of per-interrupt cards across
 * tabs (contract `fanout-interrupt-decisions-v1.md` §6, §8, §9; binding
 * decision for Wave 2a-2 commit 11).
 *
 * The Web does NOT keep a parked execution's stream open. While the view holds
 * an open ledger card or a parked response, it listens on the project SSE
 * plane (`GET /api/v2/events/prompt_lib/{projectId}`, `PROJECT_EVENTS_PATH`)
 * for two events Main publishes after commit and relays to the conversation's
 * readers only. Which server-side family carries them is Main's choice; the
 * browser sees one project stream and these event names:
 *
 * - `agent_hitl_resolved` — data: the `fanout-interrupt-resolved` object plus
 *   `conversation_id` (the conversation uuid) and `root_response_id`. The named card is dropped from
 *   that response in this view. Its action and any value are never read.
 * - `agent_fanout_wake_admitted` — data: `{conversation_id, root_response_id,
 *   execution_id, generation}`. The view attaches to the new execution's
 *   stream through the existing reattach path, appending to the response.
 *
 * It reconciles (re-reads `GET …/interrupts`) once per response when it first
 * holds ledger work (a reload of a parked response), when the tab becomes
 * visible again, and on demand after a 409: the plane is best-effort (Main
 * sends no live copy past 256 readers), the ledger is the truth. No timer
 * polls. An event for another conversation, or one that does not parse,
 * changes nothing.
 */
import { useCallback, useEffect, useMemo, useRef } from 'react';

import { useSharedEventSource } from '@/shared/api/sse';
import { getConfig } from '@/shared/config';

import { listLedgerInterrupts } from '../api/interruptLedgerApi';
import { readInterruptResolved } from '../lib/fanoutFrames';
import { applyInterruptList, dropLedgerCardFromMessage, holdsLedgerWork } from '../lib/interruptLedger';
import type { ChatMessage } from '../lib/convertMessagesToChatHistory';

/** The SSE `event:` names Main publishes on the project plane. */
export const LEDGER_RESOLVED_EVENT = 'agent_hitl_resolved';
export const LEDGER_WAKE_EVENT = 'agent_fanout_wake_admitted';
/** The project SSE stream, under `vite_server_url` (Main `router.go` `/events/prompt_lib/{projectID}`). */
const PROJECT_EVENTS_PATH = '/events/prompt_lib';
/**
 * This hook's channel on the shared project stream, per conversation: the
 * ledgers of one conversation do the same work, so a frame runs once there;
 * another conversation's ledger gets every frame too.
 */
const LEDGER_CHANNEL = 'interrupt_ledger';

type SetChatHistory = (update: (previous: readonly ChatMessage[]) => readonly ChatMessage[]) => void;

/** The wake target the transport attaches to. */
interface LedgerWakeTarget {
  readonly responseMessageId: string;
  readonly executionId: string;
}

export interface UseInterruptLedgerParams {
  readonly projectId: string | number | undefined;
  /** The conversation uuid project-plane events name the conversation by. */
  readonly conversationUuid: string | undefined;
  readonly messages: readonly ChatMessage[];
  readonly setChatHistory: SetChatHistory;
  /** Attach the view to a woken execution; `false` when the transport could not. */
  readonly attachWake: (target: LedgerWakeTarget) => boolean;
}

/** Re-read one response's open cards and fold them in. A failed read changes nothing. */
export async function reconcileLedgerMessage(
  projectId: string | number | undefined,
  messageId: string,
  setChatHistory: SetChatHistory,
): Promise<void> {
  const listed = await listLedgerInterrupts(projectId, messageId);
  if (listed !== undefined) setChatHistory((previous) => applyInterruptList(previous, messageId, listed));
}

type Raw = Readonly<Record<string, unknown>>;

function parse(data: unknown): Raw | undefined {
  try {
    const value: unknown = JSON.parse(String(data));
    return typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as Raw) : undefined;
  } catch {
    return undefined;
  }
}

function idOf(value: unknown): string {
  return typeof value === 'string' || typeof value === 'number' ? String(value) : '';
}

export function useInterruptLedger({
  projectId, conversationUuid, messages, setChatHistory, attachWake,
}: UseInterruptLedgerParams): void {
  const working = useMemo(() => messages.filter(holdsLedgerWork).map((message) => message.id), [messages]);
  const workingKey = working.join('\u0000');
  const workingRef = useRef(working);
  workingRef.current = working;
  const attachRef = useRef(attachWake);
  attachRef.current = attachWake;
  const reconciled = useRef(new Set<string>());

  const ours = useCallback((data: Raw): string | undefined => {
    const conversation = idOf(data['conversation_id']).toLowerCase();
    const matches = conversation !== '' && conversation === conversationUuid?.toLowerCase();
    const response = idOf(data['root_response_id']);
    return matches && workingRef.current.includes(response) ? response : undefined;
  }, [conversationUuid]);

  // Once per response that holds ledger work: a reload restores its cards
  // from the ledger, because the stored row does not carry them.
  useEffect(() => {
    for (const messageId of workingKey ? workingKey.split('\u0000') : []) {
      if (reconciled.current.has(messageId)) continue;
      reconciled.current.add(messageId);
      void reconcileLedgerMessage(projectId, messageId, setChatHistory);
    }
  }, [workingKey, projectId, setChatHistory]);

  // Each time a response parks (again), its open cards are re-read from the
  // ledger: a tab that attached to a wake late, or missed a frame, matches
  // Main at every park instead of only after a reload.
  const parkedKey = useMemo(() => messages.filter((message) => message.fanoutParked).map((message) => message.id).join('\u0000'), [messages]);
  // `undefined` until the first render's parked set is known: what is parked
  // on load is reconciled once by the effect above.
  const parked = useRef<ReadonlySet<string> | undefined>(undefined);
  useEffect(() => {
    const now = new Set(parkedKey ? parkedKey.split('\u0000') : []);
    const before = parked.current;
    parked.current = now;
    if (!before) return;
    for (const messageId of now) {
      if (!before.has(messageId)) void reconcileLedgerMessage(projectId, messageId, setChatHistory);
    }
  }, [parkedKey, projectId, setChatHistory]);

  useEffect(() => {
    if (!workingKey || typeof document === 'undefined') return undefined;
    const onVisibilityChange = (): void => {
      if (document.visibilityState !== 'visible') return;
      for (const messageId of workingRef.current) void reconcileLedgerMessage(projectId, messageId, setChatHistory);
    };
    document.addEventListener('visibilitychange', onVisibilityChange);
    return () => document.removeEventListener('visibilitychange', onVisibilityChange);
  }, [workingKey, projectId, setChatHistory]);

  const config = getConfig();
  const serverUrl = config.status === 'ok' ? config.config.vite_server_url : null;
  const project = idOf(projectId);
  const streamUrl = workingKey && project && serverUrl ? `${serverUrl}${PROJECT_EVENTS_PATH}/${encodeURIComponent(project)}` : null;

  // The tab's one project stream, shared with the conversation list and the
  // handoff badges: a connection of its own would cost one more of the
  // browser's per-host and the server's per-principal stream slots per tab.
  useSharedEventSource(streamUrl, `${LEDGER_CHANNEL}:${conversationUuid ?? ''}`, {
    [LEDGER_RESOLVED_EVENT]: (event: MessageEvent) => {
      const data = parse(event.data);
      const response = data && ours(data);
      const resolved = data && readInterruptResolved(data);
      if (!response || !resolved) return;
      setChatHistory((previous) => dropLedgerCardFromMessage(previous, response, resolved.interrupt_key));
    },
    [LEDGER_WAKE_EVENT]: (event: MessageEvent) => {
      const data = parse(event.data);
      const response = data && ours(data);
      const executionId = idOf(data?.['execution_id']);
      if (!response || !executionId) return;
      attachRef.current({ responseMessageId: response, executionId });
    },
  });
}
