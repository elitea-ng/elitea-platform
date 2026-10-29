import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { listReleaseGateSuites, putReleaseGateSuite } from '@/shared/api/generated/applications/applications';
import type { ReleaseGateSuite, ReleaseGateSuiteList, ReleaseGateSuiteRequest } from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';

/**
 * The release gate suites of an agent (ADR-0040 decision 4): which of its
 * evaluation datasets gate a release, with what criteria, and which one is
 * the regression suite. Read through the envelope (`unwrapBody`).
 */

const KEY = ['agent-evaluation', 'release-gate-suites'] as const;

export function useReleaseGateSuites(projectId: string | undefined, applicationId: number | undefined) {
  return useQuery({
    queryKey: [...KEY, projectId, applicationId],
    enabled: projectId !== undefined && projectId !== '' && applicationId !== undefined,
    queryFn: async () =>
      unwrapBody(await listReleaseGateSuites(projectId ?? '', applicationId ?? 0)) as ReleaseGateSuiteList,
  });
}

export function usePutReleaseGateSuite(projectId: string | undefined, applicationId: number | undefined) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ datasetId, body }: { readonly datasetId: number; readonly body: ReleaseGateSuiteRequest }) =>
      unwrapBody(await putReleaseGateSuite(projectId ?? '', applicationId ?? 0, datasetId, body)) as ReleaseGateSuite,
    onSuccess: () => queryClient.invalidateQueries({ queryKey: [...KEY, projectId, applicationId] }),
  });
}

/** The server's refusal as a sentence; its `message` names the broken rule. */
export function releaseGateErrorMessage(error: unknown): string {
  if (error instanceof EliteaApiError && error.failure.kind === 'http') {
    const body = error.failure.body as { code?: unknown; message?: unknown; error?: unknown } | null;
    if (body?.code === 'release_gate_regression_taken') {
      return t(
        'features.agentEvaluation.releaseGate.regressionTaken',
        'The agent already has a regression suite. Unmark that one first.',
      );
    }
    if (typeof body?.message === 'string' && body.message !== '') return body.message;
    if (typeof body?.error === 'string' && body.error !== '') return body.error;
  }
  return t('features.agentEvaluation.releaseGate.saveFailed', 'The release gate settings could not be saved.');
}
