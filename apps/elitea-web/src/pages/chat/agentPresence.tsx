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
import useMediaQuery from '@mui/material/useMediaQuery';

import {
  avatarPackOf,
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
import { AgentAvatar, participantIconUrl } from '@/features/chat-messages';
import { t } from '@/shared/i18n';

import type { ParticipantRowView } from './ParticipantRowControls';

const REDUCED_MOTION = '(prefers-reduced-motion: reduce)';

/** The participant row id of a conversation participant, when it is a positive integer. */
function participantRowId(participant: unknown): number | undefined {
  const id = Number((participant as { id?: unknown } | null)?.id);
  return Number.isSafeInteger(id) && id > 0 ? id : undefined;
}

/** An agent participant's logo (`icon_meta`) and avatar pack, as a chat message reads them. */
function agentMediaOf(participant: unknown): { iconUrl: string | undefined; pack: unknown } {
  const persona = (participant as { persona?: { avatar_pack_v1?: unknown; avatar_pack?: unknown } } | null)?.persona;
  const pack = avatarPackOf(persona?.avatar_pack_v1 ?? persona?.avatar_pack);
  return { iconUrl: participantIconUrl(participant as Parameters<typeof participantIconUrl>[0]), pack };
}

/** The row avatar's edge: the participant row's avatar place (`ParticipantItem`'s icon box). */
const ROW_AVATAR_SIZE = '1.5rem';

/**
 * The participants panel's `renderAgentAvatar` slot (#3131): every agent row's avatar is the shared `AgentAvatar` at the
 * row's avatar place, animating with the agent's live state as in chat (only the active agent, never under reduced
 * motion; #3145). Its still is the agent's logo or avatar pack, else the default avatar (the brand mark), so an agent
 * without either animates too. The row's box lets the motion's ring draw outside it. The state's label stays on the
 * right, without a second avatar.
 */
export function agentAvatarRenderer(participants: readonly unknown[] | undefined, rows: readonly ParticipantRowView[]): (props: { participantId: number }) => ReactNode {
  return ({ participantId }) => {
    const participant = (participants ?? []).find((candidate) => participantRowId(candidate) === participantId);
    if (participant === undefined) return null;
    const { iconUrl, pack } = agentMediaOf(participant);
    const motion = rows.find((row) => row.participantId === participantId)?.motion ?? 'still';
    return (
      <Box data-testid="participant-avatar" data-participant-id={participantId} data-motion={motion} sx={{ display: 'flex' }}>
        <AgentAvatar iconUrl={iconUrl} pack={pack} motion={motion} size={ROW_AVATAR_SIZE} />
      </Box>
    );
  };
}

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

type Announced = Map<number, { label: string; at: number }>;

/**
 * The lines to announce now for `rows`, recording them in `last`. A label repeated says nothing; a change within
 * `ANNOUNCE_INTERVAL_MS` of the agent's last announcement waits (`held`, its due time returned) and is said then.
 */
export function announcements(rows: readonly ParticipantRowView[], last: Announced, now: number): { lines: string[]; dueAt: number | undefined } {
  const lines: string[] = [];
  let dueAt: number | undefined;
  for (const row of rows) {
    if (row.label === undefined) continue;
    const before = last.get(row.participantId);
    if (before?.label === row.label) continue;
    if (before !== undefined && now - before.at < ANNOUNCE_INTERVAL_MS) {
      const due = before.at + ANNOUNCE_INTERVAL_MS;
      dueAt = dueAt === undefined ? due : Math.min(dueAt, due);
      continue;
    }
    last.set(row.participantId, { label: row.label, at: now });
    lines.push(`${row.name}: ${row.label}`);
  }
  return { lines, dueAt };
}

/**
 * The politely announced line for the rows' labels: a repeat says nothing, and a change inside `ANNOUNCE_INTERVAL_MS`
 * of the agent's last announcement is said once that interval is over (the latest label then), so the final state is
 * never lost. Changes of several agents at once are read as one line.
 */
export function AgentStateAnnouncer({ rows }: { readonly rows: readonly ParticipantRowView[] }): ReactNode {
  const [message, setMessage] = useState('');
  const last = useRef<Announced>(new Map());
  const [tick, setTick] = useState(0);
  useEffect(() => {
    const { lines, dueAt } = announcements(rows, last.current, Date.now());
    if (lines.length > 0) setMessage(lines.join('. '));
    if (dueAt === undefined) return undefined;
    const timer = setTimeout(() => setTick((n) => n + 1), Math.max(0, dueAt - Date.now()));
    return () => clearTimeout(timer);
  }, [rows, tick]);
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
