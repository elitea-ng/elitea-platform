/**
 * An in-memory `HandoffIpc` for tests and the dev harness: scripted answers
 * for each command, every call recorded, and `emit` to play the host's side
 * of `handoff://event`. A command can be made to reject the way the host
 * does (`{code, message, details}`).
 */
import type {
  ApplyResult,
  HandoffEvent,
  HandoffEventHandler,
  HandoffIpc,
  HandoffPreview,
  HandoffStatus,
  Preflight,
  ReclaimAnswer,
  TakeBackMode,
} from './handoffIpc';
import { HandoffIpcError } from './handoffIpc';

type FailableCommand = 'allowed' | 'preview' | 'send' | 'status' | 'reclaim' | 'preflight' | 'apply' | 'undo' | 'erase';

const EMPTY_PREVIEW: HandoffPreview = {
  files: 0,
  bytes: 0,
  unpacked_bytes: 0,
  excluded: [],
  findings: [],
  has_upstream: false,
  project_workspace_bytes_left: null,
  caps: { violations: [], largest: [] },
  shadow: false,
};

export const LOCAL_STATUS: HandoffStatus = { state: 'LOCAL', epoch: 0, executor: 'local', read_only: false, sent_from_here: false };

export interface FakeHandoffIpc extends HandoffIpc {
  emit(event: HandoffEvent): void;
  setAllowed(allowed: boolean): void;
  setPreview(preview: Partial<HandoffPreview>): void;
  /** What `handoff_status` answers from now on. */
  setStatus(status: Partial<HandoffStatus>): void;
  /** Answers of successive `handoff_status` calls; the last one repeats. */
  queueStatus(...statuses: Partial<HandoffStatus>[]): void;
  setReclaim(answer: ReclaimAnswer): void;
  setPreflight(preflight: Partial<Preflight>): void;
  setApply(result: Partial<ApplyResult>): void;
  /** Hold `handoff_send` until `releaseSend()` (to look at the progress). */
  holdSend(): void;
  releaseSend(): void;
  failNext(command: FailableCommand, code: string, message: string, details?: unknown): void;
  subscriberCount(): number;
  readonly calls: {
    previews: { workspaceId: string; conversationUuid: string; leave: string[] }[];
    sends: { workspaceId: string; conversationUuid: string; leave: string[] }[];
    statuses: string[];
    reclaims: string[];
    preflights: string[];
    applies: { preflightId: string; mode: TakeBackMode }[];
    undos: string[];
    erases: string[];
  };
}

export function createFakeHandoffIpc(initial: { status?: Partial<HandoffStatus>; allowed?: boolean; preview?: Partial<HandoffPreview> } = {}): FakeHandoffIpc {
  let allowed = initial.allowed ?? true;
  let preview: HandoffPreview = { ...EMPTY_PREVIEW, files: 12, bytes: 2048, unpacked_bytes: 8192, ...initial.preview };
  let status: HandoffStatus = { ...LOCAL_STATUS, ...initial.status };
  let queue: Partial<HandoffStatus>[] = [];
  let reclaim: ReclaimAnswer = { state: 'RECLAIMING', epoch: 3, cloud_sha: 'abc123' };
  let preflight: Preflight = {
    preflight_id: 'pf-1',
    outcome: 'fast_path',
    changed_paths: ['src/main.rs'],
    conflict_paths: [],
    skipped_paths: [],
    cloud_turns: 1,
    modes: ['apply', 'worktree', 'discard'],
  };
  let apply: ApplyResult = { applied: true, outcome: 'applied', checkpoint_id: 'w1:7', worktree_path: null, branch: 'elitea/cloud/abcd1234', imported_turns: 1, released: true };
  let hold: Promise<void> | undefined;
  let release: (() => void) | undefined;
  const handlers = new Set<HandoffEventHandler>();
  const failures = new Map<FailableCommand, HandoffIpcError>();
  const calls: FakeHandoffIpc['calls'] = { previews: [], sends: [], statuses: [], reclaims: [], preflights: [], applies: [], undos: [], erases: [] };
  const failure = (command: FailableCommand): Promise<never> | undefined => {
    const error = failures.get(command);
    if (error === undefined) return undefined;
    failures.delete(command);
    return Promise.reject(error);
  };

  return {
    calls,
    emit: (event) => {
      for (const handler of handlers) handler(event);
    },
    setAllowed: (next) => {
      allowed = next;
    },
    setPreview: (next) => {
      preview = { ...preview, ...next };
    },
    setStatus: (next) => {
      status = { ...status, ...next };
    },
    queueStatus: (...next) => {
      queue = next;
    },
    setReclaim: (next) => {
      reclaim = next;
    },
    setPreflight: (next) => {
      preflight = { ...preflight, ...next };
    },
    setApply: (next) => {
      apply = { ...apply, ...next };
    },
    holdSend: () => {
      hold = new Promise<void>((resolve) => {
        release = resolve;
      });
    },
    releaseSend: () => release?.(),
    failNext: (command, code, message, details = null) => {
      failures.set(command, new HandoffIpcError(code, message, details));
    },
    subscriberCount: () => handlers.size,
    allowed: () => failure('allowed') ?? Promise.resolve(allowed),
    preview(workspaceId, conversationUuid, leave = []) {
      calls.previews.push({ workspaceId, conversationUuid, leave: [...leave] });
      return failure('preview') ?? Promise.resolve({ ...preview });
    },
    async send(workspaceId, conversationUuid, leave) {
      calls.sends.push({ workspaceId, conversationUuid, leave: [...leave] });
      const failed = failure('send');
      if (failed !== undefined) return failed;
      if (hold !== undefined) await hold;
      status = { ...status, state: 'CLOUD_IDLE', executor: 'cloud', read_only: true, sent_from_here: true, epoch: 2 };
      return { handoff_id: 'h-1', state: 'CLOUD_IDLE', epoch: 2, files: preview.files, excluded: preview.excluded.length };
    },
    status(conversationUuid) {
      calls.statuses.push(conversationUuid);
      const failed = failure('status');
      if (failed !== undefined) return failed;
      const next = queue.shift();
      if (next !== undefined) status = { ...status, ...next };
      return Promise.resolve({ ...status });
    },
    reclaim(conversationUuid) {
      calls.reclaims.push(conversationUuid);
      return failure('reclaim') ?? Promise.resolve({ ...reclaim });
    },
    preflight(conversationUuid) {
      calls.preflights.push(conversationUuid);
      return failure('preflight') ?? Promise.resolve({ ...preflight });
    },
    apply(preflightId, mode) {
      calls.applies.push({ preflightId, mode });
      return failure('apply') ?? Promise.resolve({ ...apply });
    },
    undo(checkpointId) {
      calls.undos.push(checkpointId);
      return failure('undo') ?? Promise.resolve({ restored: preflight.changed_paths });
    },
    erase(conversationUuid) {
      calls.erases.push(conversationUuid);
      const failed = failure('erase');
      if (failed !== undefined) return failed;
      status = { ...LOCAL_STATUS, epoch: status.epoch + 1 };
      return Promise.resolve({ state: 'LOCAL', epoch: status.epoch });
    },
    onEvent(handler) {
      handlers.add(handler);
      return Promise.resolve(() => {
        handlers.delete(handler);
      });
    },
  };
}
