/**
 * A new thread's first message is on screen at once (#3127). Before the first turn can start, the thread is prepared:
 * its conversation is created (with the agents chosen for it), and with "Work in › New worktree" the worktree is made
 * first and the message moves to the worktree's own thread. All of that takes a while, so the message shows in the
 * transcript as soon as it is sent, marked "Preparing workspace…", and the composer is cleared: the person can write
 * the next one while it is prepared (it goes once the turn runs, as steering).
 *
 * The send itself is unchanged and runs behind the message. Its keys stay what they were (#3077): the composer's
 * synchronous gate, then one first message at a time here, and the client ids of the send (`useSendPrompt`'s submit id,
 * Main's `client_request_id`, `useWorkIn`'s thread id) kept until it lands, so repeated Enter or Retry make one thread,
 * one worktree and one turn. A send that fails leaves the message on screen, "Not sent", with Retry: never dropped.
 *
 * The worktree's thread shows the message its folder's thread handed it (`peekFirstMessage`) from its first render, and
 * sends it once it can (its agents read); the hand-off is dropped once it went (`takeFirstMessage`).
 */
import { useEffect, useRef, useState } from 'react';

import { peekFirstMessage, takeFirstMessage } from '@/features/workspace-worktrees';

import type { SelectionSeed } from './useAgentSelection';
import type { WorkspaceComposerProps } from './WorkspaceComposer';

type Send = WorkspaceComposerProps['onSend'];
export type FirstSendArgs = Parameters<Send>;

/** What a folder's new thread hands to its worktree's thread: the message, and the agents chosen for it (#3128). */
export interface HandedFirstMessage {
  readonly args: FirstSendArgs;
  readonly seed?: SelectionSeed | undefined;
}

/** The workspace's handed first message, still held (shown before it is sent). */
export function peekHanded(workspaceId: string): HandedFirstMessage | undefined {
  return peekFirstMessage<HandedFirstMessage>(workspaceId);
}

/** `waiting`: handed to this thread, sent once it can; `preparing`: being sent; `failed`: not sent, Retry offered. */
export type FirstMessageState = 'waiting' | 'preparing' | 'failed';

export interface FirstMessage {
  readonly args: FirstSendArgs;
  readonly state: FirstMessageState;
}

/** The session's first message slot: shown by the transcript (with its Retry), driven by the composer (`useFirstSend`). */
export interface FirstMessageSlot {
  readonly message: FirstMessage | null;
  readonly set: (message: FirstMessage | null) => void;
  /** Sends a failed first message again, through the composer's send. */
  readonly retry: () => void;
  /** The composer's retry, bound on each render. */
  readonly bindRetry: (retry: () => void) => void;
}

/** The slot, holding the message handed to this workspace's thread from its first render. */
export function useFirstMessageSlot(handed: HandedFirstMessage | undefined): FirstMessageSlot {
  const [message, set] = useState<FirstMessage | null>(() => (handed === undefined ? null : { args: handed.args, state: 'waiting' }));
  const retryRef = useRef<() => void>(() => undefined);
  const [bound] = useState(() => ({
    retry: () => retryRef.current(),
    bindRetry: (retry: () => void) => {
      retryRef.current = retry;
    },
  }));
  return { message, set, ...bound };
}

export interface FirstSendInput {
  readonly workspaceId: string;
  readonly slot: FirstMessageSlot;
  /** Nothing was sent in this thread yet, and it runs here: its first message shows at once. */
  readonly fresh: boolean;
  /** The thread can take a message now (its agents read, no turn running). */
  readonly canSend: boolean;
  /** The send behind the message (the folder's, or the worktree hand-off). */
  readonly send: Send;
}

export interface FirstSend {
  /** The composer's send: for a fresh thread it resolves `true` at once (the input clears) and the send runs behind. */
  readonly send: Send;
  /** A first message is shown and not yet sent: no other message goes until it did. */
  readonly holding: boolean;
  /** Sends the failed first message again (the same ids). */
  readonly retry: () => void;
}

export function useFirstSend({ workspaceId, slot, fresh, canSend, send }: FirstSendInput): FirstSend {
  const { message, set } = slot;
  // Synchronous: the effect below and a click may both reach `run` before React re-renders.
  const running = useRef(false);
  const run = async (args: FirstSendArgs): Promise<void> => {
    if (running.current) return;
    running.current = true;
    set({ args, state: 'preparing' });
    let sent = false;
    try {
      sent = await send(...args);
    } catch {
      sent = false;
    } finally {
      running.current = false;
    }
    if (sent) takeFirstMessage(workspaceId);
    // Sent: the turn's own prompt stands in the transcript now (or the worktree's thread has the message).
    set(sent ? null : { args, state: 'failed' });
  };

  // A message handed to this (worktree) thread goes once the thread can take it.
  const waiting = message?.state === 'waiting' ? message.args : undefined;
  useEffect(() => {
    if (waiting !== undefined && canSend) void run(waiting);
  });

  const optimistic: Send = (...args) => {
    if (message !== null || running.current) return Promise.resolve(false);
    void run(args);
    return Promise.resolve(true);
  };

  const retry = (): void => {
    if (message?.state === 'failed') void run(message.args);
  };
  useEffect(() => slot.bindRetry(retry));

  return { send: fresh ? optimistic : send, holding: message !== null, retry };
}
