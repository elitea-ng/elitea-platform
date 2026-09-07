/**
 * The bucket-access dialog's data layer: the project's exceptions, and the two
 * writes that change them.
 */
import { useMemo } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { useUserList } from '@/shared/api/generated/admin/admin';
import type { UserRecord } from '@/shared/api/generated/model';
import { unwrapListPage } from '@/shared/api/unwrap';

import {
  fetchBucketPermissions,
  removeBucketPermission,
  saveBucketPermissions,
  type BucketPermissionRow,
} from '../api/bucketAccessApi';
import { withBucketAccess, type BucketAccess } from '../lib/bucketAccess';

const bucketAccessQueryKey = (projectId: string): readonly string[] =>
  ['artifacts', 'bucket-permissions', projectId];

export function useBucketPermissions(projectId: string | undefined) {
  return useQuery({
    queryKey: bucketAccessQueryKey(projectId ?? ''),
    queryFn: () => fetchBucketPermissions(projectId ?? ''),
    enabled: projectId !== undefined && projectId !== '',
  });
}

export interface BucketAccessMutations {
  readonly setAccess: (input: { readonly userId: number; readonly access: BucketAccess }) => Promise<void>;
  readonly removeException: (userId: number) => Promise<void>;
  readonly isSaving: boolean;
}

/**
 * The two writes, both scoped to ONE bucket.
 *
 * `setAccess` rebuilds the member's WHOLE map before it writes, because the
 * route replaces rather than merges. `rows` is the source of the other
 * exceptions, so a stale query would drop them — the mutation therefore reads
 * the rows the caller passes, which is the same array the dialog rendered, and
 * invalidates afterwards.
 */
export function useBucketAccessMutations(
  projectId: string | undefined,
  bucket: string | undefined,
  rows: readonly BucketPermissionRow[],
): BucketAccessMutations {
  const queryClient = useQueryClient();
  const required = (): { projectId: string; bucket: string } => {
    if (projectId === undefined || projectId === '') throw new Error('A project must be selected.');
    if (bucket === undefined || bucket === '') throw new Error('A bucket must be selected.');
    return { projectId, bucket };
  };
  const refresh = () =>
    queryClient.invalidateQueries({ queryKey: bucketAccessQueryKey(projectId ?? '') });

  const existingMap = (userId: number): Readonly<Record<string, readonly string[]>> =>
    rows.find((row) => row.user_id === userId)?.bucket_permissions ?? {};

  const setAccess = useMutation({
    mutationFn: async ({ userId, access }: { userId: number; access: BucketAccess }) => {
      const { projectId: id, bucket: name } = required();
      await saveBucketPermissions(id, userId, withBucketAccess(existingMap(userId), name, access));
    },
    onSuccess: refresh,
  });

  const removeException = useMutation({
    mutationFn: async (userId: number) => {
      const { projectId: id, bucket: name } = required();
      await removeBucketPermission(id, userId, name);
    },
    onSuccess: refresh,
  });

  return {
    setAccess: async (input) => {
      await setAccess.mutateAsync(input);
    },
    removeException: async (userId) => {
      await removeException.mutateAsync(userId);
    },
    isSaving: setAccess.isPending || removeException.isPending,
  };
}

/**
 * The project members the picker can name.
 *
 * The SAME source Settings › Users reads (`useUserList` over
 * `/api/v2/admin/users/default/{projectID}`), so the two screens cannot
 * disagree about who is in the project. `unwrapListPage` takes the envelope or
 * the body, which is what keeps this from resolving to an empty picker on a
 * 200 (#132).
 */
export interface BucketAccessCandidate {
  readonly id: number;
  readonly name: string;
  readonly email: string;
}

export function useBucketAccessCandidates(projectId: string | undefined): {
  readonly candidates: readonly BucketAccessCandidate[];
  readonly isLoading: boolean;
} {
  const query = useUserList(
    projectId ?? '',
    { limit: 100, offset: 0 },
    { query: { enabled: projectId !== undefined && projectId !== '' } },
  ) as { isFetching: boolean; data?: unknown };

  const candidates = useMemo(() => {
    const { rows } = unwrapListPage<UserRecord>(query.data, 'bucketAccessCandidates');
    const mapped: BucketAccessCandidate[] = [];
    for (const row of rows) {
      const id = Number(row.id);
      // A member whose id is not numeric cannot be named in an exception; the
      // route keys on the database user id. Skipping is right, and silent is
      // right too — the row is still a member, just not one this dialog can
      // restrict.
      if (!Number.isInteger(id) || id <= 0) continue;
      mapped.push({ id, name: row.name ?? '', email: row.email ?? '' });
    }
    return mapped;
  }, [query.data]);

  return { candidates, isLoading: query.isFetching };
}
