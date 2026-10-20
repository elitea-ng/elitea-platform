/**
 * The typed client for the desktop host's local workspace index (IPC.md,
 * "Local index"; ADR-0029 decision 7): the `index_*` commands and the
 * `index://event` progress channel. Wire shapes are the host's (snake_case)
 * and are not renamed here. Failures reject with a `WorkspaceIpcError`
 * (`local_index_disabled`, `index_busy`, `index_refused`, …).
 */
import type { HostInvoke } from './hostBridge';
import { tauriListen, toWorkspaceIpcError, type ListenFn, type TauriEventInternals } from './workspaceIpc';

/**
 * `stale`: files may have changed since the last build (or it was not checked
 * since the app started). `stale_policy`: the build was made under another
 * file access policy (`path_deny`); agents are not offered it until the
 * rebuild under the current one finishes.
 */
export type IndexState = 'off' | 'building' | 'ready' | 'stale' | 'stale_policy' | 'error';

export interface IndexStatus {
  state: IndexState;
  /** Documents of the last build. */
  files: number;
  entities: number;
  relations: number;
  /** 0 until embeddings exist. */
  vectors: number;
  /** When the last build was committed (UTC, ISO 8601). */
  last_run: string | null;
  /** Files turns changed since the last build. */
  changed_files: number;
  /** Why the last refresh failed. */
  error: string | null;
  /** The policy turns the index off (`state` is `off`); turning it off and removing it stay allowed. */
  policy_off: boolean;
  /** An index exists on this computer. */
  on_disk: boolean;
}

/** `queued`: waiting for another folder's refresh (one runs at a time). */
export type IndexPhase = 'queued' | 'listing' | 'parsing' | 'building' | 'saving' | 'ready' | 'stale' | 'cancelled' | 'error';

export interface IndexEvent {
  workspace_id: string;
  phase: IndexPhase;
  /** Progress text, or why it failed. */
  message: string | null;
  /** As `index_status` would answer now. */
  status: IndexStatus;
}

export type IndexEventHandler = (event: IndexEvent) => void;

/**
 * A workspace's `embeddings` setting: `off` (the default), `local:<model id>`
 * (a catalogue model, on this computer), or `remote:<dim>:<model>` (the
 * deployment's `/llm/v1/embeddings`; `index_embeddings_set` takes
 * `remote:<model>` and probes the width).
 */
export type EmbeddingsChoice = string;

export const EMBEDDINGS_OFF: EmbeddingsChoice = 'off';

/** A catalogue model and how much of it is on this computer. */
export type ModelInfo = {
  id: string;
  display_name: string;
  summary: string;
  license: string;
  /** Bytes to download. */
  download_bytes: number;
  /** Memory while it embeds, roughly. */
  ram_mb: number;
  dimension: number;
  /** A download runs. */
  downloading: boolean;
} & ({ state: 'missing' } | { state: 'partial'; have: number; total: number } | { state: 'ready' });

export interface IndexModels {
  models: ModelInfo[];
  /** The policy lets the person embed on this computer / through the deployment. */
  local_allowed: boolean;
  remote_allowed: boolean;
  /** Bytes the model cache holds. */
  cache_bytes: number;
}

/** `paused`: a turn runs, or `held` says why (`turn`, `on_battery`, `low_power`). */
export type EmbedState = 'waiting' | 'running' | 'paused' | 'done' | 'error';

export interface EmbedStatus {
  choice: EmbeddingsChoice;
  state: EmbedState;
  /** Distinct chunk texts with a vector, of how many. */
  done: number;
  total: number;
  held: 'turn' | 'on_battery' | 'low_power' | null;
  error: string | null;
  /** Asked for whatever the power conditions. */
  forced: boolean;
}

/** A model download's progress (`index://event`, `kind: model`). */
export interface ModelEvent {
  kind: 'model';
  model: string;
  phase: 'downloading' | 'ready' | 'cancelled' | 'failed';
  done: number;
  total: number;
  error: string | null;
}

/** A workspace's embedding progress (`index://event`, `kind: embeddings`). */
export interface EmbeddingsEvent {
  kind: 'embeddings';
  workspace_id: string;
  status: EmbedStatus;
}

export interface IndexIpc {
  /** Where the index is, from a small read; never opens it or starts a refresh. */
  status(workspaceId: string): Promise<IndexStatus>;
  /** Open the index (the folder's session page is open) and check the folder for changes. */
  open(workspaceId: string): Promise<IndexStatus>;
  /** Turn the index on and start building it. */
  enable(workspaceId: string): Promise<IndexStatus>;
  /** Stop building and stop offering the index; the build is kept. */
  disable(workspaceId: string): Promise<IndexStatus>;
  /** Start a refresh (only what changed is read again); `full` rebuilds from nothing. */
  refresh(workspaceId: string, full?: boolean): Promise<IndexStatus>;
  /** Stop the running refresh; `false` when none runs. */
  cancel(workspaceId: string): Promise<boolean>;
  /** Delete the index (the person confirmed). */
  remove(workspaceId: string): Promise<void>;
  /** Subscribe to `index://event`; resolves once the subscription is live, with its unsubscribe. */
  onEvent(handler: IndexEventHandler): Promise<() => void>;
  /** The embedding models, their state here, and what the policy allows. */
  models(): Promise<IndexModels>;
  /** Start (or resume) a download; the person saw its size. */
  downloadModel(modelId: string): Promise<ModelInfo>;
  /** Stop a running download (what was fetched is kept); `false` when none runs. */
  cancelModel(modelId: string): Promise<boolean>;
  /** Copy a model's files from a folder (no download). */
  importModel(modelId: string, folder: string): Promise<ModelInfo>;
  /** Delete a downloaded model. */
  removeModel(modelId: string): Promise<ModelInfo>;
  embeddings(workspaceId: string): Promise<EmbeddingsChoice>;
  /** Answers the stored value (`remote:<model>` comes back as `remote:<dim>:<model>`). */
  setEmbeddings(workspaceId: string, choice: EmbeddingsChoice): Promise<EmbeddingsChoice>;
  /** `null` when the workspace does not embed (or its index is not open). */
  embeddingsStatus(workspaceId: string): Promise<EmbedStatus | null>;
  /** Embed now whatever the power state; `full` drops the vectors first. */
  rebuildEmbeddings(workspaceId: string, full?: boolean): Promise<void>;
  onModelEvent(handler: (event: ModelEvent) => void): Promise<() => void>;
  onEmbeddingsEvent(handler: (event: EmbeddingsEvent) => void): Promise<() => void>;
}

