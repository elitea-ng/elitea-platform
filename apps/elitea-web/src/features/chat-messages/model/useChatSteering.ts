/**
 * Steering during execution (client contract 1.26) for the cloud chat.
 *
 * WHERE A SEND GOES. While a turn of this chat runs and the transport owns
 * it, the composer text goes to `POST .../steering` as a steering message.
 * The worker injects it at the next safe point of the SAME turn. When the
 * server refuses, nothing is lost; the text goes the old way:
 *
 *  - `turn_not_running`: the turn ended first, so the text waits in the A17
 *    queue and goes as a normal new message when the composer settles.
 *  - `steering_not_supported` (a pipeline): the same A17 queue, and later
 *    sends of this turn go there directly.
 *  - `rate_limited` (429): the hook waits `retry_after_ms` (the notice
 *    counts the seconds down) and sends the SAME text with the SAME
 *    Idempotency-Key again, at most `STEERING_RETRY_LIMIT` times. The key
 *    makes sure that the server keeps one message only. After the last
 *    try, the text goes to the A17 queue, plus a short notice.
 *  - any other refusal (`steering_queue_full`, a network failure, ...): the
 *    A17 queue, plus a short notice.
 *
 * WHEN A MESSAGE IS SENT AGAIN. A message that the turn did not inject is
 * `turn_ended` on the server after the turn. This tab (and only this tab,
 * see `chatSteering.store.ts`'s `own`) then joins the texts of its own
 * leftover messages with a blank line, queues ONE normal message (A17) and
 * DELETEs the rows. It reads the server list for this, never only the
 * local view: the SSE has no `turn_ended` event, and a switch to another
 * conversation also closes the stream while the turn runs on.
 */
import { useCallback, useEffect, useRef, useState } from 'react';

import {
  STEERING_ERROR,
  STEERING_RETRY_DEFAULT_MS,
  STEERING_RETRY_LIMIT,
  STEERING_STATE,
  joinSteeringTexts,
  leftoverSteering,
  steeringWaitSeconds,
  type SteeringItem,
} from '@elitea/client-core/stream';

import { editSteering, listSteering, newSteeringKey, queueSteering, removeSteering, type SteeringTarget } from '@/entities/conversation/api/steeringApi';
import { t } from '@/shared/i18n';

import { useChatSteeringStore } from './chatSteering.store';

/** The answer that the owned run writes, as the transport reports it. */
export interface ChatSteeringRunTarget {
  readonly projectId: string | number;
  readonly messageGroupUuid: string;
}

export interface UseChatSteeringParams {
  /** The A17 queue key of the conversation on screen. */
  readonly conversationKey: string;
  /** A turn of this chat is open (the composer shows Stop). */
  readonly isStreaming: boolean;
  /** The answer of the run that the transport owns now, or `null`. */
  readonly streamTarget: () => ChatSteeringRunTarget | null;
  /** The send of today (A17): sends now when idle, queues for after the turn otherwise. */
  readonly send: (question: string) => void;
  /** Puts a text in the A17 queue; it goes as a normal message when the composer settles. */
  readonly queueAfterTurn: (question: string) => void;
}

export interface ChatSteeringModel {
  /** What the composer calls. */
  readonly onSend: (question: string) => void;
  readonly edit: (item: SteeringItem, content: string) => Promise<void>;
  readonly remove: (item: SteeringItem) => Promise<void>;
  /** A reattach after a reload opened the stream: read the list of its answer. */
  readonly onReattached: () => void;
  /** A short text about the last refused send, or `null`. */
  readonly notice: string | null;
  readonly dismissNotice: () => void;
}

/** The waits before the list is read again while own messages still show `pending`. */
const SETTLE_RETRY_MS = [1_500, 4_000] as const;

function targetOf(responseMessageId: string, projectId: string): SteeringTarget {
  return { projectId, responseMessageId };
}

/** The time between two updates of the countdown. */
const COUNTDOWN_TICK_MS = 250;

const sleep = (ms: number) => new Promise<void>((resolve) => { setTimeout(resolve, ms); });

/**
 * The countdown of a rate-limited send. `start(ms)` shows "You can send
 * again in N s" until the wait ends; the text updates each tick. A later
 * wait that ends later replaces the current one.
 */
