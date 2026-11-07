/**
 * The avatar state of each answering participant (ADR-0050 decision 6),
 * per conversation, from the `agent_state` frames of its stream (client
 * contract 1.38). The fold is `@elitea/client-core/conversation`'s, so the
 * web, the desktop and mobile move an avatar on the same frames.
 *
 * Keyed by conversation: participant ids are row ids of one project's
 * schema, so two projects can share one.
 */
import { useEffect, useState } from 'react';

import useMediaQuery from '@mui/material/useMediaQuery';
import { create, type StoreApi, type UseBoundStore } from 'zustand';

import {
  applyAgentStateFrame,
  avatarMotion,
  newestSince,
  nextChangeAt,
  settleAvatarStates,
  type AgentStateFrameFields,
  type AvatarMotion,
  type AvatarStates,
  type ListAvatar,
} from '@elitea/client-core/conversation';
import type { AgentState } from '@elitea/client-core/stream';

import { SocketMessageType, type ChatStreamFrame } from '../lib/chatStreamFrame';
import { terminalAgentState } from '../lib/chatStreamTurnEnd';

const EMPTY: AvatarStates = {};

interface AgentAvatarState {
  readonly byConversation: Readonly<Record<string, AvatarStates>>;
  readonly apply: (conversationUuid: string, frame: AgentStateFrameFields, now: number) => void;
  readonly settle: (conversationUuid: string, final: AgentState, now: number) => void;
}

type AgentAvatarStore = UseBoundStore<StoreApi<AgentAvatarState>>;

/** @public A fresh store; the app reads the lazily built one below. */
export function createAgentAvatarStore(): AgentAvatarStore {
  return create<AgentAvatarState>()((set) => ({
    byConversation: {},
    apply: (conversationUuid, frame, now) =>
      set((state) => {
        const before = state.byConversation[conversationUuid] ?? EMPTY;
        const after = applyAgentStateFrame(before, frame, now);
        return after === before ? state : { byConversation: { ...state.byConversation, [conversationUuid]: after } };
      }),
    settle: (conversationUuid, final, now) =>
      set((state) => {
        const before = state.byConversation[conversationUuid];
        if (before === undefined) return state;
        const after = settleAvatarStates(before, final, now);
        return after === before ? state : { byConversation: { ...state.byConversation, [conversationUuid]: after } };
      }),
  }));
}

let instance: AgentAvatarStore | undefined;

function resolveStore(): AgentAvatarStore {
  instance ??= createAgentAvatarStore();
  return instance;
}

function useAgentAvatarStoreHook<T>(selector: (state: AgentAvatarState) => T): T {
  return resolveStore()(selector);
}

/** @public The lazily built singleton, with the hook + `getState` surface of the other stores here. */
export const useAgentAvatarStore = Object.assign(useAgentAvatarStoreHook, {
  getState: (): AgentAvatarState => resolveStore().getState(),
  /** Tests only: drop the singleton. */
  resetForTests: (): void => {
    instance = undefined;
  },
});

/**
 * Records one stream frame. Returns whether it was an `agent_state`, which
 * the caller then keeps out of the transcript; any other frame is a no-op.
 */
export function applyAgentAvatarFrame(conversationUuid: string | undefined, frame: AgentStateFrameFields): boolean {
  if (frame.type !== SocketMessageType.AgentState) return false;
  if (conversationUuid !== undefined && conversationUuid !== '') useAgentAvatarStore.getState().apply(conversationUuid, frame, Date.now());
  return true;
}

/**
 * Ends a run's avatar states when its stream ends. The stream closes on the
 * turn's last frame, before Main's derived `agent_state` after it, so the
 * last state is read off that frame (`terminalAgentState`); with none (a
 * cancel, a lost stream) the avatars go idle. No avatar loops after its run.
 */
export function settleAgentAvatars(conversationUuid: string | undefined, terminalFrame?: ChatStreamFrame): void {
  if (conversationUuid === undefined || conversationUuid === '') return;
  const final: AgentState = terminalFrame === undefined ? 'idle' : terminalAgentState(terminalFrame);
  useAgentAvatarStore.getState().settle(conversationUuid, final, Date.now());
}

const REDUCED_MOTION = '(prefers-reduced-motion: reduce)';

/**
 * The motion of each avatar of one conversation's list. Re-renders when a
 * state arrives and when a `success` / `error` slot ends on its own.
 */
export function useAgentAvatarMotion(conversationUuid: string | undefined): (avatar: ListAvatar) => AvatarMotion {
  const states = useAgentAvatarStore((state) => (conversationUuid === undefined ? EMPTY : (state.byConversation[conversationUuid] ?? EMPTY)));
  const reducedMotion = useMediaQuery(REDUCED_MOTION, { noSsr: true });
  // The clock is the newest state change (each stamps its own `since`), so a
  // render reads no wall clock; the timer moves it on when a one-shot ends.
  const latest = newestSince(states);
  const [tick, setTick] = useState<{ readonly after: number; readonly now: number }>({ after: 0, now: 0 });
  const now = tick.after === latest ? Math.max(tick.now, latest) : latest;
  useEffect(() => {
    const at = nextChangeAt(states, now);
    if (at === undefined) return undefined;
    const timer = setTimeout(() => { setTick({ after: latest, now: at }); }, Math.max(0, at - now));
    return () => { clearTimeout(timer); };
  }, [states, now, latest]);
  return (avatar) => avatarMotion(avatar, states, now, { reducedMotion });
}
