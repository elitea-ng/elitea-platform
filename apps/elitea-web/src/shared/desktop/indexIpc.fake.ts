/**
 * An in-memory `IndexIpc` for tests and the dev harness: the host's rules
 * (policy gate, `index_off`, cancel answering whether a refresh ran) over a
 * map of statuses. `emit` plays the host's side of `index://event`; `set`
 * puts an index in a given state without one.
 */
import type { EmbedCaps, EmbeddingsEvent, EmbedStatus, IndexEstimate, IndexEvent, IndexEventHandler, IndexIpc, IndexModels, IndexStatus, ModelEvent, ModelInfo, RemoteModel } from './indexIpc';
import { EMBEDDINGS_OFF, OFF_STATUS } from './indexIpc';
import { WorkspaceIpcError } from './workspaceIpc';

type FailableCommand = 'status' | 'open' | 'enable' | 'disable' | 'refresh' | 'cancel' | 'remove' | 'downloadModel' | 'importModel' | 'removeModel' | 'remoteModels' | 'setEmbeddings' | 'rebuildEmbeddings' | 'estimate' | 'setEmbeddingCaps';

/** The host's catalogue (sizes as `catalog.rs` pins them, rounded). */
const FAKE_MODELS: ModelInfo[] = [
  { id: 'qwen3-embedding-0.6b', display_name: 'Qwen3 Embedding 0.6B', summary: 'Better on code.', license: 'Apache-2.0', download_bytes: 1_200_000_000, ram_mb: 2600, dimension: 512, downloading: false, state: 'missing' },
  { id: 'multilingual-e5-small', display_name: 'multilingual-e5-small', summary: 'Small and fast.', license: 'MIT', download_bytes: 470_000_000, ram_mb: 1400, dimension: 384, downloading: false, state: 'missing' },
];

export interface FakeIndexIpc extends IndexIpc {
  /** Deliver an event to every subscriber (and record its status). */
  emit(event: IndexEvent): void;
  /** What `index_status` answers for a workspace from now on. */
  set(workspaceId: string, status: Partial<IndexStatus>): void;
  /** Turn the policy gate on or off (`local_index_disabled`). */
  setPolicyAllowed(allowed: boolean): void;
  /** Make the next call of `command` reject the way the host does. */
  failNext(command: FailableCommand, code: string, message: string): void;
  subscriberCount(): number;
  /** Deliver a model download's progress (and record it). */
  emitModel(event: ModelEvent): void;
  /** Deliver a workspace's embedding progress (and record it). */
  emitEmbeddings(event: EmbeddingsEvent): void;
  /** What `index_models` answers for one model from now on. */
  setModel(modelId: string, info: Partial<ModelInfo>): void;
  /** The next folder picker answers `null` (the person cancelled). */
  cancelNextPick(): void;
  readonly embeddingCalls: {
    downloads: string[];
    cancelled: string[];
    imported: string[];
    removed: string[];
    set: { workspaceId: string; choice: string }[];
    rebuilds: { workspaceId: string; full: boolean }[];
    estimates: { workspaceId: string; choice: string | undefined }[];
    caps: { workspaceId: string; caps: EmbedCaps }[];
  };
  readonly calls: { opened: string[]; enabled: string[]; disabled: string[]; refreshes: { workspaceId: string; full: boolean }[]; cancelled: string[]; removed: string[] };
}

export interface FakeIndexOptions {
  statuses?: Record<string, Partial<IndexStatus>>;
  /** `false`: the policy turns the index off. Default `true`. */
  policyAllowed?: boolean;
  /** Each workspace's `embeddings` setting (default `off`). */
  embeddings?: Record<string, string>;
  /** Default: local yes, remote no (the policy defaults). */
  embeddingsAllowed?: { local?: boolean; remote?: boolean };
  /** What `index_remote_models` answers (default none). */
  remoteModels?: RemoteModel[];
  /** The policy's download mirror host (default none). */
  mirror?: string;
  /** What `index_estimate` answers (default: 4 000 chunks, 120 000 tokens, the default caps, nothing known of the budget). */
  estimate?: Partial<IndexEstimate>;
}

