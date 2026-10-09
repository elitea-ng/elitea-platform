/**
 * Folds the host's `agent://event` stream into what the transcript renders.
 *
 * The channel is trusted for content but not for delivery: events can arrive
 * twice, out of order, and BEFORE `agent_turn_start` has returned the turn id
 * (the host starts emitting immediately). So the state keeps, per turn, the
 * raw events sorted by `seq` and deduplicated, and the view is a pure fold
 * over that list. The common case (a higher seq than any seen) is applied
 * incrementally; a late lower seq re-folds from the sorted list.
 */
import type { AgentEvent, ApprovalRequestPayload, TurnPhase } from '@/shared/desktop/workspaceIpc';

export type TranscriptItem =
  | { type: 'text'; key: string; text: string }
  | {
      type: 'tool';
      key: string;
      callId: string;
      tool: string;
      argsSummary: string;
      remote: boolean;
      result?: { ok: boolean; summary: string; truncated: boolean };
    };

interface TurnDone {
  committed: boolean;
  conversationId: string;
  messageIds: string[];
  changedFiles: number;
}

export interface TurnView {
  phase: TurnPhase | null;
  message?: string;
  items: TranscriptItem[];
  /** Approvals still waiting for an answer, oldest first. */
  approvals: ApprovalRequestPayload[];
  error?: { code: string; message: string };
  done?: TurnDone;
  /** The AGENTS.md files the host applied to this turn (from `status` `running`). */
  projectInstructions?: string[];
}

interface TurnRecord {
  events: AgentEvent[];
  resolved: string[];
  view: TurnView;
}

export interface TurnsState {
  activeTurnId: string | null;
  turns: Record<string, TurnRecord>;
  /** The turns this session started, oldest first (the channel also carries other sessions' turns). */
  begun: string[];
}

export type TurnAction =
  | { type: 'begin'; turnId: string }
  | { type: 'event'; event: AgentEvent }
  | { type: 'approval-resolved'; turnId: string; requestId: string }
  | { type: 'reset' };

export const initialTurnsState: TurnsState = { activeTurnId: null, turns: {}, begun: [] };

const EMPTY_VIEW: TurnView = { phase: null, items: [], approvals: [] };

const TERMINAL: readonly TurnPhase[] = ['done', 'cancelled', 'error'];

export function isTerminalPhase(phase: TurnPhase | null): boolean {
  return phase !== null && TERMINAL.includes(phase);
}

type EventOf<K extends AgentEvent['kind']> = Extract<AgentEvent, { kind: K }>;

function onStatus(view: TurnView, event: EventOf<'status'>): TurnView {
  const next: TurnView = { ...view, phase: event.payload.phase };
  if (event.payload.message !== undefined) next.message = event.payload.message;
  else delete next.message;
  if (event.payload.project_instructions !== undefined) next.projectInstructions = event.payload.project_instructions;
  if (isTerminalPhase(event.payload.phase)) next.approvals = [];
  return next;
}

function onTextDelta(view: TurnView, event: EventOf<'text_delta'>): TurnView {
  const last = view.items[view.items.length - 1];
  if (last?.type === 'text') {
    const merged: TranscriptItem = { ...last, text: last.text + event.payload.text };
    return { ...view, items: [...view.items.slice(0, -1), merged] };
  }
  return { ...view, items: [...view.items, { type: 'text', key: `t${event.seq}`, text: event.payload.text }] };
}

function onToolCall(view: TurnView, event: EventOf<'tool_call'>): TurnView {
  const { call_id: callId, tool, args_summary: argsSummary, remote } = event.payload;
  if (view.items.some((item) => item.type === 'tool' && item.callId === callId)) return view;
  return { ...view, items: [...view.items, { type: 'tool', key: `c${callId}`, callId, tool, argsSummary, remote }] };
}

