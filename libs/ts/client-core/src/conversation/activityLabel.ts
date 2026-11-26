/**
 * The text an agent's avatar state reads as (ADR-0061 decision 4c): "Thinking",
 * "Running cargo", "Waiting for CI Doctor", "Handed to CI Doctor". One helper
 * for web, desktop and mobile, so the wording matches on every client; each
 * client passes its translated templates.
 *
 * A label is built from the state, the sanitised names of `activity`
 * (`tool`, `program`) and participant names only: never from command text or
 * tool arguments. It is at most `MAX_ACTIVITY_LABEL` characters.
 */
import { AGENT_STATE, WAITING_ON, type AgentActivity, type AgentState } from '../stream/frameTypes';

/** The longest label `activityLabel` returns. */
export const MAX_ACTIVITY_LABEL = 80;

/**
 * The templates of every label, `{tool}` and `{name}` filled in. Each client
 * passes its own translation; `ACTIVITY_LABELS_EN` is the source wording.
 */
export interface ActivityLabels {
  readonly idle: string;
  readonly thinking: string;
  readonly waking: string;
  readonly speaking: string;
  /** `working` without a named tool. */
  readonly working: string;
  /** `working` with a tool or program: `{tool}`. */
  readonly running: string;
  readonly waitingForYou: string;
  /** `waiting` on an agent: `{name}`. */
  readonly waitingForAgent: string;
  /** The agent's name is not known. */
  readonly anotherAgent: string;
  /** A turn ended with a hand-off: `{name}` (the receivers, joined). */
  readonly handedTo: string;
  readonly done: string;
  readonly error: string;
}

export const ACTIVITY_LABELS_EN: ActivityLabels = {
  idle: 'Idle',
  thinking: 'Thinking',
  waking: 'Resuming',
  speaking: 'Writing',
  working: 'Working',
  running: 'Running {tool}',
  waitingForYou: 'Waiting for you',
  waitingForAgent: 'Waiting for {name}',
  anotherAgent: 'another agent',
  handedTo: 'Handed to {name}',
  done: 'Done',
  error: 'Error',
};

/** A participant's display name, or `undefined` when it is not known. */
export type ParticipantName = (participantId: number) => string | undefined;

function fill(template: string, values: Readonly<Record<string, string>>): string {
  return template.replace(/\{(\w+)\}/g, (whole, key: string) => values[key] ?? whole);
}

function cut(text: string): string {
  if (text.length <= MAX_ACTIVITY_LABEL) return text;
  return `${text.slice(0, MAX_ACTIVITY_LABEL - 1).trimEnd()}…`;
}

/**
 * The label of an agent's `state` and `activity`. `names` resolves the peer
 * and hand-off receivers; `labels` are the client's translated templates.
 */
export function activityLabel(
  state: AgentState,
  activity: AgentActivity | undefined,
  names: ParticipantName = () => undefined,
  labels: ActivityLabels = ACTIVITY_LABELS_EN,
): string {
  const nameOf = (id: number | undefined) => (id === undefined ? undefined : names(id)?.trim() || undefined);
  switch (state) {
    case AGENT_STATE.Thinking:
      return cut(labels.thinking);
    case AGENT_STATE.Waking:
      return cut(labels.waking);
    case AGENT_STATE.Speaking:
      return cut(labels.speaking);
    case AGENT_STATE.Working: {
      const tool = activity?.program ?? activity?.tool;
      return cut(tool === undefined ? labels.working : fill(labels.running, { tool }));
    }
    case AGENT_STATE.Waiting: {
      if (activity?.waiting_on !== WAITING_ON.Agent) return cut(labels.waitingForYou);
      return cut(fill(labels.waitingForAgent, { name: nameOf(activity.peer_participant_id) ?? labels.anotherAgent }));
    }
    case AGENT_STATE.Success: {
      const receivers = (activity?.handoff_to ?? []).map((id) => nameOf(id) ?? labels.anotherAgent);
      return cut(receivers.length > 0 ? fill(labels.handedTo, { name: receivers.join(', ') }) : labels.done);
    }
    case AGENT_STATE.Error:
      return cut(labels.error);
    default:
      return cut(labels.idle);
  }
}
