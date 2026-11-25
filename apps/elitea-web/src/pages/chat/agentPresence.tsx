/**
 * Live agent presence in a participants panel (ADR-0061 decision 4c, #3067), shared by the chat's panel and a Local
 * work thread's Participants tab:
 *
 * - `useAgentRowViews`: each agent's static state badge and label; only the thread's active agent animates, and
 *   none does under `prefers-reduced-motion`.
 * - `AgentStateAnnouncer`: ONE polite live region per thread that reads state changes out ("CI Doctor: Running
 *   cargo"), at most once per agent per second.
 * - `HandoffBaton`: when a turn ends with a hand-off (`activity.handoff_to`), a dot travels from the handing agent's
 *   row to the receiver's for `motion.handoff` on `motion.easing`; under reduced motion it is not drawn at all (the
 *   change is instant, and the side row says who handed to whom).
 *
 * Labels come from client-core `activityLabel` with these translated templates, so web, desktop and mobile word
 * them alike; they are built from sanitised names only, never from command text.
 */
import { useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode, type RefObject } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';
import useMediaQuery from '@mui/material/useMediaQuery';

import {
  HANDOFF_MS,
  MOTION_EASING,
  activeAnimatedParticipant,
  activityLabel,
  motionAllowed,
  newestSince,
  shownState,
  type ActivityLabels,
  type AvatarStates,
} from '@elitea/client-core/conversation';
import { AgentAvatar, useAgentAvatarStates } from '@/features/chat-messages';
import { t } from '@/shared/i18n';

import type { ParticipantRowView } from './ParticipantRowControls';

const REDUCED_MOTION = '(prefers-reduced-motion: reduce)';

/** At most one announcement per agent per this long. */
export const ANNOUNCE_INTERVAL_MS = 1_000;

/** The label templates in the person's language. */
function activityLabels(): ActivityLabels {
  return {
    idle: t('chat.presence.idle', 'Idle'),
    thinking: t('chat.presence.thinking', 'Thinking'),
    waking: t('chat.presence.waking', 'Resuming'),
    speaking: t('chat.presence.speaking', 'Writing'),
    working: t('chat.presence.working', 'Working'),
    running: t('chat.presence.running', 'Running {tool}'),
    waitingForYou: t('chat.presence.waitingForYou', 'Waiting for you'),
    waitingForAgent: t('chat.presence.waitingForAgent', 'Waiting for {name}'),
    anotherAgent: t('chat.presence.anotherAgent', 'another agent'),
    handedTo: t('chat.presence.handedTo', 'Handed to {name}'),
    done: t('chat.presence.done', 'Done'),
    error: t('chat.presence.error', 'Error'),
  };
}

/** One agent of the thread, as the rows need it. */
export interface PresenceAgent {
  readonly participantId: number;
  readonly name: string;
  readonly cloud: boolean;
}

/** The row view of every agent, from the thread's avatar states. */
export function agentRowViews(agents: readonly PresenceAgent[], states: AvatarStates, leadParticipantId: number, reducedMotion: boolean): ParticipantRowView[] {
  const now = newestSince(states);
  const labels = activityLabels();
  const nameOf = (id: number) => agents.find((agent) => agent.participantId === id)?.name;
  const active = motionAllowed({ reducedMotion }) ? activeAnimatedParticipant(states, now) : undefined;
  return agents.map((agent) => {
    const entry = states[agent.participantId];
    const state = shownState(entry, now);
    return {
      participantId: agent.participantId,
      name: agent.name,
      cloud: agent.cloud,
      isLead: agent.participantId === leadParticipantId,
      label: entry === undefined ? undefined : activityLabel(state, entry.activity, nameOf, labels),
      motion: active === agent.participantId ? state : 'still',
    };
  });
}

export function useAgentRowViews(agents: readonly PresenceAgent[], states: AvatarStates, leadParticipantId: number): ParticipantRowView[] {
  const reducedMotion = useMediaQuery(REDUCED_MOTION, { noSsr: true });
  return useMemo(() => agentRowViews(agents, states, leadParticipantId, reducedMotion), [agents, states, leadParticipantId, reducedMotion]);
}

/**
 * The thread's active agent and its label ("Mira · Running cargo") for a header, with its avatar animated (still under
 * reduced motion); nothing while no agent works or in a thread of one agent.
 */
