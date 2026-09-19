import { useGetPipelineInboundTrigger, useGetPipelineSchedule } from '@/shared/api/generated/applications/applications';
import type { PipelineInboundTrigger, PipelineSchedule } from '@/shared/api/generated/model';

/**
 * What the application-information panel says started, or will start, this
 * pipeline.
 *
 * NOTE(#899): this was a read of pylon's deleted
 * `/elitea_core/pipeline_trigger/...` route, which answered one row carrying a
 * `type` discriminator and a schedule/webhook blob. The Go backend has no
 * trigger-type column — it serves a cron schedule and an inbound trigger
 * INDEPENDENTLY — so the type is derived here the same way the pipeline
 * editor's own selector derives it, and the `schedule` blob is assembled from
 * the schedule read rather than passed through. `timezone` and `webhook_type`
 * are absent because nothing stores them any more: a schedule fires on the
 * platform's clock, and the inbound route verifies ONE bearer credential
 * rather than a per-provider signature header.
 *
 * Split into its own module rather than left in `ApplicationInformation.tsx`
 * for the §3.5 400-line file budget, which that file was already against.
 */
export interface PipelineTriggerParams {
  readonly isPipeline: boolean;
  readonly projectId: string | undefined;
  readonly versionId: string | undefined;
}

export interface PipelineTriggerTypeResult {
  readonly type: string | null | undefined;
  readonly schedule: unknown;
}

/**
 * Both reads answer a discriminated union — the 200 body or an error body —
 * even though `eliteaFetch` rejects on every non-2xx. Narrowing on `status`
 * rather than reaching through `.data.data` keeps the error arm from silently
 * becoming a mis-shaped success (R-A6, issue #132).
 */
function okBody<T>(answer: { readonly status: number; readonly data: unknown } | undefined): T | undefined {
  return answer !== undefined && answer.status === 200 ? (answer.data as T) : undefined;
}

export function usePipelineTriggerType({ isPipeline, projectId, versionId }: PipelineTriggerParams): PipelineTriggerTypeResult {
  const enabled = isPipeline && projectId !== undefined && versionId !== undefined;
  const project = Number(projectId ?? 0);
  const version = Number(versionId ?? 0);
  const scheduleQuery = useGetPipelineSchedule(project, version, { query: { enabled, retry: false } });
  const triggerQuery = useGetPipelineInboundTrigger(project, version, { query: { enabled, retry: false } });
  const schedule = okBody<PipelineSchedule>(scheduleQuery.data);
  const webhook = okBody<PipelineInboundTrigger>(triggerQuery.data);

  if (schedule?.configured === true) {
    return { type: 'schedule', schedule: { cron: schedule.cron, last_run: schedule.last_run } };
  }
  // A REVOKED trigger is kept as evidence and refused by the inbound path, so
  // it must not read as a live webhook here either.
  if (webhook?.configured === true && webhook.revoked_at === undefined) {
    return { type: 'webhook', schedule: {} };
  }
  return { type: undefined, schedule: undefined };
}
