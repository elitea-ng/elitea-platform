/**
 * The default motion pack (ADR-0050 decision 6, "Default pack"): every agent
 * gets state animations over its still, with no media. This is the one spec;
 * the web and the desktop render it with CSS, mobile with Reanimated.
 *
 * The spec names TOKEN ROLES, not colours: each client maps a role to its
 * ADR-0024 brand token, so a white label re-themes the pack. The timings are
 * the `motion.avatar` and `motion.handoff` tokens (ADR-0061 decision 4c), so a
 * brand re-times them in `tokens.json` with the rest of its motion.
 */
import { MOTION_TOKENS } from '../brand/motion.gen';
import { AGENT_STATE, type AgentState } from '../stream/frameTypes';

/** The brand token role a motion is drawn in. */
export const MOTION_TONE = {
  /** The brand accent: thinking, speaking, working, waking. */
  Accent: 'accent',
  /** The warning tone: waiting on a person. */
  Attention: 'attention',
  Success: 'success',
  Error: 'error',
} as const;

export type MotionTone = (typeof MOTION_TONE)[keyof typeof MOTION_TONE];

/** The shape a motion draws around or over the still. */
export const MOTION_KIND = {
  /** The still scales gently. */
  Breathe: 'breathe',
  /** A partial ring turns around the still. */
  Ring: 'ring',
  /** A full ring pulses outward. */
  Pulse: 'pulse',
  /** A dot orbits the still. */
  Orbit: 'orbit',
  /** A steady ring fades in and out. */
  Halo: 'halo',
  /** A badge (a check or a cross) pops in once. */
  Badge: 'badge',
} as const;

export type MotionKind = (typeof MOTION_KIND)[keyof typeof MOTION_KIND];

export interface MotionSpec {
  readonly kind: MotionKind;
  readonly tone: MotionTone | undefined;
  /** One cycle (or the one-shot's length), ms. */
  readonly durationMs: number;
  /** Whether the motion repeats; a one-shot plays once and holds its end. */
  readonly loop: boolean;
  /** The still's scale at the motion's peak (1 is none). */
  readonly peakScale: number;
  /** The badge glyph of a one-shot. */
  readonly badge?: 'check' | 'cross' | undefined;
}

/**
 * A duration token in milliseconds: `"1200ms"`, `"1.2s"` or a bare number of ms. A brand can re-time the avatar tokens,
 * and these are read at import: a value that is none of these gives `fallback` (the default timing), never a throw.
 */
export function tokenMs(value: unknown, fallback: number): number {
  const match = typeof value === 'string' ? /^(\d+(?:\.\d+)?)(ms|s)?$/.exec(value.trim()) : null;
  if (match === null) return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : fallback;
  const amount = Number(match[1]);
  return match[2] === 's' ? amount * 1000 : amount;
}

const AVATAR = MOTION_TOKENS.avatar;

/** One state's timing from `motion.avatar`. */
function timing(slot: { readonly duration: string; readonly loop: boolean }, defaultMs: number): Pick<MotionSpec, 'durationMs' | 'loop'> {
  return { durationMs: tokenMs(slot.duration, defaultMs), loop: slot.loop };
}

/** How long the hand-off baton travels between two avatars (`motion.handoff`, on `motion.easing`). */
export const HANDOFF_MS: number = tokenMs(MOTION_TOKENS.handoff, 400);

/** The easing of every avatar and hand-off motion (`motion.easing`). */
export const MOTION_EASING: string = MOTION_TOKENS.easing;

/** The motion of each state. `idle` breathes; the list never animates it (`isAnimatedState`). */
export const MOTION_SPEC: Readonly<Record<AgentState, MotionSpec>> = {
  [AGENT_STATE.Idle]: { kind: MOTION_KIND.Breathe, tone: undefined, ...timing(AVATAR.idle, 4000), peakScale: 1.03 },
  [AGENT_STATE.Thinking]: { kind: MOTION_KIND.Ring, tone: MOTION_TONE.Accent, ...timing(AVATAR.thinking, 1200), peakScale: 1 },
  [AGENT_STATE.Speaking]: { kind: MOTION_KIND.Pulse, tone: MOTION_TONE.Accent, ...timing(AVATAR.speaking, 900), peakScale: 1.06 },
  [AGENT_STATE.Working]: { kind: MOTION_KIND.Orbit, tone: MOTION_TONE.Accent, ...timing(AVATAR.working, 1600), peakScale: 1 },
  [AGENT_STATE.Waiting]: { kind: MOTION_KIND.Halo, tone: MOTION_TONE.Attention, ...timing(AVATAR.waiting, 2000), peakScale: 1 },
  [AGENT_STATE.Success]: { kind: MOTION_KIND.Badge, tone: MOTION_TONE.Success, ...timing(AVATAR.oneShot, 400), peakScale: 1, badge: 'check' },
  [AGENT_STATE.Error]: { kind: MOTION_KIND.Badge, tone: MOTION_TONE.Error, ...timing(AVATAR.oneShot, 400), peakScale: 1, badge: 'cross' },
  [AGENT_STATE.Waking]: { kind: MOTION_KIND.Pulse, tone: MOTION_TONE.Accent, ...timing(AVATAR.waking, 1400), peakScale: 1.1 },
};
