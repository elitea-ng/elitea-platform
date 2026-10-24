/**
 * An in-memory `HandoffIpc` for tests and the dev harness: scripted answers
 * for each command, every call recorded, and `emit` to play the host's side
 * of `handoff://event`. A command can be made to reject the way the host
 * does (`{code, message, details}`).
 *
 * It models what the host decides, so the screens can be driven end to end:
 *
 *   - the preview honours `leave_on_laptop` (the path is left out, its
 *     findings with it) and the folder's settings (an includable excluded
 *     path the settings `include` is sent, its findings become warnings,
 *     `include_findings`);
 *   - the folder's settings are kept per workspace and validated
 *     (`invalid_request`, `include_not_allowed`);
 *   - the env harvest offers names and value lengths only, nothing selected
 *     but the settings' `env_names`;
 *   - a send with include warnings needs `confirmIncludes`, an env over the
 *     limits rejects `env_too_large`, and a held send can be cancelled.
 *
 * The scripted preview's `findings` may name any path: a finding in a path
 * that is excluded (and not included) or left on the laptop is dropped, one
 * in an included path is a warning, any other blocks.
 */
import type {
  ApplyOutcome,
  ApplyResult,
  EnvVar,
  FolderHandoffSettings,
  HandoffEvent,
  HandoffEventHandler,
  HandoffIpc,
  HandoffPolicy,
  HandoffPreview,
  HandoffSent,
  HandoffSettingsAnswer,
  HandoffStatus,
  Preflight,
  ReclaimAnswer,
  TakeBackMode,
} from './handoffIpc';
import { HandoffIpcError } from './handoffIpc';
import { previewEnv } from './handoffIpc.fakeEnv';

type FailableCommand =
  | 'allowed' | 'preview' | 'send' | 'cancel' | 'status' | 'statusMany' | 'reclaim'
  | 'preflight' | 'apply' | 'undo' | 'erase' | 'settingsGet' | 'settingsSet';

export const DEFAULT_POLICY: HandoffPolicy = {
  source: 'server',
  exclusions: { platform: ['.env', '*.pem', 'id_rsa*'], project: [], user: [], path_deny: [] },
  allow_user_include: true,
  env_harvest: false,
  env_limits: { max_vars: 50, max_bytes: 32 * 1024 },
  limits: {
    max_segment_bytes: 200 * 1024 * 1024,
    max_file_bytes: 25 * 1024 * 1024,
    max_files: 20_000,
    max_unpacked_bytes: 1024 * 1024 * 1024,
    project_workspace_bytes_left: null,
  },
};

const EMPTY_PREVIEW: HandoffPreview = {
  files: 0,
  bytes: 0,
  unpacked_bytes: 0,
  excluded: [],
  included: [],
  findings: [],
  include_findings: [],
  has_upstream: false,
  project_workspace_bytes_left: null,
  caps: { violations: [], largest: [] },
  shadow: false,
  policy: DEFAULT_POLICY,
  env: null,
};

const EMPTY_SETTINGS: FolderHandoffSettings = { exclude_patterns: [], include: [], env_names: [] };

export const LOCAL_STATUS: HandoffStatus = { state: 'LOCAL', epoch: 0, executor: 'local', read_only: false, sent_from_here: false };

const ENV_NAME = /^[A-Za-z_][A-Za-z0-9_]*$/;

export interface FakeHandoffIpc extends HandoffIpc {
  emit(event: HandoffEvent): void;
  setAllowed(allowed: boolean): void;
  /** The folder as the host sees it before `leave_on_laptop` and the settings apply. */
  setPreview(preview: Partial<HandoffPreview>): void;
  setPolicy(policy: Partial<HandoffPolicy>): void;
  /** What the environment holds now (names and value lengths). */
  setEnvOffered(offered: EnvVar[]): void;
  /** A folder's stored settings, without a call being recorded. */
  setSettings(workspaceId: string, settings: Partial<FolderHandoffSettings>): void;
  /** What `handoff_status` answers from now on. */
  setStatus(status: Partial<HandoffStatus>): void;
  /** What one thread answers, in `handoff_status` and `handoff_status_many`, over the shared status. */
  setStatusOf(conversationUuid: string, status: Partial<HandoffStatus>): void;
  /** Answers of successive `handoff_status` calls; the last one repeats. */
  queueStatus(...statuses: Partial<HandoffStatus>[]): void;
  setReclaim(answer: ReclaimAnswer): void;
  setPreflight(preflight: Partial<Preflight>): void;
  setApply(result: Partial<ApplyResult>): void;
  /** Hold `handoff_send` until `releaseSend()` (to look at the progress, or cancel). */
  holdSend(): void;
  releaseSend(): void;
  failNext(command: FailableCommand, code: string, message: string, details?: unknown): void;
  subscriberCount(): number;
  readonly calls: {
    previews: { workspaceId: string; conversationUuid: string; leave: string[] }[];
    sends: { workspaceId: string; conversationUuid: string; leave: string[]; confirmIncludes: boolean }[];
    cancels: string[];
    statuses: string[];
    statusManys: string[][];
    reclaims: string[];
    preflights: string[];
    /** The turn each preflight asked for (`undefined`: the latest). */
    preflightTurns: (number | undefined)[];
    applies: { preflightId: string; mode: TakeBackMode; upToTurn?: number }[];
    undos: string[];
    erases: string[];
    settingsGets: string[];
    settingsSets: { workspaceId: string; settings: FolderHandoffSettings }[];
  };
}

