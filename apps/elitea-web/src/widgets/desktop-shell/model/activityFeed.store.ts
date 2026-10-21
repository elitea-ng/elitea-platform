/**
 * The window's activity feed: what the app is doing right now, folded from
 * the three host channels — `agent://event` (turns), `index://event` (index
 * builds) and `handoff://event` (sends to the cloud) — plus the few facts the
 * open thread registers (which workspace a turn belongs to, a thread running
 * in the cloud). The island and its popover read it; nothing else does.
 *
 * Events are not replayed, so a lost one would leave an activity on screen
 * for good: `useActivityFeedSources` reconciles unfinished turns against
 * `agent_turn_status` when the window gets focus and while a commit is
 * pending. The store holds raw facts; `buildActivities` (pure) turns them
 * into the ranked list the island shows.
 */
import { create, type StoreApi, type UseBoundStore } from 'zustand';

import { foldProgress } from '@/features/workspace-index';
import type { AgentEvent, ApprovalRequestPayload, TurnDonePayload, TurnPhase } from '@/shared/desktop/workspaceIpc';
import type { HandoffEvent } from '@/shared/desktop/handoffIpc';
import type { IndexEvent, IndexStatus } from '@/shared/desktop/indexIpc';

export interface PendingApproval {
  requestId: string;
  tool: string;
  title: string;
}

/** A turn's tokens: what its calls sent (summed), what they produced, and the context the latest call used. */
export interface TurnUsage {
  inputTokens: number;
  outputTokens: number;
  contextTokens: number;
}

export interface TrackedTurn {
  turnId: string;
  workspaceId: string | null;
  workspaceName: string | null;
  conversationId: string | null;
  phase: TurnPhase;
  /** Epoch ms the feed first heard of the turn. */
  startedAt: number;
  /** The tool running now (a `tool_call` without its `tool_result`). */
  tool: string | null;
  /** Tool calls so far. */
  steps: number;
  approvals: PendingApproval[];
  /** After `done`: `pending` while the turn is being saved, `failed` once the retries ran out. */
  commit: 'none' | 'pending' | 'failed';
  finished: boolean;
  changedFiles: number;
  /** Tokens over the turn's model calls so far; `null` until the gateway reported usage. */
  usage: TurnUsage | null;
}

export interface TrackedIndex {
  workspaceId: string;
  workspaceName: string | null;
  state: IndexStatus['state'];
  entities: number;
  done: number;
  total: number | null;
}

export interface TrackedSend {
  conversationUuid: string;
  phase: HandoffEvent['phase'];
  done: number;
  total: number;
}

export interface TrackedCloud {
  conversationUuid: string;
  /** The thread's state: `CLOUD_IDLE`, `CLOUD_RUNNING`, `RECLAIMING`, … */
  state: string;
  title: string;
  workspaceId: string | null;
  conversationId: string | null;
}

/** A turn streaming from the cloud copy of a thread (the desktop chats with it). */
export interface TrackedCloudRun {
  conversationUuid: string;
  /** Epoch ms the stream began. */
  startedAt: number;
  /** A stream is running now. */
  running: boolean;
  /** What the thread waits on the person for ("Approve: run_command"), or `null`. */
  waiting: string | null;
  title: string;
  workspaceId: string | null;
  conversationId: string | null;
}

const SEND_OVER: ReadonlySet<HandoffEvent['phase']> = new Set(['complete', 'cancelled', 'error']);

export interface ActivityFeedState {
  readonly turns: Readonly<Record<string, TrackedTurn>>;
  readonly indexes: Readonly<Record<string, TrackedIndex>>;
  readonly sends: Readonly<Record<string, TrackedSend>>;
  readonly cloud: Readonly<Record<string, TrackedCloud>>;
  /** Cloud turns streaming now, by conversation UUID. */
  readonly cloudRuns: Readonly<Record<string, TrackedCloudRun>>;
  /** A turn the open thread started: where it belongs. */
  readonly registerTurn: (turnId: string, owner: { workspaceId: string; workspaceName: string; conversationId: string }) => void;
  readonly applyAgentEvent: (event: AgentEvent, now?: number) => void;
  /** What `agent_turn_status` answered for a turn the feed believes unfinished. */
  /** `approval_respond` answered a request: it no longer waits. */
  readonly answerApproval: (requestId: string) => void;
  readonly reconcileTurn: (turnId: string, status: { state: 'running' | 'committing' | 'done'; done: TurnDonePayload | null } | null) => void;
  readonly applyIndexEvent: (event: IndexEvent, workspaceName?: string | null) => void;
  readonly applyHandoffEvent: (event: HandoffEvent) => void;
  readonly setCloudRun: (conversationUuid: string, run: TrackedCloudRun | null) => void;
  readonly setCloud: (conversationUuid: string, cloud: TrackedCloud | null) => void;
  readonly reset: () => void;
}

