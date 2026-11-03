/**
 * Follow an unattended run chain after its answer settles (ADR-0045
 * decisions 4 and 11).
 *
 * Main writes to the transcript after the streamed answer settled, and sends
 * no frame for it: the chain root's `meta.run_brief` (pending, then ready or
 * failed), the `meta.goal_check` of a checked run, and the question/answer
 * groups of a continuation it starts by itself under the grant. Without a
 * read the open conversation showed none of it until a reload.
 *
 * While an answer of a chain is the last message and settled, the
 * conversation's latest groups are read about every 3 s (the conversation
 * read with `messages_limit` and `sort_order: desc`, the read the transport's
 * resume reconciliation uses: its groups carry the whole `meta`; the
 * `changes_since` delta would first need a full sync of the transcript for a
 * cursor, and its rows are the list shape, not the groups the converter
 * reads). Changed
 * marks are merged into the live messages; groups after the last one on
 * screen are appended. A continuation that is still running is the last
 * message then, and `useReattachSeededTurn` attaches to its stream.
 *
 * It stops when the chain is over (a goal check other than `not_met`, or no
 * brief or check is expected — a settled brief without conditions included —
 * and nothing new came within 15 s), when the
 * person sends, stops or leaves the conversation, or after 3 minutes without
 * a change.
 */
import { useCallback, useEffect, useRef, useState } from 'react';
import type { Dispatch, SetStateAction } from 'react';

import { conversationApi } from '@/entities/conversation';
import type { MessageGroupWire, MessageParticipantWire } from '@/entities/message';
import { convertMessagesToChatHistory } from '@/features/chat-messages';
import type { ChatMessage } from '@/features/chat-messages';

import { useStableRef } from './useStableRef';

export const CHAIN_POLL_MS = 3_000;
const CHAIN_IDLE_MS = 15_000;
export const CHAIN_QUIET_MS = 180_000;
const SNAPSHOT_GROUPS = 20;

/** Reads the conversation's latest messages, oldest first. */
export type ChainSnapshotReader = (signal: AbortSignal) => Promise<readonly ChatMessage[]>;

export interface UseChatBoxChainFollowParams {
  readonly enabled: boolean;
  readonly projectId: string | number | undefined;
  readonly conversationUuid: string | undefined;
  readonly messages: readonly ChatMessage[];
  readonly isStreaming: boolean;
  readonly setChatHistory: Dispatch<SetStateAction<readonly ChatMessage[]>>;
  /** Whether the next send carries a grant ("Run to completion" is on); read when an answer settles. */
  readonly granted: () => boolean;
  /** Tests inject the read; the default is the conversation read. */
  readonly readSnapshot?: ChainSnapshotReader | undefined;
}

export interface ChatBoxChainFollow {
  /** The person stopped the turn: do not follow its chain. */
  readonly cancel: () => void;
}

const MARK_KEYS = ['continuedAutomatically', 'continuationIndex', 'stepLimitReached', 'runBrief', 'goalCheck'] as const;

const same = (a: unknown, b: unknown): boolean => a === b || JSON.stringify(a) === JSON.stringify(b);

const isAssistant = (message: ChatMessage): boolean => message.role === 'assistant';
const inFlight = (message: ChatMessage): boolean => message.isStreaming === true || message.isLoading === true;
const atStepLimit = (message: ChatMessage): boolean =>
  message.stepLimitReached === true || message.responseMetadata?.['step_limit_reached'] === true;

/** The message is a continuation Main started: its own mark, or its question's. */
function isContinuation(message: ChatMessage, history: readonly ChatMessage[]): boolean {
  if (message.continuedAutomatically === true) return true;
  if (message.questionId === undefined) return false;
  return history.some((item) => item.id === message.questionId && item.continuedAutomatically === true);
}

/** The brief of the chain the message belongs to: its own, or the root's when it is a continuation. */
function chainBrief(message: ChatMessage, history: readonly ChatMessage[]): ChatMessage['runBrief'] {
  if (message.runBrief !== undefined) return message.runBrief;
  if (!isContinuation(message, history)) return undefined;
  const index = history.findIndex((item) => item.id === message.id);
  return history.slice(0, index === -1 ? history.length : index).findLast((item) => item.runBrief !== undefined)?.runBrief;
}

