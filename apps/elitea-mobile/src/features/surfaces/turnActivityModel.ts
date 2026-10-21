import type { LiveTurn } from '@/features/chat/turnController';
import type { ClientPolicy, NotificationPreview } from '@/security/policy';

/**
 * What the Live Activity of a running answer shows (lock screen, Dynamic
 * Island). LOCAL only: the app starts and updates it while it runs, from the
 * turn's own stream; no push token is ever requested.
 *
 * Generic text by default. The conversation's title appears only when the
 * workspace lets notifications show titles (`notification_preview: 'title'`)
 * AND allows titles on system surfaces (`allow_system_surfaces`): the
 * activity sits on the lock screen next to the widgets, which are
 * counts-only unless that key is on (client contract 1.3; both off by
 * default). Message content, tool names and the workspace host never appear.
 */

export type TurnActivityPhase = 'working' | 'tool' | 'writing' | 'waiting' | 'done' | 'failed' | 'stopped';

export interface TurnActivityProps {
  phase: TurnActivityPhase;
  /** The headline: generic, or the conversation title when previews allow it. */
  title: string;
  detail: string;
  /** The app was suspended: what it shows may no longer be true. */
  approximate: boolean;
  /** When the turn started (epoch ms), for "for N min". */
  startedAt: number;
}

/** The preview the Live Activity may use under `policy`: 'title' only when both keys allow it. */
export function activityTitlePreview(policy: Pick<ClientPolicy, 'notification_preview' | 'allow_system_surfaces'>): NotificationPreview {
  return policy.allow_system_surfaces && policy.notification_preview === 'title' ? 'title' : 'none';
}

/** The generic headline while the answer runs. */
export const GENERIC_TITLE = 'Your agent is answering';
/** The generic headline once it stopped running (it stays on the lock screen a while). */
const FINAL_TITLE: Readonly<Record<'waiting' | 'done' | 'failed' | 'stopped', string>> = {
  waiting: 'Your agent needs you',
  done: 'Your agent answered',
  failed: 'Your agent stopped',
  stopped: 'Your agent stopped',
};

/** The headline when no conversation title may be shown. */
export function genericTitle(phase: TurnActivityPhase): string {
  return phase === 'waiting' || phase === 'done' || phase === 'failed' || phase === 'stopped' ? FINAL_TITLE[phase] : GENERIC_TITLE;
}
const MAX_TITLE = 48;

export const PHASE_TEXT: Readonly<Record<TurnActivityPhase, string>> = {
  working: 'Working…',
  tool: 'Using a tool…',
  writing: 'Writing the answer…',
  waiting: 'Waiting for your decision',
  done: 'Answer ready',
  failed: "Didn't finish",
  stopped: 'Stopped',
};

export const APPROXIMATE_TEXT = 'Open the app for the latest';

/** Where a live turn is, in Live Activity terms; null when it is not worth one (queued). */
export function activityPhase(live: Pick<LiveTurn, 'turn' | 'queued'>): TurnActivityPhase | null {
  if (live.queued) return null;
  const turn = live.turn;
  switch (turn.phase) {
    case 'sending':
      return 'working';
    case 'streaming': {
      const runningTool = turn.tools.some((t) => t.status === 'running');
      if (runningTool) return 'tool';
      return turn.content !== '' ? 'writing' : 'working';
    }
    case 'paused':
      return turn.pause && turn.pause.kind !== 'output_limit' ? 'waiting' : 'done';
    case 'done':
      return turn.stopped ? 'stopped' : 'done';
    case 'failed':
      return turn.stopped ? 'stopped' : 'failed';
  }
}

export function isFinalPhase(phase: TurnActivityPhase): boolean {
  return phase === 'waiting' || phase === 'done' || phase === 'failed' || phase === 'stopped';
}

function cleanTitle(name: string | null): string | null {
  if (name === null) return null;
  const one = name.replace(/\s+/g, ' ').trim();
  if (one === '') return null;
  return one.length > MAX_TITLE ? `${one.slice(0, MAX_TITLE - 1)}…` : one;
}

export function activityProps(input: {
  phase: TurnActivityPhase;
  preview: NotificationPreview;
  conversationTitle: string | null;
  approximate: boolean;
  startedAt: number;
}): TurnActivityProps {
  const title = (input.preview === 'title' ? cleanTitle(input.conversationTitle) : null) ?? genericTitle(input.phase);
  const approximate = input.approximate && !isFinalPhase(input.phase);
  return {
    phase: input.phase,
    title,
    detail: approximate ? APPROXIMATE_TEXT : PHASE_TEXT[input.phase],
    approximate,
    startedAt: input.startedAt,
  };
}