export const INDEX_EVENT_CHANNEL = 'index://event';

/** The status of an index that is not turned on (what `index_disable` answers). */
export const OFF_STATUS: IndexStatus = Object.freeze({
  state: 'off',
  files: 0,
  entities: 0,
  relations: 0,
  vectors: 0,
  last_run: null,
  changed_files: 0,
  error: null,
  policy_off: false,
  on_disk: false,
});

/** `index://event` carries three payloads: a build's (no `kind`), a model download's and a workspace's embedding. */
function kindOf(payload: unknown): unknown {
  return typeof payload === 'object' && payload !== null ? (payload as { kind?: unknown }).kind : undefined;
}

export function createIndexIpc(hostInvoke: HostInvoke, listen: ListenFn): IndexIpc {
  const invoke = <T>(command: string, args: Record<string, unknown>): Promise<T> =>
    hostInvoke<T>(command, args).catch((error: unknown) => {
      throw toWorkspaceIpcError(error);
    });
  return {
    status: (workspaceId) => invoke<IndexStatus>('index_status', { workspace_id: workspaceId }),
    open: (workspaceId) => invoke<IndexStatus>('index_open', { workspace_id: workspaceId }),
    enable: (workspaceId) => invoke<IndexStatus>('index_enable', { workspace_id: workspaceId }),
    disable: (workspaceId) => invoke<IndexStatus>('index_disable', { workspace_id: workspaceId }),
    refresh: (workspaceId, full) =>
      invoke<IndexStatus>('index_refresh', full === true ? { workspace_id: workspaceId, full: true } : { workspace_id: workspaceId }),
    cancel: (workspaceId) => invoke<boolean>('index_cancel', { workspace_id: workspaceId }),
    remove: (workspaceId) => invoke<void>('index_remove', { workspace_id: workspaceId, confirm: true }),
    onEvent: (handler) =>
      listen(INDEX_EVENT_CHANNEL, (payload) => {
        if (kindOf(payload) === undefined) handler(payload as IndexEvent);
      }),
    models: () => invoke<IndexModels>('index_models', {}),
    downloadModel: (modelId) => invoke<ModelInfo>('index_model_download', { model_id: modelId, confirm: true }),
    cancelModel: (modelId) => invoke<boolean>('index_model_cancel', { model_id: modelId }),
    importModel: (modelId, folder) => invoke<ModelInfo>('index_model_import', { model_id: modelId, folder }),
    removeModel: (modelId) => invoke<ModelInfo>('index_model_remove', { model_id: modelId }),
    embeddings: (workspaceId) => invoke<EmbeddingsChoice>('index_embeddings_get', { workspace_id: workspaceId }),
    setEmbeddings: (workspaceId, choice) => invoke<EmbeddingsChoice>('index_embeddings_set', { workspace_id: workspaceId, embeddings: choice }),
    embeddingsStatus: (workspaceId) => invoke<EmbedStatus | null>('index_embeddings_status', { workspace_id: workspaceId }),
    rebuildEmbeddings: (workspaceId, full) =>
      invoke<void>('index_embeddings_rebuild', full === true ? { workspace_id: workspaceId, full: true } : { workspace_id: workspaceId }),
    onModelEvent: (handler) =>
      listen(INDEX_EVENT_CHANNEL, (payload) => {
        if (kindOf(payload) === 'model') handler(payload as ModelEvent);
      }),
    onEmbeddingsEvent: (handler) =>
      listen(INDEX_EVENT_CHANNEL, (payload) => {
        if (kindOf(payload) === 'embeddings') handler(payload as EmbeddingsEvent);
      }),
  };
}

/** The real client, or `undefined` outside the Tauri webview. */
export function tauriIndexIpc(): IndexIpc | undefined {
  const internals = (globalThis as { __TAURI_INTERNALS__?: Partial<TauriEventInternals> }).__TAURI_INTERNALS__;
  if (internals?.invoke === undefined || internals.transformCallback === undefined) return undefined;
  const bound: TauriEventInternals = {
    invoke: internals.invoke.bind(internals),
    transformCallback: internals.transformCallback.bind(internals),
  };
  return createIndexIpc(bound.invoke, tauriListen(bound));
}
