/**
 * REST client for `Admin › Kill switch` (ADR-0041 decision 1). The three
 * operations are in `v2.yaml` (`listExecutionSuspensions`,
 * `createExecutionSuspension`, `resumeExecutionSuspension`), so the calls
 * come from orval; this module holds the envelope peel and the query keys.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { suspensionRow, suspensionRows, type ExecutionSuspensionRow } from '@/features/kill-switch';
import {
  createExecutionSuspension,
  listExecutionSuspensions,
  resumeExecutionSuspension,
} from '@/shared/api/generated/admin/admin';
import type { ExecutionSuspensionCreate } from '@/shared/api/generated/model';

const KEY = ['admin', 'execution-suspensions'] as const;

/** The suspensions: only the active ones, or the history too. */
export function useAdminSuspensions(activeOnly: boolean, refetchInterval: number | false) {
  return useQuery({
    queryKey: [...KEY, activeOnly],
    queryFn: async (): Promise<ExecutionSuspensionRow[]> =>
      suspensionRows(await listExecutionSuspensions({ active: activeOnly })),
    refetchInterval,
  });
}

export function useCreateAdminSuspension() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (body: ExecutionSuspensionCreate) => suspensionRow(await createExecutionSuspension(body)),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: KEY }),
  });
}

/**
 * The outcome of a resume: the suspension, and whether the call only
 * recorded the first approval of a two-person resume (202) so the
 * suspension stays active until a different administrator resumes it.
 */
export interface AdminResumeOutcome {
  readonly row: ExecutionSuspensionRow;
  readonly approvalRecorded: boolean;
}

export function useResumeAdminSuspension() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: { readonly id: number; readonly reason: string }): Promise<AdminResumeOutcome> => {
      const response = await resumeExecutionSuspension(input.id, input.reason === '' ? {} : { reason: input.reason });
      return { row: suspensionRow(response), approvalRecorded: response.status === 202 };
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: KEY }),
  });
}