/**
 * More is due from Main for this tail: its answer, a continuation, a brief or
 * a goal check. A goal check is due only for a ready brief with at least one
 * condition: Main checks nothing else, so a settled brief without conditions
 * (a greeting) expects nothing more.
 */
function chainExpectsMore(history: readonly ChatMessage[]): boolean {
  const tail = history[history.length - 1];
  if (tail === undefined) return false;
  if (!isAssistant(tail)) return tail.continuedAutomatically === true;
  if (inFlight(tail) || atStepLimit(tail)) return true;
  if (tail.goalCheck !== undefined) return false;
  const brief = chainBrief(tail, history);
  if (brief?.state === 'pending') return true;
  return brief?.state === 'ready' && (brief.brief?.done_when.length ?? 0) > 0;
}

/** The chain is over at this tail: a goal check that starts no continuation. */
const chainOver = (tail: ChatMessage | undefined): boolean =>
  tail?.goalCheck !== undefined && tail.goalCheck.outcome !== 'not_met';

/** The settled answer carries a mark of an unattended chain. */
function hasChainMarks(message: ChatMessage, history: readonly ChatMessage[]): boolean {
  return atStepLimit(message) || message.runBrief !== undefined || message.continuationIndex !== undefined || isContinuation(message, history);
}

function mergeMarks(live: ChatMessage, read: ChatMessage): ChatMessage {
  if (MARK_KEYS.every((key) => same(live[key], read[key]))) return live;
  const next: Record<string, unknown> = { ...live };
  for (const key of MARK_KEYS) next[key] = read[key];
  return next as unknown as ChatMessage;
}

/**
 * The live history with a snapshot of the latest messages merged in: the
 * chain marks of a settled message (never the content a stream wrote), the
 * read row of a settled answer that runs again (an output-limit continuation
 * reuses its response), and the messages after the last one both have.
 * Answers `live` itself when nothing changed.
 */
export function mergeChainSnapshot(live: readonly ChatMessage[], read: readonly ChatMessage[]): readonly ChatMessage[] {
  const readById = new Map(read.map((message) => [message.id, message]));
  let changed = false;
  const merged = live.map((message) => {
    const fresh = readById.get(message.id);
    if (fresh === undefined || inFlight(message)) return message;
    const resumed = fresh.isStreaming === true && typeof fresh.taskId === 'string' && fresh.taskId !== message.taskId;
    const next = resumed ? { ...fresh, questionId: message.questionId ?? fresh.questionId } : mergeMarks(message, fresh);
    if (next !== message) changed = true;
    return next;
  });
  const liveIds = new Set(live.map((message) => message.id));
  const anchor = read.findLastIndex((message) => liveIds.has(message.id));
  const added = anchor === -1 ? [] : read.slice(anchor + 1);
  if (!changed && added.length === 0) return live;
  return [...merged, ...added];
}

function conversationReader(projectId: string | number, conversationUuid: string): ChainSnapshotReader {
  return async (signal) => {
    const snapshot = await conversationApi.details({ projectId, id: conversationUuid, messages_limit: SNAPSHOT_GROUPS, sort_order: 'desc' }, signal);
    const groups = snapshot['message_groups'];
    if (!Array.isArray(groups)) return [];
    return convertMessagesToChatHistory(groups as MessageGroupWire[], snapshot.participants as readonly MessageParticipantWire[] | undefined);
  };
}

interface Followed {
  readonly conversation: string | undefined;
  /** Answers settled in this view, or appended by a follow, whose chain may go on. */
  readonly ids: ReadonlySet<string>;
  /** The answer the person stopped. */
  readonly cancelled?: string | undefined;
}

/** `prev` for this conversation (a fresh record after a switch), with `ids` followed too. */
function follow(prev: Followed, conversation: string | undefined, ids: readonly string[]): Followed {
  const base: Followed = prev.conversation === conversation ? prev : { conversation, ids: new Set() };
  const fresh = ids.filter((id) => !base.ids.has(id) && id !== base.cancelled);
  return fresh.length === 0 ? base : { ...base, ids: new Set([...base.ids, ...fresh]) };
}

