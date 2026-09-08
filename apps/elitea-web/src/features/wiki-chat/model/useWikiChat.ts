/**
 * Drive one conversation: send a question, poll its invocation, feed the frames
 * to the reducer.
 *
 * HEADLESS. There is no JSX in this slice at all. The drawer that renders it
 * lives at `widgets/deepwiki`, because reusing the chat message components
 * means importing `features/chat-messages`, and `no-sideways-features` allows
 * that only from a widget. Splitting here is not a workaround for the rule —
 * it is what lets the conversation be tested without a drawer.
 *
 * EXACTLY ONE POLLER, for the reason the generation hook states: `custom_events`
 * is read-once, so two pollers each consume events the other never sees and the
 * visible symptom is a thinking log missing half its cards.
 *
 * EVERY DOOR TO THE WORLD IS INJECTED — invoke, poll, the clock, the id
 * generator and the storage. Not for purity: it is what makes "the request
 * failed before it started" a test instead of a network fixture.
 */
import { useCallback, useRef, useState } from 'react';

import { useInvocationPoll } from '@/entities/provider-run';
import { framesFromChatPoll, type ChatInvocationPoll } from '../lib/framesFromChatPoll';
import { reduceChatFrames } from './reducer';
import { capabilityOnOpen, chatHistory, failTurn, openTurn, rewindToLastQuestion, toolNameFor } from './turn';
import { initialChatState, type ChatCapability, type ChatMessage, type ChatState } from './types';


/** What the caller must supply to start one request. */
export interface ChatInvokeInput {
  readonly toolName: string;
  readonly question: string;
  readonly history: readonly { role: 'user' | 'assistant'; content: string }[];
  readonly capability: ChatCapability;
  readonly streamId: string;
  readonly messageId: string;
}

export interface WikiChatOptions {
  /** Start the invocation. Resolves to its id. */
  readonly invoke: (input: ChatInvokeInput) => Promise<string>;
  /** Fetch one poll of a running invocation. */
  readonly poll: (invocationId: string) => Promise<ChatInvocationPoll | undefined>;
  /** Read and write the last capability. */
  readonly storage: ChatStorage;
  /**
   * The transcript to open with, before anything is loaded.
   *
   * It exists for the LEGACY local conversation: a browser that still holds
   * one from before the server kept history shows it rather than an empty
   * drawer, and `hydrate` replaces it the moment the server's own transcript
   * arrives.
   */
  readonly initialMessages?: readonly ChatMessage[];
  /** A fresh identifier. Injected so a test can make ids predictable. */
  readonly newId: () => string;
  readonly intervalMs?: number;
}

/**
 * The one thing that outlives a mount: which agent answered last.
 *
 * THE CONVERSATION IS NO LONGER HERE. It used to be — the transcript was kept
 * in `localStorage` beside this, which is why it was gone on another device,
 * in another browser and on a cleared profile. elitea-main now writes both
 * turns of every wiki chat into the tenant chat tables, and the drawer reads
 * them back through `hydrate`.
 *
 * The capability stays, and stays local, because it is a PREFERENCE and not a
 * record: which way the toggle was left is a property of this browser, every
 * stored answer already carries the capability it was produced with, and a
 * server round trip to restore a two-position switch would be worse in every
 * way.
 *
 * Deliberately an interface rather than a direct `localStorage` reach: the raw
 * global escapes the namespaced sweep that logout runs, which is how keys
 * survive a sign-out (issue #22). The widget passes a namespaced store.
 */
export interface ChatStorage {
  readonly loadCapability: () => ChatCapability | null;
  readonly saveCapability: (capability: ChatCapability) => void;
}

export interface WikiChatController {
  readonly state: ChatState;
  readonly send: (question: string) => void;
  readonly regenerate: () => void;
  readonly clear: () => void;
  /**
   * Replace the transcript with one that was LOADED rather than typed.
   *
   * REFUSED WHILE A TURN IS RUNNING. The server's copy of a conversation is
   * behind the one on screen for as long as an answer is in flight — the
   * answer is written when the terminal poll is drained — so hydrating
   * mid-turn would replace a live answer with a transcript that does not
   * have it yet.
   */
  readonly hydrate: (messages: readonly ChatMessage[]) => void;
  readonly setMode: (mode: ChatCapability) => void;
  /** Restore the capability the last answer was produced with. */
  readonly restoreCapability: () => void;
}