export function ActiveAgentStatus({ conversationKey, agents }: { readonly conversationKey: string; readonly agents: readonly PresenceAgent[] }): ReactNode {
  const states = useAgentAvatarStates(conversationKey === '' ? undefined : conversationKey);
  const rows = useAgentRowViews(agents, states, 0);
  const now = newestSince(states);
  const active = agents.length < 2 ? undefined : activeAnimatedParticipant(states, now);
  const row = rows.find((candidate) => candidate.participantId === active);
  if (row?.label === undefined) return null;
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5, minWidth: 0 }} data-testid="active-agent-status" data-participant-id={row.participantId}>
      <AgentAvatar motion={row.motion} size="1rem" />
      <Typography variant="labelSmall" color="text.secondary" noWrap>{`${row.name} · ${row.label}`}</Typography>
    </Box>
  );
}

/** The politely announced line for the rows' labels; repeats and bursts inside `ANNOUNCE_INTERVAL_MS` say nothing new. */
export function AgentStateAnnouncer({ rows }: { readonly rows: readonly ParticipantRowView[] }): ReactNode {
  const [message, setMessage] = useState('');
  const last = useRef(new Map<number, { label: string; at: number }>());
  useEffect(() => {
    const now = Date.now();
    for (const row of rows) {
      if (row.label === undefined) continue;
      const before = last.current.get(row.participantId);
      if (before?.label === row.label || (before !== undefined && now - before.at < ANNOUNCE_INTERVAL_MS)) continue;
      last.current.set(row.participantId, { label: row.label, at: now });
      setMessage(`${row.name}: ${row.label}`);
    }
  }, [rows]);
  return (
    <Box
      component="output"
      aria-live="polite"
      data-testid="agent-state-announcer"
      sx={{ position: 'absolute', width: '1px', height: '1px', overflow: 'hidden', clip: 'rect(0 0 0 0)', whiteSpace: 'nowrap' }}
    >
      {message}
    </Box>
  );
}

/** The hand-off a state map shows: the handing agent and its first receiver, or `undefined`. */
export function handoffOf(states: AvatarStates): { from: number; to: number; since: number } | undefined {
  let found: { from: number; to: number; since: number } | undefined;
  for (const [key, entry] of Object.entries(states)) {
    const to = entry.activity?.handoff_to?.[0];
    if (entry.state !== 'success' || to === undefined) continue;
    if (found === undefined || entry.since > found.since) found = { from: Number(key), to, since: entry.since };
  }
  return found;
}

function rowRect(container: HTMLElement, participantId: number): DOMRect | undefined {
  const node = container.querySelector(`[data-testid="participant-lead"][data-participant-id="${String(participantId)}"]`);
  const rect = node?.getBoundingClientRect();
  if (rect === undefined || rect.height === 0) return undefined;
  const box = container.getBoundingClientRect();
  // Off-screen (scrolled out of the panel): no baton.
  return rect.bottom < box.top || rect.top > box.bottom ? undefined : rect;
}

/** The baton between two rows of `container`; nothing under reduced motion or while either row is off-screen. */
export function HandoffBaton({ container, states }: { readonly container: RefObject<HTMLElement | null>; readonly states: AvatarStates }): ReactNode {
  const reducedMotion = useMediaQuery(REDUCED_MOTION, { noSsr: true });
  const handoff = handoffOf(states);
  const [path, setPath] = useState<{ key: string; x: number; y: number; dy: number } | null>(null);
  const [moved, setMoved] = useState(false);
  const key = handoff === undefined ? '' : `${String(handoff.from)}>${String(handoff.to)}@${String(handoff.since)}`;
  const fromId = handoff?.from;
  const toId = handoff?.to;
  useLayoutEffect(() => {
    const node = container.current;
    if (reducedMotion || fromId === undefined || toId === undefined || node === null) return undefined;
    const from = rowRect(node, fromId);
    const to = rowRect(node, toId);
    if (from === undefined || to === undefined) return undefined;
    const box = node.getBoundingClientRect();
    setPath({ key, x: from.left - box.left, y: from.top - box.top + from.height / 2, dy: to.top - from.top });
    setMoved(false);
    const frame = requestAnimationFrame(() => setMoved(true));
    const done = setTimeout(() => setPath(null), HANDOFF_MS + 50);
    return () => { cancelAnimationFrame(frame); clearTimeout(done); };
    // `key` names the hand-off (its time too): a new one restarts the baton.
  }, [key, reducedMotion, container, fromId, toId]);
  if (reducedMotion || path === null || path.key !== key) return null;
  return (
    <Box
      aria-hidden
      data-testid="handoff-baton"
      sx={(theme: Theme) => ({
        position: 'absolute',
        left: path.x,
        top: path.y,
        width: '0.5rem',
        height: '0.5rem',
        borderRadius: theme.vars.shape.radiusLg,
        backgroundColor: theme.vars.palette.primary.main,
        pointerEvents: 'none',
        transform: `translateY(${moved ? path.dy : 0}px)`,
        transition: `transform ${String(HANDOFF_MS)}ms ${MOTION_EASING}`,
      })}
    />
  );
}
