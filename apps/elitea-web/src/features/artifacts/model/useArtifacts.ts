import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { getConfig } from '@/shared/config';

import {
  createArtifactBucket,
  fetchArtifacts,
  fetchArtifactBuckets,
  fetchArtifactStorageConfigurations,
  removeArtifact,
  removeArtifacts,
  removeArtifactBucket,
  setArtifactBucketPinned,
  setArtifactBucketRetention,
} from '../api/artifactsApi';

export const artifactQueryKeys = {
  all: ['artifacts'] as const,
  buckets: (projectId: string) => [...artifactQueryKeys.all, 'buckets', projectId] as const,
  files: (projectId: string, bucket: string) => [...artifactQueryKeys.all, 'files', projectId, bucket] as const,
  storage: (projectId: string) => [...artifactQueryKeys.all, 'storage', projectId] as const,
};

function resolvedBaseUrl(): string {
  const config = getConfig();
  if (config.status !== 'ok') throw new Error('Runtime configuration is unavailable.');
  return config.config.vite_server_url;
}

export function useArtifactBuckets(projectId: string | undefined) {
  return useQuery({
    queryKey: artifactQueryKeys.buckets(projectId ?? ''),
    queryFn: ({ signal }) => fetchArtifactBuckets(resolvedBaseUrl(), projectId ?? '', signal),
    enabled: projectId !== undefined && projectId !== '',
  });
}

export function useArtifacts(projectId: string | undefined, bucket: string | undefined) {
  return useQuery({
    queryKey: artifactQueryKeys.files(projectId ?? '', bucket ?? ''),
    queryFn: ({ signal }) => fetchArtifacts(resolvedBaseUrl(), projectId ?? '', bucket ?? '', signal),
    enabled: projectId !== undefined && projectId !== '' && bucket !== undefined && bucket !== '',
  });
}

export function useArtifactStorageConfigurations(projectId: string | undefined) {
  return useQuery({
    queryKey: artifactQueryKeys.storage(projectId ?? ''),
    queryFn: ({ signal }) => fetchArtifactStorageConfigurations(projectId ?? '', signal),
    enabled: projectId !== undefined && projectId !== '',
  });
}

export function useArtifactMutations(projectId: string | undefined) {
  const queryClient = useQueryClient();
  const requiredProjectId = (): string => {
    if (projectId === undefined || projectId === '') throw new Error('A project must be selected.');
    return projectId;
  };
  const refreshBuckets = () => queryClient.invalidateQueries({ queryKey: artifactQueryKeys.buckets(requiredProjectId()) });
  const refreshFiles = (bucket: string) =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: artifactQueryKeys.files(requiredProjectId(), bucket) }),
      queryClient.invalidateQueries({ queryKey: artifactQueryKeys.buckets(requiredProjectId()) }),
    ]);

  const createBucket = useMutation({
    mutationFn: ({ name, retentionDays }: { readonly name: string; readonly retentionDays?: number | null }) =>
      createArtifactBucket(requiredProjectId(), name, retentionDays),
    onSuccess: refreshBuckets,
  });
  const pinBucket = useMutation({
    mutationFn: ({ name, isPinned }: { readonly name: string; readonly isPinned: boolean }) =>
      setArtifactBucketPinned(requiredProjectId(), name, isPinned),
    onSuccess: refreshBuckets,
  });
  /**
   * Bucket EDIT. `setArtifactBucketRetention` existed with a unit test and
   * zero production callers — the dead-export class this branch is clearing
   * — because no screen ever offered a bucket-edit affordance. It is the
   * whole of "edit a bucket": the API has no rename (S3 buckets cannot be
   * renamed in place, and neither `PUT /buckets/{name}` nor the legacy
   * `editBucket` it replaced ever renamed one — the legacy call only
   * reconfigured the lifecycle, `expiration_measure`/`expiration_value`).
   */
  const editBucketRetention = useMutation({
    mutationFn: ({ name, retentionDays }: { readonly name: string; readonly retentionDays: number | null }) =>
      setArtifactBucketRetention(requiredProjectId(), name, retentionDays),
    onSuccess: refreshBuckets,
  });
  const deleteBucket = useMutation({
    mutationFn: (name: string) => removeArtifactBucket(requiredProjectId(), name),
    onSuccess: refreshBuckets,
  });
  const deleteFile = useMutation({
    mutationFn: ({ bucket, key }: { readonly bucket: string; readonly key: string }) =>
      removeArtifact(requiredProjectId(), bucket, key),
    onSuccess: (_data, variables) => refreshFiles(variables.bucket),
  });
  const deleteMany = useMutation({
    mutationFn: ({ bucket, keys }: { readonly bucket: string; readonly keys: readonly string[] }) =>
      removeArtifacts(requiredProjectId(), bucket, keys),
    onSuccess: (_data, variables) => refreshFiles(variables.bucket),
  });

  return { createBucket, editBucketRetention, pinBucket, deleteBucket, deleteFile, deleteMany, refreshFiles };
}
