import { act, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import {
  createArtifactBucket,
  fetchArtifacts,
  fetchArtifactBuckets,
  fetchArtifactStorageConfigurations,
  removeArtifact,
  removeArtifacts,
  removeArtifactBucket,
  setArtifactBucketPinned,
} from '../api/artifactsApi';
import * as artifactsApi from '../api/artifactsApi';
import { createTestQueryClient, renderHookWithProviders } from '../__tests__/testUtils';
import {
  artifactQueryKeys,
  useArtifactBuckets,
  useArtifactMutations,
  useArtifacts,
  useArtifactStorageConfigurations,
} from './useArtifacts';
import * as runtimeConfig from '@/shared/config';

beforeEach(() => {
  vi.restoreAllMocks();
  vi.spyOn(artifactsApi, 'createArtifactBucket');
  vi.spyOn(artifactsApi, 'fetchArtifacts');
  vi.spyOn(artifactsApi, 'fetchArtifactBuckets');
  vi.spyOn(artifactsApi, 'fetchArtifactStorageConfigurations');
  vi.spyOn(artifactsApi, 'removeArtifact');
  vi.spyOn(artifactsApi, 'removeArtifacts');
  vi.spyOn(artifactsApi, 'removeArtifactBucket');
  vi.spyOn(artifactsApi, 'setArtifactBucketPinned');
  vi.spyOn(runtimeConfig, 'getConfig').mockReturnValue({
    status: 'ok',
    config: {
      vite_server_url: '/api/v2',
      vite_base_uri: '/',
      vite_public_project_id: 'public',
      allow_project_own_llms: false,
    },
  });
});

describe('artifact query hooks', () => {
  it('uses stable project and bucket-scoped keys', () => {
    expect(artifactQueryKeys.buckets('p1')).toEqual(['artifacts', 'buckets', 'p1']);
    expect(artifactQueryKeys.files('p1', 'docs')).toEqual(['artifacts', 'files', 'p1', 'docs']);
    expect(artifactQueryKeys.storage('p1')).toEqual(['artifacts', 'storage', 'p1']);
  });

  it('loads buckets, files, and storage only when identifiers are available', async () => {
    vi.mocked(fetchArtifactBuckets).mockResolvedValue([]);
    vi.mocked(fetchArtifacts).mockResolvedValue([]);
    vi.mocked(fetchArtifactStorageConfigurations).mockResolvedValue([]);
    const bucketHook = renderHookWithProviders(() => useArtifactBuckets('p1'));
    const fileHook = renderHookWithProviders(() => useArtifacts('p1', 'docs'));
    const storageHook = renderHookWithProviders(() => useArtifactStorageConfigurations('p1'));
    await waitFor(() => expect(bucketHook.result.current.isSuccess).toBe(true));
    await waitFor(() => expect(fileHook.result.current.isSuccess).toBe(true));
    await waitFor(() => expect(storageHook.result.current.isSuccess).toBe(true));
    expect(fetchArtifactBuckets).toHaveBeenCalledWith('/api/v2', 'p1', expect.any(AbortSignal));
    expect(fetchArtifacts).toHaveBeenCalledWith('/api/v2', 'p1', 'docs', expect.any(AbortSignal));

    const disabled = renderHookWithProviders(() => useArtifacts(undefined, undefined));
    expect(disabled.result.current.fetchStatus).toBe('idle');
  });

  it('exposes all CRUD mutations and invalidates the affected scopes', async () => {
    vi.mocked(createArtifactBucket).mockResolvedValue();
    vi.mocked(setArtifactBucketPinned).mockResolvedValue();
    vi.mocked(removeArtifactBucket).mockResolvedValue();
    vi.mocked(removeArtifact).mockResolvedValue();
    vi.mocked(removeArtifacts).mockResolvedValue();
    const client = createTestQueryClient();
    client.setQueryData(artifactQueryKeys.buckets('p1'), []);
    client.setQueryData(artifactQueryKeys.files('p1', 'docs'), []);
    const hook = renderHookWithProviders(() => useArtifactMutations('p1'), client);

    await act(() => hook.result.current.createBucket.mutateAsync({ name: 'docs' }));
    await act(() => hook.result.current.pinBucket.mutateAsync({ name: 'reports', isPinned: true }));
    await act(() => hook.result.current.deleteBucket.mutateAsync('reports'));
    await act(() => hook.result.current.deleteFile.mutateAsync({ bucket: 'docs', key: 'a.txt' }));
    await act(() => hook.result.current.deleteMany.mutateAsync({ bucket: 'docs', keys: ['a.txt'] }));

    expect(createArtifactBucket).toHaveBeenCalledWith('p1', 'docs', undefined);
    expect(setArtifactBucketPinned).toHaveBeenCalledWith('p1', 'reports', true);
    expect(removeArtifactBucket).toHaveBeenCalledWith('p1', 'reports');
    expect(removeArtifact).toHaveBeenCalledWith('p1', 'docs', 'a.txt');
    expect(removeArtifacts).toHaveBeenCalledWith('p1', 'docs', ['a.txt']);
    expect(client.getQueryState(artifactQueryKeys.files('p1', 'docs'))).toBeUndefined();
  });

  it('rejects mutations when no project is selected', async () => {
    const hook = renderHookWithProviders(() => useArtifactMutations(undefined));
    await expect(hook.result.current.createBucket.mutateAsync({ name: 'docs' })).rejects.toThrow('project');
  });
});