function useRetryCountdown(): { readonly text: string | null; readonly start: (ms: number) => void; readonly clear: () => void } {
  const [until, setUntil] = useState<number | null>(null);
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (until === null) return undefined;
    const timer = setInterval(() => {
      const at = Date.now();
      setNow(at);
      if (at >= until) setUntil(null);
    }, COUNTDOWN_TICK_MS);
    return () => { clearInterval(timer); };
  }, [until]);
  const start = useCallback((ms: number) => {
    const at = Date.now();
    setNow(at);
    setUntil((current) => Math.max(current ?? 0, at + ms));
  }, []);
  const clear = useCallback(() => { setUntil(null); }, []);
  const left = until === null ? 0 : until - now;
  const text = left > 0
    ? t('chatMessages.steering.rateLimited', 'Too many messages at once. You can send again in {{seconds}} s. Your message goes again then.', { seconds: String(steeringWaitSeconds(left)) })
    : null;
  return { text, start, clear };
}

function failureNotice(code: string | undefined): string {
  if (code === STEERING_ERROR.queueFull) {
    return t('chatMessages.steering.queueFull', 'This turn has too many waiting messages. Your message is sent after the turn.');
  }
  return t('chatMessages.steering.failed', 'Your message could not reach the running turn. It is sent after the turn.');
}

