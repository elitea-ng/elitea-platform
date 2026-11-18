/**
 * REST client for `Admin › Delegations awaiting review` (ADR-0044 Amendment 1,
 * A1.3; delegation epic 2156 story S6): the delegation grants that await review
 * across the projects, and the scan that writes the ones the migration missed.
 * The operations come from orval (`getDelegationReview`, `scanDelegationReview`);
 * this module holds the envelope peel and the query key.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { getDelegationReview, scanDelegationReview } from '@/shared/api/generated/admin/admin';
import type { DelegationReview, DelegationScanResult } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';

const DELEGATION_REVIEW_KEY = ['admin', 'delegation-review'] as const;

/** The queue: pending and stale grants per project, and the mode the administrator decides about. */
export function useDelegationReview() {
  return useQuery({
    queryKey: DELEGATION_REVIEW_KEY,
    retry: false,
    queryFn: async () => unwrapBody(await getDelegationReview()) as DelegationReview,
  });
}

/** Writes the pending grants the startup migration did not see. It approves nothing. */
export function useScanDelegations() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => unwrapBody(await scanDelegationReview()) as DelegationScanResult,
    onSuccess: () => queryClient.invalidateQueries({ queryKey: DELEGATION_REVIEW_KEY }),
  });
}
