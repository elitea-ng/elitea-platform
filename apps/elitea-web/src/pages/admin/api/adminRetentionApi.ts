/**
 * Data layer for Admin > Data retention (ADR-0042 decision 4, issue 1495).
 *
 * Every call goes through the GENERATED fetchers; this module adds the write
 * hooks and peels the transport envelope (`eliteaFetch` resolves the
 * envelope, not the body).
 *
 *   GET    /admin/retention/policies
 *   PUT    /admin/retention/policies/{data_class}
 *   PUT    /admin/retention/policies/{data_class}/projects/{project_id}
 *   DELETE /admin/retention/policies/{data_class}/projects/{project_id}
 *   GET    /admin/legal_holds?active=
 *   POST   /admin/legal_holds
 *   POST   /admin/legal_holds/{id}/release
 *
 * The policy routes need `retention.manage`, the hold routes
 * `legal_hold.manage` (shared migration 0195). Every write is audited.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import {
  createLegalHold,
  deleteProjectRetentionOverride,
  listLegalHolds,
  listRetentionPolicies,
  releaseLegalHold,
  setPlatformRetentionPolicy,
  setProjectRetentionOverride,
} from '@/shared/api/generated/admin/admin';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import type { LegalHold, LegalHoldCreate, RetentionPolicy } from '@/shared/api/generated/model';

import { reasonFields, reasonHeaders, type AdminReason } from '../adminDangerousAction';
import { unwrapBody } from '@/shared/api/unwrap';

const retentionKeys = {
  all: ['admin', 'retention'] as const,
  policies: () => ['admin', 'retention', 'policies'] as const,
  holds: (activeOnly: boolean) => ['admin', 'retention', 'holds', activeOnly] as const,
};

/** The server's own explanation of a refusal, when it gave one. */
export function retentionFailureReason(error: unknown): string | undefined {
  if (!(error instanceof EliteaApiError)) return undefined;
  const failure = error.failure;
  if (failure.kind !== 'http') return undefined;
  const body = failure.body;
  if (typeof body !== 'object' || body === null) return undefined;
  const reason = (body as { error?: unknown }).error;
  return typeof reason === 'string' && reason !== '' ? reason : undefined;
}

/** `GET /admin/retention/policies`: platform rows first, then overrides. */
export function useRetentionPolicies(): UseQueryResult<readonly RetentionPolicy[], Error> {
  return useQuery({
    queryKey: retentionKeys.policies(),
    queryFn: async ({ signal }): Promise<readonly RetentionPolicy[]> => {
      const body = unwrapBody(await listRetentionPolicies({ signal })) as
        | { policies?: RetentionPolicy[] }
        | undefined;
      return body?.policies ?? [];
    },
  });
}

/** `GET /admin/legal_holds`, newest first. */
export function useLegalHolds(activeOnly: boolean): UseQueryResult<readonly LegalHold[], Error> {
  return useQuery({
    queryKey: retentionKeys.holds(activeOnly),
    queryFn: async ({ signal }): Promise<readonly LegalHold[]> => {
      const params = activeOnly ? { active: true } : undefined;
      const body = unwrapBody(await listLegalHolds(params, { signal })) as
        | { holds?: LegalHold[] }
        | undefined;
      return body?.holds ?? [];
    },
  });
}

function useInvalidateRetention(): () => Promise<void> {
  const queryClient = useQueryClient();
  return () => queryClient.invalidateQueries({ queryKey: retentionKeys.all });
}

/** What one platform row's Save sends. A null maximum keeps forever. */
export interface PlatformPolicyDraft {
  readonly dataClass: string;
  readonly minAgeDays: number;
  readonly maxAgeDays: number | null;
  readonly allowProjectOverride: boolean;
}

/** `PUT /admin/retention/policies/{data_class}`. */
export function useSetPlatformPolicy(): UseMutationResult<
  void,
  Error,
  { readonly draft: PlatformPolicyDraft; readonly reason: AdminReason }
> {
  const invalidate = useInvalidateRetention();
  return useMutation({
    mutationFn: async ({ draft, reason }) => {
      // Shortening is critical and only the stored row tells it from
      // lengthening, so every write carries the reason (ADR-0059 R3.8b).
      await setPlatformRetentionPolicy(encodeURIComponent(draft.dataClass), {
        min_age_days: draft.minAgeDays,
        max_age_days: draft.maxAgeDays,
        allow_project_override: draft.allowProjectOverride,
        ...reasonFields('PUT /api/v2/admin/retention/policies/{dataClass} shorten', reason),
      });
    },
    onSuccess: invalidate,
  });
}

/** What a project override's Set sends. */
export interface OverrideDraft {
  readonly dataClass: string;
  readonly projectId: number;
  readonly maxAgeDays: number | null;
}

/** `PUT /admin/retention/policies/{data_class}/projects/{project_id}`. */
export function useSetProjectOverride(): UseMutationResult<
  void,
  Error,
  { readonly draft: OverrideDraft; readonly reason: AdminReason }
> {
  const invalidate = useInvalidateRetention();
  return useMutation({
    mutationFn: async ({ draft, reason }) => {
      await setProjectRetentionOverride(encodeURIComponent(draft.dataClass), draft.projectId, {
        max_age_days: draft.maxAgeDays,
        ...reasonFields('PUT /api/v2/admin/retention/policies/{dataClass}/projects/{projectID} shorten', reason),
      });
    },
    onSuccess: invalidate,
  });
}

/** `DELETE /admin/retention/policies/{data_class}/projects/{project_id}`. */
export function useDeleteProjectOverride(): UseMutationResult<
  void,
  Error,
  { readonly dataClass: string; readonly projectId: number; readonly reason: AdminReason }
> {
  const invalidate = useInvalidateRetention();
  return useMutation({
    mutationFn: async ({ dataClass, projectId, reason }) => {
      // Falling back to the platform maximum can shorten the retention.
      await deleteProjectRetentionOverride(
        encodeURIComponent(dataClass),
        projectId,
        reasonHeaders('DELETE /api/v2/admin/retention/policies/{dataClass}/projects/{projectID} shorten', reason),
      );
    },
    onSuccess: invalidate,
  });
}

/** `POST /admin/legal_holds`. */
export function useCreateLegalHold(): UseMutationResult<void, Error, LegalHoldCreate> {
  const invalidate = useInvalidateRetention();
  return useMutation({
    mutationFn: async (body: LegalHoldCreate) => {
      await createLegalHold(body);
    },
    onSuccess: invalidate,
  });
}

/** `POST /admin/legal_holds/{id}/release`. */
export function useReleaseLegalHold(): UseMutationResult<
  void,
  Error,
  { readonly id: number; readonly reason: AdminReason }
> {
  const invalidate = useInvalidateRetention();
  return useMutation({
    mutationFn: async ({ id, reason }) => {
      // The route keeps its own `reason` (the release reason the hold records).
      await releaseLegalHold(id, reasonFields('POST /api/v2/admin/legal_holds/{id}/release', reason));
    },
    onSuccess: invalidate,
  });
}