export function useWikiChat(options: WikiChatOptions): WikiChatController {
  const optionsRef = useRef(options);
  optionsRef.current = options;

  const [state, setState] = useState<ChatState>(() => ({
    ...initialChatState,
    messages: options.initialMessages ?? [],
    mode: options.storage.loadCapability() ?? 'ask',
  }));

  // The poller reads the live state through a ref. Without it the effect would
  // depend on `state` and restart on every frame — draining read-once events
  // into a poller that is about to be replaced.
  const stateRef = useRef(state);
  stateRef.current = state;

  const [invocationId, setInvocationId] = useState<string | null>(null);

  // NOTHING PERSISTS THE TRANSCRIPT HERE any more. A `useEffect` used to
  // write every message change to `localStorage`; the server now writes both
  // turns of a wiki chat as they happen (elitea-main's DeepWiki facade
  // observes the invoke and tees the terminal poll), so a copy kept here
  // would be a second record to disagree with the first.

  const start = useCallback((question: string, from: readonly ChatMessage[]) => {
    const trimmed = question.trim();
    const current = stateRef.current;
    if (trimmed === '' || current.isLoading) return;

    const { newId, invoke, storage } = optionsRef.current;
    const capability = current.mode;
    const blockId = newId();
    const streamId = newId();
    const messageId = newId();
    // The history is taken from the messages the turn STARTS from, not from
    // the state after the question is appended: the model must not be handed
    // the question it is being asked as prior context.
    const history = chatHistory(from);

    const opened = openTurn(
      { ...current, messages: from },
      { question: trimmed, capability, blockId, streamId, messageId },
    );
    setState(opened);

    void invoke({ toolName: toolNameFor(capability), question: trimmed, history, capability, streamId, messageId })
      .then((id) => {
        setInvocationId(id);
      })
      .catch((error: unknown) => {
        // The request never started, so no frame will ever close the block.
        // failTurn removes it; leaving it would spin for ever.
        const message = error instanceof Error ? error.message : 'The request failed.';
        setState((previous) => failTurn(previous, blockId, message));
        storage.saveCapability(capability);
      });
  }, []);

  const send = useCallback(
    (question: string) => {
      start(question, stateRef.current.messages);
    },
    [start],
  );

  const regenerate = useCallback(() => {
    const rewound = rewindToLastQuestion(stateRef.current.messages);
    if (!rewound) return;
    // Everything after the question goes, so the model is not shown the answer
    // it is being asked to replace.
    start(rewound.question, rewound.messages);
  }, [start]);

  const clear = useCallback(() => {
    setState((previous) => ({ ...initialChatState, mode: previous.mode }));
    setInvocationId(null);
  }, []);

  const hydrate = useCallback((messages: readonly ChatMessage[]) => {
    setState((previous) => (previous.isLoading ? previous : { ...previous, messages }));
  }, []);

  const setMode = useCallback((mode: ChatCapability) => {
    setState((previous) => ({ ...previous, mode }));
  }, []);

  const restoreCapability = useCallback(() => {
    const { storage } = optionsRef.current;
    const next = capabilityOnOpen(stateRef.current.messages, storage.loadCapability());
    if (next) setState((previous) => (previous.mode === next ? previous : { ...previous, mode: next }));
  }, []);

  // The loop is the run entity's: one request at a time, stopped by the
  // first terminal poll. What this hook keeps is how a poll becomes chat
  // frames, and what the reducer's effects do.
  useInvocationPoll(invocationId, {
    poll: (id) => optionsRef.current.poll(id),
    onPoll: (poll, id) => {
      const frames = framesFromChatPoll(poll, { streamId: stateRef.current.streamId ?? id });
      if (frames.length === 0) return;
      setState((previous) => {
        const { state: next, effects } = reduceChatFrames(previous, frames);
        for (const effect of effects) {
          if (effect.kind === 'persistCapability') {
            optionsRef.current.storage.saveCapability(effect.capability);
          }
        }
        return next;
      });
    },
    intervalMs: options.intervalMs,
  });

  return { state, send, regenerate, clear, hydrate, setMode, restoreCapability };
}
