import type { ChatMessage } from '@/features/chat/messageModel';

/**
 * Which persisted messages can be a fork point (client contract 1.11, "Fork from here"),
 * decided per message. The server answers 409 `fork_point_unsettled` when the copy up to the
 * fork point would hold a reply still being written or waiting for the user, so a message is
 * NOT forkable when it belongs to an exchange in flight:
 *
 * - a persisted message still streaming (a turn another device runs), with its question;
 * - the newest answer while it waits for the user (a pause: approval, sign-in, …), or while a
 *   continuation resumes it, with its question;
 * - the answer a live regeneration rewrites, with its question;
 * - the live turn's question once it is persisted (and whatever follows it).
 *
 * Everything before the earliest such exchange is settled and forkable (when persisted), also
 * while a turn streams: a live question not in the transcript yet holds nothing back.
 */

export interface ForkPointState {
  /** A live turn of this screen that has not settled yet. */
  liveActive: boolean;
  /** The live turn continues the newest answer (no question of its own). */
  liveContinuation: boolean;
  /** The live turn's question text (to find it once persisted). */
  liveUserText: string | null;
  /** The persisted answer a live regeneration rewrites, by uuid. */
  regeneratingUuid: string | null;
  /** The newest answer waits for the user (approval, sign-in, …). */
  paused: boolean;
}

/** The index of the question an exchange at `at` starts with (the nearest user message at or before it). */
function exchangeStart(messages: readonly Pick<ChatMessage, 'role'>[], at: number): number {
  for (let i = at; i >= 0; i--) if (messages[i].role === 'user') return i;
  return at;
}

/** The first index whose message is part of an exchange in flight (`messages.length` when none is). */
export function unsettledFrom(messages: readonly Pick<ChatMessage, 'role' | 'text' | 'uuid' | 'isStreaming'>[], state: ForkPointState): number {
  let cut = messages.length;
  const last = messages.length - 1;
  const cutAt = (at: number) => {
    if (at >= 0) cut = Math.min(cut, exchangeStart(messages, at));
  };
  messages.forEach((message, i) => {
    if (message.isStreaming) cutAt(i);
  });
  if (state.paused) cutAt(last);
  if (state.liveActive) {
    if (state.liveContinuation) cutAt(last);
    else if (state.regeneratingUuid !== null) cutAt(messages.findIndex((m) => m.uuid === state.regeneratingUuid));
    else {
      let lastQuestion = -1;
      for (let i = last; i >= 0; i--) {
        if (messages[i].role === 'user') {
          lastQuestion = i;
          break;
        }
      }
      // Persisted already (the thread shows no separate live question): its exchange is in flight.
      if (lastQuestion !== -1 && state.liveUserText !== null && messages[lastQuestion].text === state.liveUserText) cutAt(lastQuestion);
    }
  }
  return cut;
}

/** The keys of the messages that can be a fork point now. */
export function forkableKeys(messages: readonly Pick<ChatMessage, 'key' | 'role' | 'text' | 'uuid' | 'isStreaming'>[], state: ForkPointState): Set<string> {
  const cut = unsettledFrom(messages, state);
  const keys = new Set<string>();
  for (let i = 0; i < cut; i++) if (messages[i].uuid !== null && !messages[i].isStreaming) keys.add(messages[i].key);
  return keys;
}
