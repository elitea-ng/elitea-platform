/**
 * The avatar state machine (ADR-0050 decision 6, "Client rules", and 7e).
 *
 * The server derives one `agent_state` frame per change of a participant's
 * state (client contract 1.38). This module folds those frames into the
 * state each avatar shows, ends the one-shot `success` / `error` slots after
 * at most 3 s, and picks the ONE avatar of a message list that may animate.
 * It is pure: the clients pass the clock in and schedule their own re-render
 * at `nextChangeAt`.
 */
import { AGENT_STATE, FRAME_TYPE, type AgentState } from '../stream/frameTypes';

/** How long a `success` or `error` slot shows before the avatar settles to `idle`. */
export const ONE_SHOT_MS = 3_000;

const STATES: ReadonlySet<string> = new Set(Object.values(AGENT_STATE));

/** A wire value as an agent state; `undefined` for an absent or unknown one. */
export function agentStateOf(value: unknown): AgentState | undefined {
  return typeof value === 'string' && STATES.has(value) ? (value as AgentState) : undefined;
}

/** One participant's last state and when it began (the client's clock, ms). */
export interface ParticipantAvatarState {
  readonly state: AgentState;
  readonly since: number;
}

/** The avatar state of every participant seen on a stream, keyed by participant id. */
export type AvatarStates = Readonly<Record<number, ParticipantAvatarState>>;

/** The parts of an `agent_state` frame the reducer reads. */
export interface AgentStateFrameFields {
  readonly type?: unknown;
  readonly participant_id?: unknown;
  readonly state?: unknown;
}

/**
 * Applies one stream frame. Any frame that is not a well-formed
 * `agent_state` returns the same object, so a caller can compare by
 * reference.
 */
export function applyAgentStateFrame(states: AvatarStates, frame: AgentStateFrameFields, now: number): AvatarStates {
  if (frame.type !== FRAME_TYPE.AgentState) return states;
  const id = frame.participant_id;
  const state = agentStateOf(frame.state);
  if (typeof id !== 'number' || !Number.isSafeInteger(id) || id <= 0 || state === undefined) return states;
  if (states[id]?.state === state) return states;
  return { ...states, [id]: { state, since: now } };
}

function isOneShot(state: AgentState): boolean {
  return state === AGENT_STATE.Success || state === AGENT_STATE.Error;
}

/**
 * The state an avatar shows now: a `success` or `error` older than
 * `ONE_SHOT_MS` reads as `idle`, and an unknown participant is `idle`.
 */
export function shownState(entry: ParticipantAvatarState | undefined, now: number): AgentState {
  if (entry === undefined) return AGENT_STATE.Idle;
  if (isOneShot(entry.state) && now - entry.since >= ONE_SHOT_MS) return AGENT_STATE.Idle;
  return entry.state;
}

/** When the shown state of any avatar changes on its own (a one-shot ending); `undefined` when none will. */
export function nextChangeAt(states: AvatarStates, now: number): number | undefined {
  let next: number | undefined;
  for (const entry of Object.values(states)) {
    if (!isOneShot(entry.state)) continue;
    const end = entry.since + ONE_SHOT_MS;
    if (end > now && (next === undefined || end < next)) next = end;
  }
  return next;
}

/** Whether a shown state has a motion of its own. `idle` breathes only on the presence row, never in a list. */
export function isAnimatedState(state: AgentState): boolean {
  return state !== AGENT_STATE.Idle;
}

/**
 * The one participant whose avatar animates in a message list: the one whose
 * shown state changed last among those with a motion. Every other avatar
 * shows its still. `undefined` when none animates.
 */
export function activeAnimatedParticipant(states: AvatarStates, now: number): number | undefined {
  let active: number | undefined;
  let since = -Infinity;
  for (const [key, entry] of Object.entries(states)) {
    if (!isAnimatedState(shownState(entry, now))) continue;
    if (entry.since > since) {
      since = entry.since;
      active = Number(key);
    }
  }
  return active;
}

/** The person's motion preferences. */
/**
 * The states a run leaves when its stream ends: every participant still in a
 * looping state takes `final` (`success` or `error` from the turn's last
 * frame, `idle` for a cancel or a lost stream), so no avatar loops after its
 * run. `waiting` stays, because the answer is paused on a person, and a
 * one-shot keeps its slot. The same object when nothing changes.
 */
export function settleAvatarStates(states: AvatarStates, final: AgentState, now: number): AvatarStates {
  let next: Record<number, ParticipantAvatarState> | undefined;
  for (const [key, entry] of Object.entries(states)) {
    if (isOneShot(entry.state) || entry.state === AGENT_STATE.Waiting || entry.state === AGENT_STATE.Idle) continue;
    next ??= { ...states };
    next[Number(key)] = { state: final, since: now };
  }
  return next ?? states;
}

/** The newest state change, the clock a pure render may read (`now` only moves on with a timer). */
export function newestSince(states: AvatarStates): number {
  return Object.values(states).reduce((max, entry) => Math.max(max, entry.since), 0);
}

export interface MotionPreferences {
  /** `prefers-reduced-motion` on web and desktop, the system setting on mobile. */
  readonly reducedMotion: boolean;
  /** Mobile data-saver (a constrained or metered network). Always false on web and desktop. */
  readonly dataSaver?: boolean | undefined;
}

/** Whether avatars may animate at all: reduced motion and data-saver show the still only. */
export function motionAllowed(preferences: MotionPreferences): boolean {
  return !preferences.reducedMotion && preferences.dataSaver !== true;
}

/** One avatar in a message list. */
export interface ListAvatar {
  /** The answering participant; `undefined` for a row with no stated author. */
  readonly participantId: number | undefined;
  /**
   * Whether this is the participant's newest message in the list. The state
   * is the participant's, not a message's: only its newest message shows it.
   */
  readonly newest: boolean;
}

/** What an avatar shows: `still` (the image alone) or a state's motion. */
export type AvatarMotion = AgentState | 'still';

/**
 * The motion an avatar plays, or `still`. One call answers it for a list
 * item: the item, the list's states, the clock and the preferences.
 */
export function avatarMotion(avatar: ListAvatar, states: AvatarStates, now: number, preferences: MotionPreferences): AvatarMotion {
  const { participantId } = avatar;
  if (participantId === undefined || !avatar.newest || !motionAllowed(preferences)) return 'still';
  if (activeAnimatedParticipant(states, now) !== participantId) return 'still';
  return shownState(states[participantId], now);
}