// eslint-disable-next-line eslint/max-lines-per-function -- one routing hook: send, edit, remove and the end-of-turn rule share the same refs
export function useChatSteering(params: UseChatSteeringParams): ChatSteeringModel {
  const { conversationKey, isStreaming } = params;
  const [notice, setNotice] = useState<string | null>(null);
  const live = useRef(params);
  live.current = params;
  const settling = useRef(new Set<string>());
  const { text: countdownText, start: startCountdown, clear: clearCountdown } = useRetryCountdown();
  /** Texts whose send failed while another conversation was on screen, by conversation key. */
  const stranded = useRef(new Map<string, string[]>());

  const refresh = useCallback(async (target: SteeringTarget): Promise<void> => {
    try {
      useChatSteeringStore.getState().apply(target.responseMessageId, await listSteering(target));
    } catch {
      // Handled: the SSE frames still fill the view; the next settle reads again.
    }
  }, []);

  /**
   * Applies the end-of-turn rule to one answer of this conversation. Reads
   * the server list; `turn_ended` own messages become ONE A17 message and
   * their rows are deleted. Own messages that are still `pending` keep the
   * record, and the list is read again a little later (the server can mark
   * them a moment after the terminal frame).
   */
  const settle = useCallback(async (responseId: string, attempt: number): Promise<void> => {
    const record = useChatSteeringStore.getState().own[responseId];
    if (record === undefined || settling.current.has(responseId)) return;
    settling.current.add(responseId);
    try {
      const target = targetOf(responseId, record.projectId);
      const items = await listSteering(target);
      const store = useChatSteeringStore.getState();
      store.apply(responseId, items);
      const own = new Set(record.ids);
      const leftovers = leftoverSteering(items, own, false);
      // Another conversation is on screen now: its A17 queue is not this one. Keep the record for the reopen.
      if (live.current.conversationKey !== record.conversationKey) return;
      const text = joinSteeringTexts(leftovers);
      if (text !== '') live.current.queueAfterTurn(text);
      await Promise.all(leftovers.map((item) => removeSteering(target, item.id)));
      store.forget(responseId, leftovers.map((item) => item.id));
      const open = items.filter((item) => own.has(item.id) && item.state === STEERING_STATE.Pending);
      store.dropOwn(responseId, record.ids.filter((id) => !open.some((item) => item.id === id)));
      const wait = SETTLE_RETRY_MS[attempt];
      if (open.length > 0 && wait !== undefined) {
        setTimeout(() => {
          if (!live.current.isStreaming) void settle(responseId, attempt + 1);
        }, wait);
      }
    } catch {
      // Handled: the record stays, and the next settle (turn end, reopen) reads again.
    } finally {
      settling.current.delete(responseId);
    }
  }, []);

  /** Puts a refused text in the A17 queue of the conversation that sent it. */
  const fallBack = useCallback((conversationKey: string, question: string): void => {
    if (live.current.conversationKey === conversationKey) {
      live.current.queueAfterTurn(question);
      return;
    }
    // Another conversation is on screen: its A17 queue is not the right one. Keep the text for the reopen.
    stranded.current.set(conversationKey, [...(stranded.current.get(conversationKey) ?? []), question]);
  }, []);

  /**
   * Sends one steering message. A network failure is tried one more time,
   * and a `rate_limited` refusal again after its wait, always with the SAME
   * key: the server then answers the first message again, so the text is
   * not kept two times.
   */
  const post = useCallback(async (call: SteeringTarget, question: string, key: string) => {
    let result = await queueSteering(call, question, key);
    for (let retry = 0; ; retry++) {
      if (!result.ok && result.status === undefined) result = await queueSteering(call, question, key);
      if (result.ok || result.code !== STEERING_ERROR.rateLimited || retry >= STEERING_RETRY_LIMIT) return result;
      const wait = result.retryAfterMs ?? STEERING_RETRY_DEFAULT_MS;
      startCountdown(wait);
      await sleep(wait);
      result = await queueSteering(call, question, key);
    }
  }, [startCountdown]);

  const steer = useCallback(async (target: ChatSteeringRunTarget, question: string): Promise<void> => {
    const responseId = target.messageGroupUuid;
    const conversationKey = live.current.conversationKey;
    const result = await post({ projectId: target.projectId, responseMessageId: responseId }, question, newSteeringKey());
    const store = useChatSteeringStore.getState();
    if (result.ok) {
      if (result.item === undefined) return;
      store.addOwn(responseId, { projectId: String(target.projectId), conversationKey }, result.item.id);
      store.apply(responseId, [result.item]);
      // The turn ended while the POST was on its way: the end-of-turn rule must still see this message.
      if (!live.current.isStreaming) void settle(responseId, 0);
      return;
    }
    if (result.code === STEERING_ERROR.notSupported) store.markUnsupported(responseId);
    fallBack(conversationKey, question);
    // The last try was refused too: the countdown must not hide that the text now waits for the turn end.
    if (result.code === STEERING_ERROR.rateLimited) clearCountdown();
    if (result.code !== STEERING_ERROR.notSupported && result.code !== STEERING_ERROR.turnNotRunning) setNotice(failureNotice(result.code));
  }, [settle, post, fallBack, clearCountdown]);

  const onSend = useCallback((question: string): void => {
    if (question.trim() === '') return;
    const current = live.current;
    const target = current.isStreaming ? current.streamTarget() : null;
    if (target === null || useChatSteeringStore.getState().unsupported[target.messageGroupUuid] === true) {
      current.send(question);
      return;
    }
    void steer(target, question);
  }, [steer]);

  const recordOf = (item: SteeringItem) => useChatSteeringStore.getState().own[item.responseMessageId];

  const edit = useCallback(async (item: SteeringItem, content: string): Promise<void> => {
    const record = recordOf(item);
    if (record === undefined || content.trim() === '') return;
    const target = targetOf(item.responseMessageId, record.projectId);
    const result = await editSteering(target, item.id, content);
    if (result.ok) {
      if (result.item) useChatSteeringStore.getState().apply(item.responseMessageId, [result.item]);
      return;
    }
    // `steering_not_pending`: the turn injected it first; the list shows it as delivered.
    if (result.code === STEERING_ERROR.notPending) await refresh(target);
    else setNotice(t('chatMessages.steering.editFailed', 'The message could not be changed. Try again.'));
  }, [refresh]);

  const remove = useCallback(async (item: SteeringItem): Promise<void> => {
    const record = recordOf(item);
    if (record === undefined) return;
    const target = targetOf(item.responseMessageId, record.projectId);
    const result = await removeSteering(target, item.id);
    const store = useChatSteeringStore.getState();
    if (result.ok) {
      store.apply(item.responseMessageId, [{ ...item, state: STEERING_STATE.Removed }]);
      store.dropOwn(item.responseMessageId, [item.id]);
      return;
    }
    if (result.code === STEERING_ERROR.notPending) await refresh(target);
    else setNotice(t('chatMessages.steering.removeFailed', 'The message could not be removed. Try again.'));
  }, [refresh]);

  // When no turn of this chat runs: settle every answer that this tab steered here.
  useEffect(() => {
    const texts = stranded.current.get(conversationKey);
    if (texts !== undefined) {
      stranded.current.delete(conversationKey);
      for (const text of texts) live.current.queueAfterTurn(text);
    }
    if (isStreaming) return;
    for (const [responseId, record] of Object.entries(useChatSteeringStore.getState().own)) {
      if (record.conversationKey === conversationKey) void settle(responseId, 0);
    }
  }, [conversationKey, isStreaming, settle]);

  const dismissNotice = useCallback(() => {
    setNotice(null);
    clearCountdown();
  }, [clearCountdown]);
  const onReattached = useCallback(() => {
    const target = live.current.streamTarget();
    if (target) void refresh({ projectId: target.projectId, responseMessageId: target.messageGroupUuid });
  }, [refresh]);

  return { onSend, edit, remove, onReattached, notice: countdownText ?? notice, dismissNotice };
}
