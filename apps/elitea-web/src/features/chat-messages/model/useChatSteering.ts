/**
 * Steering during execution (client contract 1.27) for the cloud chat.
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
 * see `chatSteering.store.ts`'s `own`) then calls the hand-off
 * (`POST .../steering/handoff`). The hand-off takes the caller's
 * `turn_ended` rows in one step (`handed_off`) and returns them. The tab
 * joins ONLY the returned texts with a blank line and queues ONE normal
 * message (A17). An empty answer means that the server took them first
 * (about 10 s after the turn end it starts the next turn itself): the tab
 * sends nothing. So the server and a client never both start a turn. The
 * tab makes the question id of that message BEFORE the hand-off, and the
 * hand-off body names it: when the tab closes before it sends, the server
 * starts that turn itself with the same question id. The Idempotency-Key
 * and the question id of the hand-off stay in the `own` record until the
 * answer is used, so a retry (also after a reload) gets the same rows and
 * sends the same question. The tab reads the server list first, never only
 * the local view: the SSE has no `turn_ended` event, and a switch to another
 * conversation also closes the stream while the turn runs on.
 *
 * A STOPPED OR FAILED TURN. The server never continues a turn that the user
 * stopped (`onStop`) or that failed (`answerFailed`). Its `turn_ended`
 * messages wait, and this tab does not send them by itself: it OFFERS them
 * (`offer`). "Send" hands them off and sends them; "Dismiss" leaves them on
 * the server and forgets them here.
 *
 * CARRY-OVER. When the user sent a newer message before the hand-off, the
 * server moves the leftover messages to the NEWER answer (`steer.queued` on
 * the newer turn's stream). The store moves the item and this tab's
 * ownership with it (`chatSteering.store.ts`), so the end-of-turn rule of
 * the newer answer hands them off, and the rule of the old answer does not.
 * A carry-over to an answer whose turn already ended runs the rule again.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import {
  STEERING_ERROR,
  STEERING_RETRY_DEFAULT_MS,
  STEERING_RETRY_LIMIT,
  STEERING_STATE,
  joinSteeringTexts,
  leftoverSteering,
  type SteeringItem,
} from '@elitea/client-core/stream';

import {
  editSteering,
  handOffSteering,
  listSteering,
  newSteeringKey,
  queueSteering,
  removeSteering,
  type SteeringTarget,
} from '@/entities/conversation/api/steeringApi';
import { t } from '@/shared/i18n';

import { useChatSteeringStore, type SteeringHandoff } from './chatSteering.store';
import { failureNotice, sleep, useRetryCountdown } from './chatSteeringNotice';

/** The answer that the owned run writes, as the transport reports it. */
interface ChatSteeringRunTarget {
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
  /**
   * Puts a text in the A17 queue; it goes as a normal message when the
   * composer settles. With `questionId`, that send MUST use it as its
   * `question_id` (the hand-off named it to the server).
   */
  readonly queueAfterTurn: (question: string, questionId?: string) => void;
  /** True when the answer failed (the server does not continue a failed turn). */
  readonly answerFailed?: ((responseMessageId: string) => boolean) | undefined;
}

/** The leftover texts of a stopped or failed turn, which the user can send as the next message. */
export interface SteeringOffer {
  readonly responseMessageId: string;
  readonly text: string;
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
  /** The user pressed Stop (not "Stop and send"): the leftovers of this turn are offered, not sent. */
  readonly onStop: () => void;
  /** The leftover texts of a stopped or failed turn of this conversation, or `null`. */
  readonly offer: SteeringOffer | null;
  /** "Send": hands the offered texts off and sends them as the next message. */
  readonly acceptOffer: () => void;
  /** "Dismiss": leaves the offered texts on the server and forgets them here. */
  readonly dismissOffer: () => void;
}

interface HeldOffer extends SteeringOffer {
  readonly conversationKey: string;
}

/** A text for the A17 queue of a conversation that is not on screen. */
interface Stranded {
  readonly text: string;
  readonly questionId: string | undefined;
}

/** The waits before the list is read again while own messages still show `pending`. */
const SETTLE_RETRY_MS = [1_500, 4_000] as const;

function targetOf(responseMessageId: string, projectId: string): SteeringTarget {
  return { projectId, responseMessageId };
}