type FeedStore = UseBoundStore<StoreApi<ActivityFeedState>>;

function blankTurn(turnId: string, now: number): TrackedTurn {
  return {
    turnId,
    workspaceId: null,
    workspaceName: null,
    conversationId: null,
    phase: 'resolving',
    startedAt: now,
    tool: null,
    steps: 0,
    approvals: [],
    commit: 'none',
    finished: false,
    changedFiles: 0,
    usage: null,
  };
}

function withApproval(turn: TrackedTurn, payload: ApprovalRequestPayload): TrackedTurn {
  const label = payload.kind === 'continue' ? 'continue' : payload.tool;
  return { ...turn, approvals: [...turn.approvals.filter((a) => a.requestId !== payload.request_id), { requestId: payload.request_id, tool: label, title: payload.title }] };
}

/** Each model call re-sends the conversation: the turn's input is the sum, the context in use the latest call's. */
function withUsage(turn: TrackedTurn, usage: { prompt_tokens: number; completion_tokens: number }): TrackedTurn {
  const before = turn.usage ?? { inputTokens: 0, outputTokens: 0, contextTokens: 0 };
  return {
    ...turn,
    usage: {
      inputTokens: before.inputTokens + usage.prompt_tokens,
      outputTokens: before.outputTokens + usage.completion_tokens,
      contextTokens: usage.prompt_tokens,
    },
  };
}

/** `done`: finished unless its commit is still to land. */
function settled(turn: TrackedTurn, done: TurnDonePayload): TrackedTurn {
  const commit = done.commit === 'pending' ? 'pending' : 'none';
  return { ...turn, approvals: [], tool: null, commit, finished: commit === 'none', changedFiles: done.changed_files };
}

const TERMINAL_PHASES: ReadonlySet<TurnPhase> = new Set<TurnPhase>(['done', 'cancelled', 'error']);

/**
 * The turn ended: an `error` event or a terminal `status` (a turn refused at
 * the start never sends `done`). It stays listed only while its commit is
 * still to land. Approvals close here and in `answerApproval`, never on other
 * events (IPC.md, "Activity").
 */
function ended(turn: TrackedTurn, phase: TurnPhase): TrackedTurn {
  return { ...turn, phase, tool: null, approvals: [], finished: turn.commit === 'none' };
}

/** Folds one agent event into a turn. Pure. */
export function foldTurn(turn: TrackedTurn, event: AgentEvent): TrackedTurn {
  switch (event.kind) {
    case 'status':
      return TERMINAL_PHASES.has(event.payload.phase) ? ended(turn, event.payload.phase) : { ...turn, phase: event.payload.phase };
    case 'tool_call':
      return { ...turn, tool: event.payload.tool, steps: turn.steps + 1 };
    case 'tool_result':
      return { ...turn, tool: null };
    case 'text_delta':
      return turn;
    case 'approval_request':
      return withApproval(turn, event.payload);
    case 'approval_closed':
      return { ...turn, approvals: turn.approvals.filter((a) => a.requestId !== event.payload.request_id) };
    case 'error':
      return ended(turn, 'error');
    case 'done':
      return settled(turn, event.payload);
    case 'usage':
      return withUsage(turn, event.payload);
    case 'commit_failed':
      return { ...turn, commit: 'failed', finished: false };
  }
}

function foldIndex(previous: TrackedIndex | undefined, event: IndexEvent, name: string | null): TrackedIndex {
  const status = event.status;
  const base: TrackedIndex = previous ?? { workspaceId: event.workspace_id, workspaceName: name, state: status.state, entities: 0, done: 0, total: null };
  // The same folding the index UI does (one set of rules for "Parsing n files" / "Processed n files").
  const progress = foldProgress(previous === undefined ? null : { done: base.done, total: base.total }, event) ?? { done: 0, total: null };
  return { ...base, workspaceName: name ?? base.workspaceName, state: status.state, entities: status.entities, done: progress.done, total: progress.total };
}

