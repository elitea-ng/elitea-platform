/**
 * A workspace's semantic search: the embedding models (shared by every
 * folder), the folder's `embeddings` setting (`off` until the person
 * chooses), its embedding progress, and the actions the settings dialog
 * offers. Nothing is downloaded until the person picks a model and agrees to
 * its size; a chosen model that is not here yet is downloaded first and
 * applied to the folder once it is ready.
 */
import { useEffect, useState } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { EMBEDDINGS_OFF, type EmbeddingsChoice, type EmbedStatus, type IndexIpc, type IndexModels, type RemoteModel } from '@/shared/desktop/indexIpc';

export const LOCAL_PREFIX = 'local:';
export const REMOTE_PREFIX = 'remote:';

/** Above this many chunks the smaller model is recommended (a first pass of the larger one takes hours on a laptop). */
export const LARGE_INDEX_CHUNKS = 50_000;
export const SMALL_MODEL = 'multilingual-e5-small';
export const CODE_MODEL = 'qwen3-embedding-0.6b';

/** The model the settings recommend for an index of `chunks` chunks (the person still picks). */
export function recommendedModel(chunks: number): string {
  return chunks > LARGE_INDEX_CHUNKS ? SMALL_MODEL : CODE_MODEL;
}

/** A download's bytes so far, by model id (from `index://event`). */
export type DownloadProgress = Record<string, { done: number; total: number }>;

export type EmbeddingsAction =
  | { kind: 'choose'; choice: EmbeddingsChoice }
  | { kind: 'download'; modelId: string }
  | { kind: 'cancelDownload'; modelId: string }
  | { kind: 'import'; modelId: string }
  | { kind: 'remove'; modelId: string }
  | { kind: 'rebuild'; full: boolean };

export interface WorkspaceEmbeddings {
  models: IndexModels | undefined;
  choice: EmbeddingsChoice | undefined;
  status: EmbedStatus | null;
  downloads: DownloadProgress;
  /** A model the person chose that downloads first; applied once ready. */
  awaiting: string | null;
  /** The last download that failed, and why. */
  downloadError: { modelId: string; error: string } | null;
  /** The project's embedding models on the deployment (`undefined`: not listed — the person types a name). */
  remoteModels: RemoteModel[] | undefined;
  run: (action: EmbeddingsAction) => void;
  pending: boolean;
  actionError: unknown;
}

const modelsKey = ['workspace', 'index', 'models'] as const;
const choiceKey = (workspaceId: string) => ['workspace', 'index', workspaceId, 'embeddings'] as const;
const statusKey = (workspaceId: string) => ['workspace', 'index', workspaceId, 'embeddings-status'] as const;
const remoteModelsKey = (workspaceId: string) => ['workspace', 'index', workspaceId, 'remote-models'] as const;

/** The local model a choice names, if it is one. */
export function localModelOf(choice: EmbeddingsChoice | undefined): string | null {
  return choice?.startsWith(LOCAL_PREFIX) === true ? choice.slice(LOCAL_PREFIX.length) : null;
}

/** The remote model a stored choice (`remote:<dim>:<model>`) names, if it is one. */
export function remoteModelOf(choice: EmbeddingsChoice | undefined): string | null {
  if (choice?.startsWith(REMOTE_PREFIX) !== true) return null;
  const rest = choice.slice(REMOTE_PREFIX.length);
  const match = /^\d+:(.+)$/.exec(rest);
  return match?.[1] ?? rest;
}