function onToolResult(view: TurnView, event: EventOf<'tool_result'>): TurnView {
  const { call_id: callId, ok, summary, truncated } = event.payload;
  const result = { ok, summary, truncated };
  const index = view.items.findIndex((item) => item.type === 'tool' && item.callId === callId);
  if (index === -1) {
    // A result with no call (the call was lost): still show it, honestly unnamed.
    const orphan: TranscriptItem = { type: 'tool', key: `c${callId}`, callId, tool: callId, argsSummary: '', remote: false, result };
    return { ...view, items: [...view.items, orphan] };
  }
  return { ...view, items: view.items.map((item, i) => (i === index && item.type === 'tool' ? { ...item, result } : item)) };
}

function onApprovalRequest(view: TurnView, event: EventOf<'approval_request'>, resolved: readonly string[]): TurnView {
  const id = event.payload.request_id;
  if (resolved.includes(id) || view.approvals.some((a) => a.request_id === id) || isTerminalPhase(view.phase)) return view;
  return { ...view, approvals: [...view.approvals, event.payload] };
}

function onDone(view: TurnView, event: EventOf<'done'>): TurnView {
  const { committed, conversation_id: conversationId, message_ids: messageIds, changed_files: changedFiles } = event.payload;
  return { ...view, done: { committed, conversationId, messageIds, changedFiles }, approvals: [] };
}

function applyEvent(view: TurnView, event: AgentEvent, resolved: readonly string[]): TurnView {
  switch (event.kind) {
    case 'status':
      return onStatus(view, event);
    case 'text_delta':
      return onTextDelta(view, event);
    case 'tool_call':
      return onToolCall(view, event);
    case 'tool_result':
      return onToolResult(view, event);
    case 'approval_request':
      return onApprovalRequest(view, event, resolved);
    case 'error':
      return { ...view, error: event.payload, approvals: [] };
    case 'done':
      return onDone(view, event);
  }
}

function fold(events: readonly AgentEvent[], resolved: readonly string[]): TurnView {
  return events.reduce((view, event) => applyEvent(view, event, resolved), EMPTY_VIEW);
}

function withoutResolved(view: TurnView, requestId: string): TurnView {
  return { ...view, approvals: view.approvals.filter((a) => a.request_id !== requestId) };
}

function reduceEvent(state: TurnsState, event: AgentEvent): TurnsState {
  const record = state.turns[event.turn_id] ?? { events: [], resolved: [], view: EMPTY_VIEW };
  const last = record.events[record.events.length - 1];
  let next: TurnRecord;
  if (last === undefined || event.seq > last.seq) {
    next = { ...record, events: [...record.events, event], view: applyEvent(record.view, event, record.resolved) };
  } else if (record.events.some((seen) => seen.seq === event.seq)) {
    return state; // a duplicate delivery
  } else {
    const events = [...record.events, event].sort((a, b) => a.seq - b.seq);
    next = { ...record, events, view: fold(events, record.resolved) };
  }
  return { ...state, turns: { ...state.turns, [event.turn_id]: next } };
}

export function turnReducer(state: TurnsState, action: TurnAction): TurnsState {
  switch (action.type) {
    case 'begin':
      return {
        ...state,
        activeTurnId: action.turnId,
        begun: state.begun.includes(action.turnId) ? state.begun : [...state.begun, action.turnId],
      };
    case 'reset':
      return initialTurnsState;
    case 'event':
      return reduceEvent(state, action.event);
    case 'approval-resolved': {
      const record = state.turns[action.turnId];
      if (record === undefined) return state;
      const resolved = [...record.resolved, action.requestId];
      const next: TurnRecord = { ...record, resolved, view: withoutResolved(record.view, action.requestId) };
      return { ...state, turns: { ...state.turns, [action.turnId]: next } };
    }
  }
}

export function activeView(state: TurnsState): TurnView {
  return state.activeTurnId === null ? EMPTY_VIEW : (state.turns[state.activeTurnId]?.view ?? EMPTY_VIEW);
}

/** The turns this session started before the active one, oldest first: the thread's earlier exchanges. */
export function earlierViews(state: TurnsState): { turnId: string; view: TurnView }[] {
  return state.begun
    .filter((turnId) => turnId !== state.activeTurnId)
    .map((turnId) => ({ turnId, view: state.turns[turnId]?.view ?? EMPTY_VIEW }));
}
