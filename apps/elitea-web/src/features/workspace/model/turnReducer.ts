/**
 * Folds the host's `agent://event` stream into what the transcript renders.
 *
 * The channel is trusted for content but not for delivery: events can arrive
 * twice, out of order, and BEFORE `agent_turn_start` has returned the turn id
 * (the host starts emitting immediately). So the state keeps, per turn, the
 * events sorted by `seq` and deduplicated, and the view is a pure fold over
 * that list. The common case (a higher seq than any seen) is applied
 * incrementally; a late lower seq re-folds from the sorted list.
 *
 * Kept compact: a `text_delta` whose seq directly follows a stored text run
 * is merged into it (the run then covers `seq..last`), so a long answer is
 * one stored event, not thousands, and an append copies a short list. The
 * channel is global (every workspace's turns), so the turns this hook did
 * not begin are kept only for the few most recently heard
 * ([`MAX_UNCLAIMED_TURNS`]): one of them may be the turn whose start has not
 * returned yet.
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

/** A stored event; a merged text run covers the seqs `seq..last`, each seen. */
type StoredEvent = AgentEvent & { last: number };

interface TurnRecord {
  events: StoredEvent[];
  resolved: string[];
  view: TurnView;
  /**
   * An adopted turn's events came from the history, where consecutive text
   * deltas are one row numbered by the LAST of them: a live event at or
   * below this seq is already in it.
   */
  floor?: number;
}

export interface TurnsState {
  activeTurnId: string | null;
  turns: Record<string, TurnRecord>;
  /** The turns this session started, oldest first (the channel also carries other sessions' turns). */
  begun: string[];
  /** Turns heard of but not begun here, least recently heard first; at most `MAX_UNCLAIMED_TURNS`. */
  unclaimed: string[];
}

export type TurnAction =
  | { type: 'begin'; turnId: string }
  | { type: 'event'; event: AgentEvent }
  | { type: 'approval-resolved'; turnId: string; requestId: string }
  /** A turn still running that this session did not start (the thread was reopened): its recorded events, then live ones. */
  | { type: 'adopt'; turnId: string; events: readonly AgentEvent[] }
  | { type: 'reset' };

export const initialTurnsState: TurnsState = { activeTurnId: null, turns: {}, begun: [], unclaimed: [] };

/**
 * How many turns not begun here are kept. Only one of this hook's turns can
 * be starting at a time; the rest are other workspaces' turns on the shared
 * channel, which this hook never shows.
 */
export const MAX_UNCLAIMED_TURNS = 4;

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

function lastSeq(event: AgentEvent | StoredEvent): number {
  return 'last' in event ? event.last : event.seq;
}

/** `event` merged onto `run` when it is a text delta (or run) directly following the text run; otherwise null. */
function extendRun(run: StoredEvent | undefined, event: AgentEvent | StoredEvent): StoredEvent | null {
  if (run?.kind !== 'text_delta' || event.kind !== 'text_delta' || event.seq !== run.last + 1) return null;
  return { ...run, payload: { text: run.payload.text + event.payload.text }, last: lastSeq(event) };
}

/** Sorted events (unique seqs) as stored: contiguous text deltas merged into runs. */
function compact(events: readonly (AgentEvent | StoredEvent)[]): StoredEvent[] {
  const out: StoredEvent[] = [];
  for (const event of events) {
    const merged = extendRun(out[out.length - 1], event);
    if (merged === null) out.push({ ...event, last: lastSeq(event) });
    else out[out.length - 1] = merged;
  }
  return out;
}

function withoutResolved(view: TurnView, requestId: string): TurnView {
  return { ...view, approvals: view.approvals.filter((a) => a.request_id !== requestId) };
}

function sortedUnique(events: readonly AgentEvent[]): AgentEvent[] {
  const bySeq = new Map<number, AgentEvent>();
  for (const event of events) if (!bySeq.has(event.seq)) bySeq.set(event.seq, event);
  return [...bySeq.values()].sort((a, b) => a.seq - b.seq);
}

/**
 * The view of a recorded turn (`thread_history`): its events folded exactly
 * as live ones. Nothing in it waits for an answer any more, so it carries
 * no approvals.
 */
export function replayView(events: readonly AgentEvent[]): TurnView {
  return { ...fold(sortedUnique(events), []), approvals: [] };
}

function appended(events: readonly StoredEvent[], event: AgentEvent): StoredEvent[] {
  const merged = extendRun(events[events.length - 1], event);
  return merged === null ? [...events, { ...event, last: event.seq }] : [...events.slice(0, -1), merged];
}

/** Keep `turnId` as the most recently heard unclaimed turn; forget the oldest past the bound. */
function heardUnclaimed(state: TurnsState, turnId: string): Pick<TurnsState, 'turns' | 'unclaimed'> {
  if (state.begun.includes(turnId)) return state;
  const unclaimed = [...state.unclaimed.filter((id) => id !== turnId), turnId];
  if (unclaimed.length <= MAX_UNCLAIMED_TURNS) return { turns: state.turns, unclaimed };
  const dropped = unclaimed.slice(0, unclaimed.length - MAX_UNCLAIMED_TURNS);
  const turns = { ...state.turns };
  for (const id of dropped) delete turns[id];
  return { turns, unclaimed: unclaimed.slice(-MAX_UNCLAIMED_TURNS) };
}

function reduceEvent(state: TurnsState, event: AgentEvent): TurnsState {
  const record = state.turns[event.turn_id] ?? { events: [], resolved: [], view: EMPTY_VIEW };
  if (record.floor !== undefined && event.seq <= record.floor) return state;
  const last = record.events[record.events.length - 1];
  let next: TurnRecord;
  if (last === undefined || event.seq > last.last) {
    next = { ...record, events: appended(record.events, event), view: applyEvent(record.view, event, record.resolved) };
  } else if (record.events.some((seen) => seen.seq <= event.seq && event.seq <= seen.last)) {
    return state; // a duplicate delivery
  } else {
    const events = compact([...record.events, event].sort((a, b) => a.seq - b.seq));
    next = { ...record, events, view: fold(events, record.resolved) };
  }
  const kept = heardUnclaimed(state, event.turn_id);
  return { ...state, unclaimed: kept.unclaimed, turns: { ...kept.turns, [event.turn_id]: next } };
}

export function turnReducer(state: TurnsState, action: TurnAction): TurnsState {
  switch (action.type) {
    case 'begin':
      return {
        ...state,
        activeTurnId: action.turnId,
        begun: state.begun.includes(action.turnId) ? state.begun : [...state.begun, action.turnId],
        unclaimed: state.unclaimed.filter((id) => id !== action.turnId),
      };
    case 'reset':
      return initialTurnsState;
    case 'adopt': {
      const events = sortedUnique(action.events);
      const floor = events[events.length - 1]?.seq ?? -1;
      const record: TurnRecord = { events: compact(events), resolved: [], view: fold(events, []), floor };
      return {
        activeTurnId: action.turnId,
        turns: { ...state.turns, [action.turnId]: record },
        begun: state.begun.includes(action.turnId) ? state.begun : [...state.begun, action.turnId],
        unclaimed: state.unclaimed.filter((id) => id !== action.turnId),
      };
    }
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