export function useChatBoxChainFollow(params: UseChatBoxChainFollowParams): ChatBoxChainFollow {
  const { enabled, projectId, conversationUuid, messages, isStreaming, setChatHistory, granted, readSnapshot } = params;
  const messagesRef = useStableRef(messages);
  const grantedRef = useStableRef(granted);
  const [followed, setFollowed] = useState<Followed>({ conversation: conversationUuid, ids: new Set() });
  const current: Followed = followed.conversation === conversationUuid ? followed : { conversation: conversationUuid, ids: new Set() };
  const abortRef = useRef<AbortController | null>(null);

  // An answer settled in this view: follow its chain when the send carried a grant or the answer is marked.
  const wasStreaming = useRef(isStreaming);
  useEffect(() => {
    const settled = wasStreaming.current && !isStreaming;
    wasStreaming.current = isStreaming;
    if (!settled || !enabled) return;
    const history = messagesRef.current;
    const tail = history[history.length - 1];
    if (tail === undefined || !isAssistant(tail) || Boolean(tail.exception) || tail.failureCode !== undefined) return;
    if (!grantedRef.current() && !hasChainMarks(tail, history)) return;
    setFollowed((prev) => follow(prev, conversationUuid, [tail.id]));
  }, [isStreaming, enabled, conversationUuid, messagesRef, grantedRef]);

  const tail = messages[messages.length - 1];
  const tailId = tail?.id;
  const followable = ((): boolean => {
    if (!enabled || isStreaming || tail === undefined || tail.id === current.cancelled || chainOver(tail)) return false;
    if (isAssistant(tail) && inFlight(tail)) return false;
    if (current.ids.has(tail.id)) return true;
    // A brief still being written is followed wherever it is seen, a reload included.
    return isAssistant(tail) && chainBrief(tail, messages)?.state === 'pending';
  })();

  useEffect(() => {
    if (!followable || projectId === undefined || conversationUuid === undefined || tailId === undefined) return undefined;
    // Once followed, the tail stays followed when its brief stops being pending.
    setFollowed((prev) => follow(prev, conversationUuid, [tailId]));
    const read = readSnapshot ?? conversationReader(projectId, conversationUuid);
    const controller = new AbortController();
    abortRef.current = controller;
    const startedAt = Date.now();
    let lastChangeAt = startedAt;
    let timer: ReturnType<typeof setTimeout> | undefined;

    const tick = async (): Promise<void> => {
      if (controller.signal.aborted) return;
      let snapshot: readonly ChatMessage[] = [];
      try {
        snapshot = await read(controller.signal);
      } catch {
        // A failed read is a read with nothing new; the quiet bound ends the follow.
      }
      if (controller.signal.aborted) return;
      const live = messagesRef.current;
      const merged = mergeChainSnapshot(live, snapshot);
      const now = Date.now();
      if (merged !== live) {
        lastChangeAt = now;
        const added = merged.slice(live.length).map((message) => message.id);
        if (added.length > 0) setFollowed((prev) => follow(prev, conversationUuid, added));
        setChatHistory((previous) => mergeChainSnapshot(previous, snapshot));
        // A new tail is followed by the next run of this effect.
        if (added.length > 0) return;
      }
      const last = merged[merged.length - 1];
      if (chainOver(last) || now - lastChangeAt >= CHAIN_QUIET_MS) return;
      if (!chainExpectsMore(merged) && now - lastChangeAt >= CHAIN_IDLE_MS) return;
      timer = setTimeout(() => void tick(), CHAIN_POLL_MS);
    };
    timer = setTimeout(() => void tick(), CHAIN_POLL_MS);
    return () => {
      controller.abort();
      if (timer !== undefined) clearTimeout(timer);
      if (abortRef.current === controller) abortRef.current = null;
    };
  }, [followable, projectId, conversationUuid, tailId, readSnapshot, messagesRef, setChatHistory]);

  const tailRef = useStableRef(tailId);
  const cancel = useCallback(() => {
    abortRef.current?.abort();
    const stopped = tailRef.current;
    setFollowed((prev) => ({ ...follow(prev, conversationUuid, []), cancelled: stopped }));
  }, [conversationUuid, tailRef]);

  return { cancel };
}
