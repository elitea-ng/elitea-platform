/**
 * The kill switch (ADR-0041 decision 1): the wire shape of a suspension, the
 * server's refusal words, and the project surface's hooks.
 *
 * The admin console reads the same shape over the admin routes
 * (`pages/admin/api/adminKillSwitchApi.ts`); the project surface here serves
 * the agent page's Stop agent action. Both peel the `{data,status,headers}`
 * envelope with `unwrapBody`: `eliteaFetch` returns the envelope, not the
 * body.
 */
import { useMemo } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { usePermissionList } from '@/shared/api/generated/auth/auth';
import type { Permission } from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import {
  createProjectExecutionSuspension,
  listProjectExecutionSuspensions,
  resumeProjectExecutionSuspension,
} from '@/shared/api/generated/settings/settings';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';

/** The permission every suspend and resume route is gated on (shared 0194). */
export const SAFETY_SUSPEND_PERMISSION = 'safety.suspend';

/** One suspension, in the server's own key names. */
export interface ExecutionSuspensionRow {
  readonly id: number;
  readonly scope: string;
  readonly project_id: number | null;
  readonly application_id: number | null;
  readonly toolkit_type: string | null;
  readonly reason: string;
  readonly actor_id: number;
  readonly source: string;
  readonly resume_requires: string;
  readonly created_at: string;
  readonly cancelled_runs: number;
  readonly active: boolean;
  readonly resumed_at: string | null;
  readonly resumed_by: number | null;
  readonly resume_reason: string | null;
}

/** Reads `{suspensions: [...]}` out of the envelope or the bare body. */
export function suspensionRows(response: unknown): ExecutionSuspensionRow[] {
  const body = unwrapBody(response);
  if (typeof body === 'object' && body !== null) {
    const rows = (body as { suspensions?: unknown }).suspensions;
    if (Array.isArray(rows)) return rows as ExecutionSuspensionRow[];
  }
  throw new Error('execution suspensions: the server answered with an unrecognised body');
}

/** Reads one suspension out of the envelope or the bare body. */
export function suspensionRow(response: unknown): ExecutionSuspensionRow {
  return unwrapBody(response) as ExecutionSuspensionRow;
}

/** The server's code words, or undefined for a refusal with no known code. */
function codeMessage(code: unknown): string | undefined {
  switch (code) {
    case 'already_suspended':
      return t('features.killSwitch.error.alreadySuspended', 'An active suspension already covers this target.');
    case 'confirmation_required':
      return t('features.killSwitch.error.confirmationRequired', 'Type STOP to stop the whole platform.');
    case 'resume_forbidden':
      return t(
        'features.killSwitch.error.resumeForbidden',
        'The resume rule of this suspension does not let you resume it. Ask a platform administrator.',
      );
    default:
      return undefined;
  }
}

function statusMessage(status: number): string {
  switch (status) {
    case 403:
      return t('features.killSwitch.error.forbidden', 'You do not have permission to stop or resume execution.');
    case 404:
      return t('features.killSwitch.error.notFound', 'The suspension does not exist.');
    case 409:
      return t('features.killSwitch.error.notActive', 'The suspension was already resumed.');
    default:
      return t('features.killSwitch.error.generic', 'Could not save the suspension. Try again.');
  }
}

/** The words for a refused suspend or resume, from the server's status and code. */
export function describeSuspensionError(error: unknown): string {
  const failure = error instanceof EliteaApiError && error.failure.kind === 'http' ? error.failure : undefined;
  if (failure === undefined) return statusMessage(0);
  const body = failure.body;
  const code = typeof body === 'object' && body !== null ? (body as { code?: unknown }).code : undefined;
  return codeMessage(code) ?? statusMessage(failure.status);
}

const projectKey = (projectId: string | undefined): readonly unknown[] =>
  ['execution-suspensions', 'project', projectId ?? ''];

/** The active suspensions of one project. Off when `enabled` is false. */
export function useProjectSuspensions(projectId: string | undefined, enabled: boolean) {
  return useQuery({
    queryKey: projectKey(projectId),
    enabled: enabled && projectId !== undefined && projectId !== '',
    queryFn: async () => suspensionRows(await listProjectExecutionSuspensions(projectId ?? '', { active: true })),
  });
}

export interface StopAgentInput {
  readonly applicationId: number;
  readonly reason: string;
}

/** Suspends one agent of the project. The server cancels its runs in flight. */
export function useStopAgent(projectId: string | undefined) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: StopAgentInput) =>
      suspensionRow(
        await createProjectExecutionSuspension(projectId ?? '', {
          scope: 'agent',
          application_id: input.applicationId,
          reason: input.reason,
        }),
      ),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: projectKey(projectId) }),
  });
}

/** True when the caller has `safety.suspend` in the project (the project admin). */
export function useCanSuspend(projectId: string | undefined): boolean {
  const query = usePermissionList(projectId ?? '', {
    query: { enabled: projectId !== undefined && projectId !== '' },
  });
  return useMemo(() => {
    const list = unwrapBody(query.data) as Permission[] | undefined;
    return Array.isArray(list) && list.some((entry) => entry.enabled && entry.name === SAFETY_SUSPEND_PERMISSION);
  }, [query.data]);
}

/** Resumes one suspension of the project. */
export function useResumeProjectSuspension(projectId: string | undefined) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (suspensionId: number) =>
      suspensionRow(await resumeProjectExecutionSuspension(projectId ?? '', suspensionId, {})),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: projectKey(projectId) }),
  });
}