/** `enabled`: the folder's index is on (the dialog's embeddings section shows). */
export function useEmbeddings(ipc: IndexIpc, workspaceId: string, enabled: boolean): WorkspaceEmbeddings {
  const queryClient = useQueryClient();
  const models = useQuery({
    queryKey: modelsKey,
    queryFn: () => ipc.models(),
    enabled,
  });
  const choice = useQuery({
    queryKey: choiceKey(workspaceId),
    queryFn: () => ipc.embeddings(workspaceId),
    enabled,
  });
  const status = useQuery({
    queryKey: statusKey(workspaceId),
    queryFn: () => ipc.embeddingsStatus(workspaceId),
    enabled,
  });
  const remoteModels = useQuery({
    queryKey: remoteModelsKey(workspaceId),
    queryFn: () => ipc.remoteModels(workspaceId),
    enabled: enabled && models.data?.remote_allowed === true,
    retry: false,
  });
  const [downloads, setDownloads] = useState<DownloadProgress>({});
  const [awaiting, setAwaiting] = useState<string | null>(null);
  const [downloadError, setDownloadError] = useState<WorkspaceEmbeddings['downloadError']>(null);

  const apply = useMutation({
    mutationFn: (next: EmbeddingsChoice) => ipc.setEmbeddings(workspaceId, next),
    onSuccess: (stored) => {
      queryClient.setQueryData(choiceKey(workspaceId), stored);
      void queryClient.invalidateQueries({ queryKey: statusKey(workspaceId) });
    },
  });

  useEffect(() => {
    if (!enabled) return undefined;
    const offs: (() => void)[] = [];
    let disposed = false;
    const keep = (off: () => void): void => {
      if (disposed) off();
      else offs.push(off);
    };
    void ipc
      .onModelEvent((event) => {
        if (event.phase === 'downloading') {
          setDownloads((previous) => ({
            ...previous,
            [event.model]: { done: event.done, total: event.total },
          }));
          return;
        }
        setDownloads(({ [event.model]: _finished, ...rest }) => rest);
        if (event.phase === 'failed') setDownloadError({ modelId: event.model, error: event.error ?? '' });
        void queryClient.invalidateQueries({ queryKey: modelsKey });
      })
      .then(keep)
      .catch(() => undefined);
    void ipc
      .onEmbeddingsEvent((event) => {
        if (event.workspace_id === workspaceId) queryClient.setQueryData(statusKey(workspaceId), event.status);
      })
      .then(keep)
      .catch(() => undefined);
    return () => {
      disposed = true;
      offs.forEach((off) => off());
    };
  }, [ipc, workspaceId, enabled, queryClient]);

  // The model the person chose finished downloading: use it for this folder.
  const awaitedReady = awaiting !== null && models.data?.models.some((model) => model.id === awaiting && model.state === 'ready') === true;
  const applyChoice = apply.mutate;
  useEffect(() => {
    if (!awaitedReady || awaiting === null) return;
    setAwaiting(null);
    applyChoice(`${LOCAL_PREFIX}${awaiting}`);
  }, [awaitedReady, awaiting, applyChoice]);

  /** Import a model's files (the host opens the folder picker; `null`: the person cancelled) or remove them. */
  const manageFiles = async (next: { kind: 'import' | 'remove'; modelId: string }): Promise<void> => {
    if (next.kind === 'import') {
      if ((await ipc.importModel(next.modelId)) === null) return;
    } else {
      if (awaiting === next.modelId) setAwaiting(null);
      await ipc.removeModel(next.modelId);
    }
    await queryClient.invalidateQueries({ queryKey: modelsKey });
  };

  const action = useMutation({
    mutationFn: async (next: EmbeddingsAction): Promise<void> => {
      switch (next.kind) {
        case 'choose': {
          const model = localModelOf(next.choice);
          const info = model === null ? undefined : models.data?.models.find((candidate) => candidate.id === model);
          if (info !== undefined && info.state !== 'ready') {
            // Not here yet: the dialog asks before downloading.
            setAwaiting(info.id);
            return;
          }
          setAwaiting(null);
          await apply.mutateAsync(next.choice);
          return;
        }
        case 'download':
          setDownloadError(null);
          setAwaiting(next.modelId);
          await ipc.downloadModel(next.modelId);
          await queryClient.invalidateQueries({ queryKey: modelsKey });
          return;
        case 'cancelDownload':
          await ipc.cancelModel(next.modelId);
          setAwaiting(null);
          await queryClient.invalidateQueries({ queryKey: modelsKey });
          return;
        case 'import':
        case 'remove':
          await manageFiles(next);
          return;
        case 'rebuild':
          await ipc.rebuildEmbeddings(workspaceId, next.full);
          await queryClient.invalidateQueries({
            queryKey: statusKey(workspaceId),
          });
          return;
      }
    },
  });

  return {
    models: models.data,
    choice: enabled ? choice.data : EMBEDDINGS_OFF,
    status: status.data ?? null,
    downloads,
    awaiting,
    downloadError,
    remoteModels: remoteModels.data !== undefined && remoteModels.data.length > 0 ? remoteModels.data : undefined,
    run: action.mutate,
    pending: action.isPending || apply.isPending,
    actionError: action.error ?? apply.error,
  };
}