// eslint-disable-next-line eslint/max-lines-per-function -- one routing hook: send, edit, remove and the end-of-turn rule share the same refs
export function useChatSteering(params: UseChatSteeringParams): ChatSteeringModel {
  const { conversationKey, isStreaming } = params;
  const [notice, setNotice] = useState<string | null>(null);
  const live = useRef(params);
  live.current = params;
  const settling = useRef(new Set<string>());
  const { text: countdownText, start: startCountdown, clear: clearCountdown } = useRetryCountdown();
  /** Texts that must go while another conversation was on screen, by conversation key. */
  const stranded = useRef(new Map<string, Stranded[]>());
  /** The answers that the user stopped in this page life (the record can come after the Stop). */
  const stopped = useRef(new Set<string>());
  const [offers, setOffers] = useState<Readonly<Record<string, HeldOffer>>>({});

  const refresh = useCallback(async (target: SteeringTarget): Promise<void> => {
    try {
      useChatSteeringStore.getState().apply(target.responseMessageId, await listSteering(target));
    } catch {
      // Handled: the SSE frames still fill the view; the next settle reads again.
    }
  }, []);

  /** Puts a text in the A17 queue of the conversation that sent it, with the question id that its send must use. */
  const deliver = useCallback((conversationKey: string, text: string, questionId?: string): void => {
    if (live.current.conversationKey === conversationKey) {
      if (questionId === undefined) live.current.queueAfterTurn(text);
      else live.current.queueAfterTurn(text, questionId);
      return;
    }
    // Another conversation is on screen: its A17 queue is not the right one. Keep the text for the reopen.
    stranded.current.set(conversationKey, [...(stranded.current.get(conversationKey) ?? []), { text, questionId }]);
  }, []);

  /** The server does not continue this turn: the user stopped it, or it failed. */
  const isHeld = (responseId: string, held: boolean | undefined): boolean =>
    held === true || stopped.current.has(responseId) || live.current.answerFailed?.(responseId) === true;

  /**
   * Applies the end-of-turn rule to one answer of this conversation. Reads
   * the server list. When own messages are `turn_ended`, calls the hand-off
   * and queues ONLY the texts that it returns as ONE A17 message, with the
   * question id that the hand-off named. When the server does not continue
   * the turn (`isHeld`), it offers the texts instead, and keeps the record.
   * Own messages that are still `pending` keep the record, and the list is
   * read again a little later (the server can mark them a moment after the
   * terminal frame). On a failure the record keeps the hand-off keys, so the
   * next settle sends the same keys and gets the same rows.
   */
  const settle = useCallback(async (responseId: string, attempt: number): Promise<void> => {
    const record = useChatSteeringStore.getState().own[responseId];
    if (record === undefined || settling.current.has(responseId)) return;
    settling.current.add(responseId);
    // Runs this settle again after the wait of `attempt`, if one is left and no turn runs then.
    const again = (): void => {
      const wait = SETTLE_RETRY_MS[attempt];
      if (wait === undefined) return;
      setTimeout(() => {
        if (!live.current.isStreaming) void settle(responseId, attempt + 1);
      }, wait);
    };
    try {
      const target = targetOf(responseId, record.projectId);
      const items = await listSteering(target);
      const store = useChatSteeringStore.getState();
      store.apply(responseId, items);
      const own = new Set(record.ids);
      const leftovers = leftoverSteering(items, own, false);
      // Another conversation is on screen now: its A17 queue is not this one. Keep the record for the reopen.
      if (live.current.conversationKey !== record.conversationKey) return;
      const handing = record.handoffKey !== undefined && record.handoffQuestionId !== undefined;
      const offered = leftovers.length > 0 && !handing && isHeld(responseId, record.held);
      if (offered) {
        const offer: HeldOffer = { responseMessageId: responseId, conversationKey: record.conversationKey, text: joinSteeringTexts(leftovers) };
        setOffers((current) => ({ ...current, [responseId]: offer }));
      } else if (leftovers.length > 0 || handing) {
        const handoff: SteeringHandoff = handing
          ? { key: record.handoffKey as string, questionId: record.handoffQuestionId as string }
          : { key: newSteeringKey(), questionId: crypto.randomUUID() };
        store.setHandoff(responseId, handoff);
        const handed = await handOffSteering(target, handoff.key, handoff.questionId);
        const text = joinSteeringTexts(handed);
        if (text !== '') deliver(record.conversationKey, text, handoff.questionId);
        // The answer is used: the next hand-off of this answer needs new keys.
        store.setHandoff(responseId, undefined);
        store.forget(responseId, leftovers.map((item) => item.id));
        store.apply(responseId, handed);
      }
      const open = items.filter((item) => own.has(item.id) && item.state === STEERING_STATE.Pending);
      // An offered text keeps its id: the offer (also after a reload) needs the record.
      const kept = new Set([...open, ...(offered ? leftovers : [])].map((item) => item.id));
      store.dropOwn(responseId, record.ids.filter((id) => !kept.has(id)));
      if (open.length > 0) again();
    } catch {
      // Handled: the record (and its hand-off keys) stays. A later settle sends the same keys and gets the same rows.
      again();
    } finally {
      settling.current.delete(responseId);
    }
  }, [deliver]);

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
    deliver(conversationKey, question);
    // The last try was refused too: the countdown must not hide that the text now waits for the turn end.
    if (result.code === STEERING_ERROR.rateLimited) clearCountdown();
    if (result.code !== STEERING_ERROR.notSupported && result.code !== STEERING_ERROR.turnNotRunning) setNotice(failureNotice(result.code));
  }, [settle, post, deliver, clearCountdown]);

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

  // A carry-over can give this tab a message of an answer whose turn already ended.
  const carried = useChatSteeringStore((state) => state.carried);
  // When no turn of this chat runs: settle every answer that this tab steered here.
  useEffect(() => {
    const texts = stranded.current.get(conversationKey);
    if (texts !== undefined) {
      stranded.current.delete(conversationKey);
      for (const item of texts) deliver(conversationKey, item.text, item.questionId);
    }
    if (isStreaming) return;
    for (const [responseId, record] of Object.entries(useChatSteeringStore.getState().own)) {
      if (record.conversationKey === conversationKey) void settle(responseId, 0);
    }
  }, [conversationKey, isStreaming, settle, deliver, carried]);

  const dismissNotice = useCallback(() => {
    setNotice(null);
    clearCountdown();
  }, [clearCountdown]);
  const onReattached = useCallback(() => {
    const target = live.current.streamTarget();
    if (target) void refresh({ projectId: target.projectId, responseMessageId: target.messageGroupUuid });
  }, [refresh]);

  const onStop = useCallback(() => {
    const target = live.current.streamTarget();
    if (target === null) return;
    stopped.current.add(target.messageGroupUuid);
    useChatSteeringStore.getState().hold(target.messageGroupUuid);
  }, []);

  // An offer goes when its record goes (a carry-over moved the messages, or the user dismissed it).
  const ownRecords = useChatSteeringStore((state) => state.own);
  const held = Object.values(offers).find((item) => item.conversationKey === conversationKey && ownRecords[item.responseMessageId] !== undefined);
  const offerId = held?.responseMessageId;
  const offerText = held?.text;
  const offer = useMemo(() => (offerId === undefined || offerText === undefined ? null : { responseMessageId: offerId, text: offerText }), [offerId, offerText]);
  const closeOffer = useCallback((responseId: string) => {
    setOffers((current) => Object.fromEntries(Object.entries(current).filter(([key]) => key !== responseId)));
  }, []);
  const acceptOffer = useCallback(() => {
    if (offerId === undefined) return;
    closeOffer(offerId);
    // With the hand-off keys in the record, the end-of-turn rule hands the texts off and sends them.
    useChatSteeringStore.getState().setHandoff(offerId, { key: newSteeringKey(), questionId: crypto.randomUUID() });
    void settle(offerId, 0);
  }, [offerId, closeOffer, settle]);
  const dismissOffer = useCallback(() => {
    if (offerId === undefined) return;
    closeOffer(offerId);
    const record = useChatSteeringStore.getState().own[offerId];
    if (record !== undefined) useChatSteeringStore.getState().dropOwn(offerId, record.ids);
  }, [offerId, closeOffer]);

  return { onSend, edit, remove, onReattached, notice: countdownText ?? notice, dismissNotice, onStop, offer, acceptOffer, dismissOffer };
}
