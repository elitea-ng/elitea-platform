/**
 * Agents talking to each other in a Local work turn (ADR-0061 decisions 3d and 3e, #3064 #3065), folded into the
 * turn's view as side rows: a hand-off (`floor_handed`: "Mira → CI Doctor: …") and a consult (`consult_started`:
 * "Mira asked CI Doctor: …", then `consult_finished`: the reply). The page renders them with the chat's side talk,
 * folded into "Side conversation (n)" under the turn. The host sends excerpts; the committed side messages carry the
 * full texts for a reload, the web viewer and mobile.
 */
import type { TeamTalkEvent } from '@/shared/desktop/teamTalkIpc';
import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

/** One side row of a turn: who said it, to whom, and what. */
export interface SideTalkItem {
  key: string;
  kind: 'hand_off' | 'consult_question' | 'consult_reply';
  /** The author's name; `''` when the host did not know it (the page names it "An agent"). */
  author: string;
  /** The names of the agents it is addressed to. */
  addressees: string[];
  text: string;
  /** A consult's callee ran in the cloud without the folder (ADR-0061 decision 3e). */
  cloud?: true;
  /** A consult's reply that failed or was refused. */
  failed?: true;
}

interface TeamTalkView {
  /** The turn's side rows, in order (absent: none). */
  sideTalk?: SideTalkItem[];
  /** The turn's agent, for a consult's caller name. */
  agent?: { name: string; participantId: number } | undefined;
}

type TalkEvent = Extract<AgentEvent, TeamTalkEvent>;

export function isTeamTalkEvent(event: AgentEvent): event is TalkEvent {
  return event.kind === 'floor_handed' || event.kind === 'consult_started' || event.kind === 'consult_finished';
}

function callerName(view: TeamTalkView, caller: number): string {
  return view.agent?.participantId === caller ? view.agent.name : '';
}

function itemOf(view: TeamTalkView, event: TalkEvent): SideTalkItem {
  const key = `side${String(event.seq)}`;
  switch (event.kind) {
    case 'floor_handed':
      return { key, kind: 'hand_off', author: event.payload.from_name, addressees: event.payload.to.map((agent) => agent.name), text: event.payload.message_excerpt };
    case 'consult_started': {
      const { caller, callee_name: callee, question_excerpt: text, local } = event.payload;
      return { key, kind: 'consult_question', author: callerName(view, caller), addressees: [callee], text, ...(local ? {} : { cloud: true as const }) };
    }
    case 'consult_finished': {
      const { caller, callee_name: callee, reply_excerpt: text, ok, local } = event.payload;
      return { key, kind: 'consult_reply', author: callee, addressees: [callerName(view, caller)], text, ...(local ? {} : { cloud: true as const }), ...(ok ? {} : { failed: true as const }) };
    }
  }
}

/** The event's side row, once per delivery (a replayed event keeps its seq, so its key). */
export function applyTeamTalkEvent<V extends TeamTalkView>(view: V, event: TalkEvent): V {
  const rows = view.sideTalk ?? [];
  const item = itemOf(view, event);
  if (rows.some((row) => row.key === item.key)) return view;
  return { ...view, sideTalk: [...rows, item] };
}
