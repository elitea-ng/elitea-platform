import { useCallback, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import {
  deletePipelineSchedule,
  getGetPipelineInboundTriggerQueryKey,
  getGetPipelineScheduleQueryKey,
  revealPipelineInboundTrigger,
  revokePipelineInboundTrigger,
  rotatePipelineInboundTrigger,
  savePipelineSchedule,
  useGetPipelineInboundTrigger,
  useGetPipelineSchedule,
} from '@/shared/api/generated/applications/applications';
import type { PipelineInboundTrigger, PipelineSchedule } from '@/shared/api/generated/model';

/**
 * The data half of the pipeline's "Triggers & schedules" section — the
 * client for issues 192 (an external caller starts a pipeline) and 193 (a
 * cron does).
 *
 * ## Why the reads go through `.data` and never through the hook result
 *
 * `eliteaFetch` resolves the whole RESPONSE ENVELOPE — `{ status, data }` —
 * and the generated `as T` cast types that envelope, not the body. Reading a
 * field straight off a query result therefore compiles and is `undefined` at
 * run time on a perfectly successful 200. That defect has shipped twice in
 * this application on one endpoint (issue 132), so every read below unwraps
 * the envelope in one place and hands the caller the BODY.
 *
 * ## Why the writes are plain calls and not generated mutation hooks
 *
 * orval's `react-query` client generates a QUERY hook for every operation in
 * this document, including the writes. Driving a rotate or a revoke through
 * `useQuery` would re-run it on a refetch, so the write functions are called
 * directly and the affected query key is invalidated — the same shape
 * `features/settings/ui/project-general/AgentPipelineBuilder.tsx` uses.
 *
 * ## The credential is state, not cache
 *
 * `secret` comes back on a rotate and on an explicit reveal, and on nothing
 * else. It is held in component state and never written into the query
 * cache: a cached credential would survive navigation, appear in a devtools
 * cache dump, and outlive the moment the person asked to see it.
 */
export interface PipelineTriggerSettings {
  readonly trigger: PipelineInboundTrigger | undefined;
  readonly schedule: PipelineSchedule | undefined;
  readonly isLoading: boolean;
  /** The credential, present only after a rotate or an explicit reveal. */
  readonly secretUrl: string | undefined;
  /** The last write that failed, as a message the section can render. */
  readonly error: string | undefined;
  readonly isBusy: boolean;
  readonly rotate: () => Promise<void>;
  readonly reveal: () => Promise<void>;
  readonly revoke: () => Promise<void>;
  readonly hideSecret: () => void;
  readonly saveSchedule: (cron: string, active: boolean) => Promise<void>;
  readonly removeSchedule: () => Promise<void>;
}

/** `undefined` for either id means "this pipeline is not resolved yet"; every query stays disabled. */
export interface PipelineTriggerSettingsInput {
  readonly projectId: string | undefined;
  readonly versionId: number | undefined;
}

function numericProjectId(projectId: string | undefined): number | undefined {
  if (projectId === undefined || projectId === '') return undefined;
  const parsed = Number(projectId);
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : undefined;
}

export function usePipelineTriggerSettings(input: PipelineTriggerSettingsInput): PipelineTriggerSettings {
  const projectId = numericProjectId(input.projectId);
  const versionId = input.versionId;
  const enabled = projectId !== undefined && versionId !== undefined;
  const queryClient = useQueryClient();

  const [secretUrl, setSecretUrl] = useState<string | undefined>(undefined);
  const [error, setError] = useState<string | undefined>(undefined);
  const [isBusy, setIsBusy] = useState(false);

  const triggerQuery = useGetPipelineInboundTrigger(projectId ?? 0, versionId ?? 0, {
    query: { enabled },
  });
  const scheduleQuery = useGetPipelineSchedule(projectId ?? 0, versionId ?? 0, {
    query: { enabled },
  });

  const refresh = useCallback(async (): Promise<void> => {
    if (projectId === undefined || versionId === undefined) return;
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: getGetPipelineInboundTriggerQueryKey(projectId, versionId) }),
      queryClient.invalidateQueries({ queryKey: getGetPipelineScheduleQueryKey(projectId, versionId) }),
    ]);
  }, [queryClient, projectId, versionId]);

  // One wrapper for every write. It is what keeps "a failed write must leave a
  // message a person can read" from being re-decided per action — and what
  // stops a rejected promise from reaching the console as an unhandled
  // rejection while the panel shows nothing.
  const run = useCallback(
    async (action: () => Promise<string | undefined>): Promise<void> => {
      if (!enabled) return;
      setIsBusy(true);
      setError(undefined);
      try {
        const revealed = await action();
        if (revealed !== undefined) setSecretUrl(revealed);
        await refresh();
      } catch (cause) {
        setError(cause instanceof Error ? cause.message : String(cause));
      } finally {
        setIsBusy(false);
      }
    },
    [enabled, refresh],
  );

  const rotate = useCallback(
    () =>
      run(async () => {
        const response = await rotatePipelineInboundTrigger(projectId ?? 0, versionId ?? 0);
        return response.status === 200 ? response.data.secret_url : undefined;
      }),
    [run, projectId, versionId],
  );

  const reveal = useCallback(
    () =>
      run(async () => {
        const response = await revealPipelineInboundTrigger(projectId ?? 0, versionId ?? 0);
        return response.status === 200 ? response.data.secret_url : undefined;
      }),
    [run, projectId, versionId],
  );

  const revoke = useCallback(
    () =>
      run(async () => {
        await revokePipelineInboundTrigger(projectId ?? 0, versionId ?? 0);
        setSecretUrl(undefined);
        return undefined;
      }),
    [run, projectId, versionId],
  );

  const saveScheduleAction = useCallback(
    (cron: string, active: boolean) =>
      run(async () => {
        await savePipelineSchedule(projectId ?? 0, versionId ?? 0, { cron, active });
        return undefined;
      }),
    [run, projectId, versionId],
  );

  const removeSchedule = useCallback(
    () =>
      run(async () => {
        await deletePipelineSchedule(projectId ?? 0, versionId ?? 0);
        return undefined;
      }),
    [run, projectId, versionId],
  );

  const hideSecret = useCallback(() => setSecretUrl(undefined), []);

  // The envelope is named before it is unwrapped. `eliteaFetch` resolves
  // `{data, status, headers}` and the generated `as T` types that envelope, so
  // the BODY is one `.data` down — not zero (which types as the envelope and
  // reads every field as undefined) and not two.
  const triggerEnvelope = triggerQuery.data;
  const scheduleEnvelope = scheduleQuery.data;

  return {
    trigger: triggerEnvelope?.status === 200 ? triggerEnvelope.data : undefined,
    schedule: scheduleEnvelope?.status === 200 ? scheduleEnvelope.data : undefined,
    isLoading: enabled && (triggerQuery.isLoading || scheduleQuery.isLoading),
    secretUrl,
    error,
    isBusy,
    rotate,
    reveal,
    revoke,
    hideSecret,
    saveSchedule: saveScheduleAction,
    removeSchedule,
  };
}