export function createFakeIndexIpc(options: FakeIndexOptions = {}): FakeIndexIpc {
  const statuses = new Map<string, IndexStatus>(
    Object.entries(options.statuses ?? {}).map(([id, status]) => [id, { ...OFF_STATUS, on_disk: status.state !== undefined && status.state !== 'off', ...status }]),
  );
  const handlers = new Set<IndexEventHandler>();
  const failures = new Map<FailableCommand, WorkspaceIpcError>();
  const calls: FakeIndexIpc['calls'] = { opened: [], enabled: [], disabled: [], refreshes: [], cancelled: [], removed: [] };
  let policyAllowed = options.policyAllowed ?? true;
  const models = new Map<string, ModelInfo>(FAKE_MODELS.map((model) => [model.id, { ...model }]));
  const choices = new Map<string, string>(Object.entries(options.embeddings ?? {}));
  const embedStatuses = new Map<string, EmbedStatus>();
  const allowed = { local: options.embeddingsAllowed?.local ?? true, remote: options.embeddingsAllowed?.remote ?? false };
  const caps = new Map<string, EmbedCaps>();
  const modelHandlers = new Set<(event: ModelEvent) => void>();
  const embeddingHandlers = new Set<(event: EmbeddingsEvent) => void>();
  const embeddingCalls: FakeIndexIpc['embeddingCalls'] = { downloads: [], cancelled: [], imported: [], removed: [], set: [], rebuilds: [], estimates: [], caps: [] };
  let cancelPick = false;
  const subscribe = <T>(set: Set<(event: T) => void>, handler: (event: T) => void): Promise<() => void> => {
    set.add(handler);
    return Promise.resolve(() => {
      set.delete(handler);
    });
  };
  const reject = (code: string, message: string): Promise<never> => Promise.reject(new WorkspaceIpcError(code, message));
  const model = (id: string): ModelInfo | undefined => models.get(id);
  const setModel = (id: string, info: Partial<ModelInfo>): ModelInfo => {
    const next = { ...model(id)!, ...info } as ModelInfo;
    models.set(id, next);
    return { ...next };
  };
  const cached = (info: ModelInfo): number => (info.state === 'ready' ? info.download_bytes : info.state === 'partial' ? info.have : 0);

  const current = (id: string): IndexStatus => statuses.get(id) ?? { ...OFF_STATUS };
  const failure = (command: FailableCommand): Promise<never> | undefined => {
    const error = failures.get(command);
    if (error === undefined) return undefined;
    failures.delete(command);
    return Promise.reject(error);
  };
  const gate = (command: FailableCommand): Promise<never> | undefined =>
    failure(command) ??
    (policyAllowed ? undefined : Promise.reject(new WorkspaceIpcError('local_index_disabled', 'The local index is turned off by your organisation’s policy.')));

  return {
    calls,
    status(workspaceId) {
      const failed = failure('status');
      if (failed !== undefined) return failed;
      // The policy off is a status, not an error: the person may still turn the index off or remove it.
      if (!policyAllowed) return Promise.resolve({ ...OFF_STATUS, policy_off: true, on_disk: current(workspaceId).on_disk });
      return Promise.resolve({ ...current(workspaceId) });
    },
    open(workspaceId) {
      calls.opened.push(workspaceId);
      const refused = gate('open');
      if (refused !== undefined) return refused;
      // Opening checks the folder: an index that is on starts a refresh.
      const status = current(workspaceId);
      if (status.state === 'stale' || status.state === 'stale_policy') statuses.set(workspaceId, { ...status, state: 'building' });
      return Promise.resolve({ ...current(workspaceId) });
    },
    enable(workspaceId) {
      calls.enabled.push(workspaceId);
      const refused = gate('enable');
      if (refused !== undefined) return refused;
      statuses.set(workspaceId, { ...current(workspaceId), state: 'building', error: null, on_disk: true });
      return Promise.resolve({ ...current(workspaceId) });
    },
    disable(workspaceId) {
      calls.disabled.push(workspaceId);
      const failed = failure('disable');
      if (failed !== undefined) return failed;
      // The build is kept.
      statuses.set(workspaceId, { ...OFF_STATUS, on_disk: current(workspaceId).on_disk });
      return Promise.resolve({ ...current(workspaceId), policy_off: !policyAllowed });
    },
    refresh(workspaceId, full) {
      calls.refreshes.push({ workspaceId, full: full === true });
      const refused = gate('refresh');
      if (refused !== undefined) return refused;
      if (current(workspaceId).state === 'off') return Promise.reject(new WorkspaceIpcError('index_off', 'Turn the index on for this workspace first.'));
      statuses.set(workspaceId, { ...current(workspaceId), state: 'building' });
      return Promise.resolve({ ...current(workspaceId) });
    },
    cancel(workspaceId) {
      calls.cancelled.push(workspaceId);
      const failed = failure('cancel');
      if (failed !== undefined) return failed;
      const status = current(workspaceId);
      if (status.state !== 'building') return Promise.resolve(false);
      statuses.set(workspaceId, { ...status, state: status.last_run === null ? 'error' : 'stale' });
      return Promise.resolve(true);
    },
    remove(workspaceId) {
      calls.removed.push(workspaceId);
      const failed = failure('remove');
      if (failed !== undefined) return failed;
      statuses.delete(workspaceId);
      return Promise.resolve();
    },
    onEvent(handler) {
      handlers.add(handler);
      return Promise.resolve(() => {
        handlers.delete(handler);
      });
    },
    emit(event) {
      statuses.set(event.workspace_id, { ...event.status });
      Array.from(handlers).forEach((handler) => handler(event));
    },
    set(workspaceId, status) {
      statuses.set(workspaceId, { ...current(workspaceId), ...status });
    },
    setPolicyAllowed(allowed) {
      policyAllowed = allowed;
    },
    failNext(command, code, message) {
      failures.set(command, new WorkspaceIpcError(code, message));
    },
    subscriberCount: () => handlers.size,
    models() {
      const list = Array.from(models.values(), (info) => ({ ...info }));
      const answer: IndexModels = {
        models: list,
        local_allowed: allowed.local,
        remote_allowed: allowed.remote,
        cache_bytes: list.reduce((sum, info) => sum + cached(info), 0),
        mirror: options.mirror ?? null,
      };
      return Promise.resolve(answer);
    },
    downloadModel(modelId) {
      embeddingCalls.downloads.push(modelId);
      const failed = failure('downloadModel');
      if (failed !== undefined) return failed;
      const info = model(modelId);
      if (info === undefined) return reject('model_unknown', 'No such model.');
      if (!allowed.local) return reject('embeddings_not_allowed', 'Your organization’s policy does not allow embedding on this computer.');
      if (info.downloading) return reject('model_busy', 'It is downloading already.');
      return Promise.resolve(setModel(modelId, { downloading: info.state !== 'ready' }));
    },
    cancelModel(modelId) {
      embeddingCalls.cancelled.push(modelId);
      if (model(modelId)?.downloading !== true) return Promise.resolve(false);
      setModel(modelId, { downloading: false });
      return Promise.resolve(true);
    },
    importModel(modelId) {
      const failed = failure('importModel');
      if (failed !== undefined) return failed;
      if (model(modelId) === undefined) return reject('model_unknown', 'No such model.');
      if (cancelPick) {
        cancelPick = false;
        return Promise.resolve(null);
      }
      embeddingCalls.imported.push(modelId);
      return Promise.resolve(setModel(modelId, { state: 'ready', downloading: false }));
    },
    removeModel(modelId) {
      const failed = failure('removeModel');
      if (failed !== undefined) return failed;
      if (model(modelId) === undefined) return reject('model_unknown', 'No such model.');
      embeddingCalls.removed.push(modelId);
      return Promise.resolve(setModel(modelId, { state: 'missing', downloading: false }));
    },
    remoteModels() {
      const failed = failure('remoteModels');
      if (failed !== undefined) return failed;
      if (!allowed.remote) return reject('embeddings_not_allowed', 'Your organization’s policy does not allow embeddings through the deployment.');
      return Promise.resolve((options.remoteModels ?? []).map((item) => ({ ...item })));
    },
    cancelNextPick() {
      cancelPick = true;
    },
    embeddings: (workspaceId) => Promise.resolve(choices.get(workspaceId) ?? EMBEDDINGS_OFF),
    setEmbeddings(workspaceId, choice) {
      embeddingCalls.set.push({ workspaceId, choice });
      const failed = failure('setEmbeddings');
      if (failed !== undefined) return failed;
      if (current(workspaceId).state === 'off') return reject('index_off', 'Turn the index on for this workspace first.');
      let stored = choice;
      if (choice.startsWith('local:')) {
        if (!allowed.local) return reject('embeddings_not_allowed', 'Your organization’s policy does not allow embedding on this computer.');
        if (!models.has(choice.slice('local:'.length))) return reject('embeddings_unknown', 'That is not an embeddings choice.');
      } else if (choice.startsWith('remote:')) {
        if (!allowed.remote) return reject('embeddings_not_allowed', 'Your organization’s policy does not allow embeddings through the deployment.');
        // The host probes the width.
        if (!/^remote:\d+:/.test(choice)) stored = `remote:768:${choice.slice('remote:'.length)}`;
      } else if (choice !== EMBEDDINGS_OFF) {
        return reject('embeddings_unknown', 'That is not an embeddings choice.');
      }
      choices.set(workspaceId, stored);
      if (stored === EMBEDDINGS_OFF) embedStatuses.delete(workspaceId);
      return Promise.resolve(stored);
    },
    embeddingsStatus: (workspaceId) => Promise.resolve(embedStatuses.get(workspaceId) ?? null),
    rebuildEmbeddings(workspaceId, full) {
      embeddingCalls.rebuilds.push({ workspaceId, full: full === true });
      const failed = failure('rebuildEmbeddings');
      if (failed !== undefined) return failed;
      if ((choices.get(workspaceId) ?? EMBEDDINGS_OFF) === EMBEDDINGS_OFF) return reject('embeddings_off', 'Choose how this folder is embedded first.');
      return Promise.resolve();
    },
    estimate(workspaceId, choice) {
      embeddingCalls.estimates.push({ workspaceId, choice });
      const failed = failure('estimate');
      if (failed !== undefined) return failed;
      const asked = choice ?? choices.get(workspaceId) ?? EMBEDDINGS_OFF;
      if (asked === EMBEDDINGS_OFF) return reject('embeddings_off', 'Choose how to embed this workspace to see an estimate.');
      const remote = asked.startsWith('remote:');
      const kept = caps.get(workspaceId) ?? { run: 2_000_000, day: 200_000 };
      return Promise.resolve({
        origin: remote ? 'remote' : 'local',
        model: asked.split(':').at(-1) ?? asked,
        texts: 4_000,
        tokens: 120_000,
        caps: kept,
        day_used: 0,
        days_needed: remote ? 1 : null,
        project_budget: null,
        price_per_1m_usd: null,
        cost_usd: null,
        ...options.estimate,
      });
    },
    setEmbeddingCaps(workspaceId, next) {
      embeddingCalls.caps.push({ workspaceId, caps: next });
      const failed = failure('setEmbeddingCaps');
      if (failed !== undefined) return failed;
      if (next.run < 1 || next.day < 1) return reject('embed_caps_invalid', 'A cap must be between 1 and 10 000 000 000 tokens.');
      caps.set(workspaceId, { ...next });
      return Promise.resolve({ ...next });
    },
    onModelEvent: (handler) => subscribe(modelHandlers, handler),
    onEmbeddingsEvent: (handler) => subscribe(embeddingHandlers, handler),
    emitModel(event) {
      if (model(event.model) !== undefined) {
        const state: Partial<ModelInfo> = event.phase === 'ready' ? { state: 'ready' } : event.done > 0 ? { state: 'partial', have: event.done, total: event.total } : {};
        setModel(event.model, { ...state, downloading: event.phase === 'downloading' });
      }
      Array.from(modelHandlers).forEach((handler) => handler(event));
    },
    emitEmbeddings(event) {
      embedStatuses.set(event.workspace_id, { ...event.status });
      Array.from(embeddingHandlers).forEach((handler) => handler(event));
    },
    setModel(modelId, info) {
      if (model(modelId) !== undefined) setModel(modelId, info);
    },
    embeddingCalls,
  };
}