interface FakeInitial {
  status?: Partial<HandoffStatus>;
  allowed?: boolean;
  preview?: Partial<HandoffPreview>;
  policy?: Partial<HandoffPolicy>;
  envOffered?: EnvVar[];
  /** The host could not read the login shell: nothing offered, this notice. */
  envNotice?: string;
  settings?: Record<string, Partial<FolderHandoffSettings>>;
}

function matchesInclude(path: string, include: readonly string[]): boolean {
  return include.some((entry) => {
    const pattern = entry.startsWith('/') ? entry.slice(1) : entry;
    if (!pattern.includes('*')) return pattern === path;
    const re = new RegExp(`^${pattern.split('*').map((part) => part.replace(/[.+?^${}()|[\]\\]/g, '\\$&')).join('[^/]*')}$`);
    return re.test(path);
  });
}

export function createFakeHandoffIpc(initial: FakeInitial = {}): FakeHandoffIpc {
  let allowed = initial.allowed ?? true;
  let base: HandoffPreview = { ...EMPTY_PREVIEW, files: 12, bytes: 2048, unpacked_bytes: 8192, ...initial.preview };
  let policy: HandoffPolicy = { ...DEFAULT_POLICY, ...initial.policy };
  let envOffered: EnvVar[] = initial.envOffered ?? [];
  const settings = new Map<string, FolderHandoffSettings>(Object.entries(initial.settings ?? {}).map(([id, value]) => [id, { ...EMPTY_SETTINGS, ...value }]));
  let status: HandoffStatus = { ...LOCAL_STATUS, ...initial.status };
  const perThread = new Map<string, Partial<HandoffStatus>>();
  let queue: Partial<HandoffStatus>[] = [];
  let reclaim: ReclaimAnswer = { state: 'RECLAIMING', epoch: 3, cloud_sha: 'abc123' };
  let preflight: Preflight = {
    preflight_id: 'pf-1',
    outcome: 'fast_path',
    applied: false,
    changed_paths: [{ path: 'src/main.rs', change: 'modified', from: null }],
    conflict_paths: [],
    skipped_paths: [],
    base: 'b'.repeat(40),
    result_tree: 'c'.repeat(40),
    cloud_turns: 1,
    up_to_turn: 1,
    modes: ['apply', 'worktree', 'discard'],
    files: [{ path: 'src/main.rs', status: 'modified', added: 1, removed: 1, diff: '--- a/src/main.rs\n+++ b/src/main.rs\n@@ -1 +1 @@\n-fn main() {}\n+fn main() { run() }\n' }],
    diffs_truncated: false,
  };
  let apply: Omit<ApplyResult, 'report'> & { report?: ApplyResult['report'] } = {
    applied: true,
    outcome: 'applied',
    checkpoint_id: 'w1:7',
    worktree_path: null,
    branch: 'elitea/cloud/abcd1234',
    imported_turns: 1,
    released: true,
    copied: [],
  };
  let hold: Promise<void> | undefined;
  let release: (() => void) | undefined;
  let running: { uuid: string; cancel: () => void } | undefined;
  const handlers = new Set<HandoffEventHandler>();
  const failures = new Map<FailableCommand, HandoffIpcError>();
  const calls: FakeHandoffIpc['calls'] = {
    previews: [], sends: [], cancels: [], statuses: [], statusManys: [], reclaims: [],
    preflights: [], preflightTurns: [], applies: [], undos: [], erases: [], settingsGets: [], settingsSets: [],
  };
  const failure = (command: FailableCommand): Promise<never> | undefined => {
    const error = failures.get(command);
    if (error === undefined) return undefined;
    failures.delete(command);
    return Promise.reject(error);
  };
  const emit = (event: HandoffEvent): void => {
    for (const handler of handlers) handler(event);
  };
  const settingsOf = (workspaceId: string): FolderHandoffSettings => settings.get(workspaceId) ?? EMPTY_SETTINGS;
  const answer = (workspaceId: string): HandoffSettingsAnswer => ({
    settings: structuredClone(settingsOf(workspaceId)),
    policy: structuredClone(policy),
    env_offered: policy.env_harvest && initial.envNotice === undefined ? envOffered.map((env) => ({ ...env })) : [],
    env_notice: policy.env_harvest ? (initial.envNotice ?? null) : null,
  });
  const statusOf = (uuid: string): HandoffStatus => ({ ...status, ...perThread.get(uuid) });

  /** The preview the host would answer: `leave_on_laptop` and the folder's settings applied. */
  const compute = (workspaceId: string, leave: readonly string[]): HandoffPreview => {
    const folder = settingsOf(workspaceId);
    const left = new Set(leave);
    const include = policy.allow_user_include ? folder.include : [];
    const excluded: HandoffPreview['excluded'] = [];
    const included: HandoffPreview['included'] = [];
    for (const entry of base.excluded) {
      if (left.has(entry.path)) {
        excluded.push({ ...entry, reason: 'leave_on_laptop', includable: false });
      } else if (entry.includable && policy.allow_user_include && matchesInclude(entry.path, include)) {
        included.push({ path: entry.path, reason: entry.reason });
      } else {
        excluded.push({ ...entry, includable: entry.includable && policy.allow_user_include });
      }
    }
    for (const path of leave) {
      if (!base.excluded.some((entry) => entry.path === path)) excluded.push({ path, reason: 'leave_on_laptop', tracked: true, includable: false });
    }
    const out = new Set(excluded.map((entry) => entry.path));
    const into = new Set(included.map((entry) => entry.path));
    const findings = base.findings.filter((finding) => !out.has(finding.path) && !into.has(finding.path));
    const includeFindings = [...base.include_findings, ...base.findings.filter((finding) => into.has(finding.path))];
    const env = policy.env_harvest ? previewEnv(envOffered, initial.envNotice, policy, folder.env_names) : null;
    return { ...structuredClone(base), excluded, included, findings, include_findings: includeFindings, policy: structuredClone(policy), env };
  };

  return {
    calls,
    emit,
    setAllowed: (next) => {
      allowed = next;
    },
    setPreview: (next) => {
      base = { ...base, ...next };
    },
    setPolicy: (next) => {
      policy = { ...policy, ...next };
    },
    setEnvOffered: (next) => {
      envOffered = next;
    },
    setSettings: (workspaceId, next) => {
      settings.set(workspaceId, { ...settingsOf(workspaceId), ...next });
    },
    setStatus: (next) => {
      status = { ...status, ...next };
    },
    setStatusOf: (uuid, next) => {
      perThread.set(uuid, { ...perThread.get(uuid), ...next });
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
      return failure('preview') ?? Promise.resolve(compute(workspaceId, leave));
    },
    async send(workspaceId, conversationUuid, leave, options) {
      const confirmIncludes = options?.confirmIncludes === true;
      calls.sends.push({ workspaceId, conversationUuid, leave: [...leave], confirmIncludes });
      const failed = failure('send');
      if (failed !== undefined) return failed;
      const now = compute(workspaceId, leave);
      if (now.findings.length > 0) throw new HandoffIpcError('secret_found', 'Secrets were found.', { findings: now.findings });
      if (now.include_findings.length > 0 && !confirmIncludes) {
        throw new HandoffIpcError('include_confirm_required', 'The included files need a confirmation.', { findings: now.include_findings });
      }
      if (now.env?.over_limit === true) throw new HandoffIpcError('env_too_large', 'Too many environment variables.');
      if (hold !== undefined) {
        const cancelled = new Promise<'cancelled'>((resolve) => {
          running = { uuid: conversationUuid, cancel: () => resolve('cancelled') };
        });
        const outcome = await Promise.race([hold.then(() => 'released' as const), cancelled]);
        running = undefined;
        if (outcome === 'cancelled') {
          emit({ conversation_uuid: conversationUuid, phase: 'cancelled', done: 0, total: 0, message: null });
          throw new HandoffIpcError('handoff_cancelled', 'The send was cancelled.');
        }
      }
      status = { ...status, state: 'CLOUD_IDLE', executor: 'cloud', read_only: true, sent_from_here: true, epoch: 2 };
      const sent: HandoffSent = {
        handoff_id: 'h-1',
        state: 'CLOUD_IDLE',
        epoch: 2,
        files: now.files,
        excluded: now.excluded.length,
        included: now.included.length,
        env_vars: now.env?.vars ?? 0,
      };
      return sent;
    },
    cancel(conversationUuid) {
      calls.cancels.push(conversationUuid);
      const failed = failure('cancel');
      if (failed !== undefined) return failed;
      if (running === undefined || running.uuid !== conversationUuid) return Promise.resolve({ cancelled: false });
      running.cancel();
      return Promise.resolve({ cancelled: true });
    },
    status(conversationUuid) {
      calls.statuses.push(conversationUuid);
      const failed = failure('status');
      if (failed !== undefined) return failed;
      const next = queue.shift();
      if (next !== undefined) status = { ...status, ...next };
      return Promise.resolve(statusOf(conversationUuid));
    },
    statusMany(conversationUuids) {
      calls.statusManys.push([...conversationUuids]);
      const failed = failure('statusMany');
      if (failed !== undefined) return failed;
      return Promise.resolve(Object.fromEntries(conversationUuids.map((uuid) => [uuid, statusOf(uuid)])));
    },
    reclaim(conversationUuid) {
      calls.reclaims.push(conversationUuid);
      return failure('reclaim') ?? Promise.resolve({ ...reclaim });
    },
    preflight(conversationUuid, upToTurn) {
      calls.preflights.push(conversationUuid);
      calls.preflightTurns.push(upToTurn);
      return failure('preflight') ?? Promise.resolve(structuredClone({ ...preflight, up_to_turn: upToTurn ?? preflight.cloud_turns }));
    },
    apply(preflightId, mode, upToTurn) {
      calls.applies.push(upToTurn === undefined ? { preflightId, mode } : { preflightId, mode, upToTurn });
      const { outcome, changed_paths, conflict_paths, skipped_paths, base, result_tree } = preflight;
      const report: ApplyOutcome = apply.report ?? { outcome, applied: apply.applied, changed_paths, conflict_paths, skipped_paths, base, result_tree };
      return failure('apply') ?? Promise.resolve(structuredClone({ ...apply, report }));
    },
    undo(checkpointId) {
      calls.undos.push(checkpointId);
      return failure('undo') ?? Promise.resolve({ restored: preflight.changed_paths.map((change) => change.path) });
    },
    erase(conversationUuid) {
      calls.erases.push(conversationUuid);
      const failed = failure('erase');
      if (failed !== undefined) return failed;
      status = { ...LOCAL_STATUS, epoch: status.epoch + 1 };
      perThread.delete(conversationUuid);
      return Promise.resolve({ state: 'LOCAL', epoch: status.epoch });
    },
    settingsGet(workspaceId) {
      calls.settingsGets.push(workspaceId);
      return failure('settingsGet') ?? Promise.resolve(answer(workspaceId));
    },
    settingsSet(workspaceId, next) {
      calls.settingsSets.push({ workspaceId, settings: structuredClone(next) });
      const failed = failure('settingsSet');
      if (failed !== undefined) return failed;
      const bad = (message: string, field: string, value: string): Promise<never> =>
        Promise.reject(new HandoffIpcError('invalid_request', message, { field, value }));
      for (const pattern of next.exclude_patterns) if (pattern.trim() === '') return bad('An exclusion pattern is empty.', 'exclude_patterns', pattern);
      if (next.include.length > 0 && !policy.allow_user_include) return Promise.reject(new HandoffIpcError('include_not_allowed', 'Including excluded files is not allowed.'));
      for (const path of next.include) {
        if (path.trim() === '' || path.split('/').includes('..')) return bad(`The include ${path} is outside the folder.`, 'include', path);
      }
      for (const name of next.env_names) if (!ENV_NAME.test(name)) return bad(`${name} is not a variable name.`, 'env_names', name);
      settings.set(workspaceId, structuredClone(next));
      return Promise.resolve(answer(workspaceId));
    },
    onEvent(handler) {
      handlers.add(handler);
      return Promise.resolve(() => {
        handlers.delete(handler);
      });
    },
  };
}