export function createActivityFeedStore(): FeedStore {
  return create<ActivityFeedState>((set) => ({
    turns: {},
    indexes: {},
    sends: {},
    cloud: {},
    cloudRuns: {},
    registerTurn: (turnId, owner) =>
      set((state) => {
        const turn = state.turns[turnId] ?? blankTurn(turnId, Date.now());
        return { turns: { ...state.turns, [turnId]: { ...turn, ...owner } } };
      }),
    applyAgentEvent: (event, now = Date.now()) =>
      set((state) => {
        const turn = state.turns[event.turn_id] ?? blankTurn(event.turn_id, now);
        const next = foldTurn(turn, event);
        const turns = { ...state.turns, [event.turn_id]: next };
        // Settled turns do not pile up: keep only the unfinished ones and the newest few.
        return { turns: prune(turns) };
      }),
    answerApproval: (requestId) =>
      set((state) => {
        const turns = Object.fromEntries(
          Object.entries(state.turns).map(([id, turn]) => [id, turn.approvals.some((a) => a.requestId === requestId) ? { ...turn, approvals: turn.approvals.filter((a) => a.requestId !== requestId) } : turn]),
        );
        return { turns };
      }),
    reconcileTurn: (turnId, status) =>
      set((state) => {
        const turn = state.turns[turnId];
        if (turn === undefined) return {};
        // The host forgot the turn (an app restart): it is over.
        if (status === null) return { turns: prune({ ...state.turns, [turnId]: { ...turn, finished: true, commit: 'none', approvals: [], tool: null } }) };
        if (status.state === 'running') return {};
        if (status.state === 'committing') return { turns: { ...state.turns, [turnId]: { ...turn, commit: 'pending', approvals: [], tool: null } } };
        const failed = status.done?.commit === 'failed';
        const next: TrackedTurn = { ...turn, approvals: [], tool: null, commit: failed ? 'failed' : 'none', finished: !failed, changedFiles: status.done?.changed_files ?? turn.changedFiles };
        return { turns: prune({ ...state.turns, [turnId]: next }) };
      }),
    applyIndexEvent: (event, name = null) =>
      set((state) => {
        const next = foldIndex(state.indexes[event.workspace_id], event, name);
        return { indexes: { ...state.indexes, [event.workspace_id]: next } };
      }),
    applyHandoffEvent: (event) =>
      set((state) => {
        const sends = { ...state.sends };
        if (SEND_OVER.has(event.phase)) delete sends[event.conversation_uuid];
        else sends[event.conversation_uuid] = { conversationUuid: event.conversation_uuid, phase: event.phase, done: event.done, total: event.total };
        return { sends };
      }),
    setCloudRun: (uuid, run) =>
      set((state) => {
        const next = { ...state.cloudRuns };
        if (run === null) delete next[uuid];
        else next[uuid] = run;
        return { cloudRuns: next };
      }),
    setCloud: (uuid, cloud) =>
      set((state) => {
        const next = { ...state.cloud };
        if (cloud === null) delete next[uuid];
        else next[uuid] = cloud;
        return { cloud: next };
      }),
    reset: () => set({ turns: {}, indexes: {}, sends: {}, cloud: {}, cloudRuns: {} }),
  }));
}

const KEEP_FINISHED = 3;

function prune(turns: Record<string, TrackedTurn>): Record<string, TrackedTurn> {
  const all = Object.values(turns);
  const finished = all.filter((turn) => turn.finished).sort((a, b) => b.startedAt - a.startedAt);
  const drop = new Set(finished.slice(KEEP_FINISHED).map((turn) => turn.turnId));
  if (drop.size === 0) return turns;
  return Object.fromEntries(all.filter((turn) => !drop.has(turn.turnId)).map((turn) => [turn.turnId, turn]));
}

let instance: FeedStore | undefined;

function resolveStore(): FeedStore {
  instance ??= createActivityFeedStore();
  return instance;
}

function useActivityFeedHook<T>(selector: (state: ActivityFeedState) => T): T {
  return resolveStore()(selector);
}

/** The lazily-constructed singleton, with the zustand `getState`/`subscribe` surface. */
export const useActivityFeed = Object.assign(useActivityFeedHook, {
  getState: (): ActivityFeedState => resolveStore().getState(),
  subscribe: (listener: (state: ActivityFeedState) => void): (() => void) => resolveStore().subscribe(listener),
});
