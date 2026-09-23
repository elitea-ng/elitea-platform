/**
 * The pipeline editor's two unattended entry points, read and written
 * through the GENERATED client (#899).
 *
 * WHAT THIS REPLACES. The ported `usePipelineTrigger.ts` +
 * `entities/pipeline/api/pipelineTriggerApi.ts` pair spoke pylon's
 * single-resource route
 * `/elitea_core/pipeline_trigger/prompt_lib/{p}/pipeline/{v}/trigger`, one
 * `PipelineTrigger` object carrying a `type` discriminator and a
 * schedule/webhook jsonb blob. That route was deleted with #126 and is
 * served by nothing, so every request it made could only 404. Both modules
 * are gone.
 *
 * THE SHAPE IS DIFFERENT, DELIBERATELY. `internal/api/v2/pipelinetriggers`
 * has no trigger-TYPE concept. It serves two INDEPENDENT facilities, each
 * scoped to one pipeline VERSION, and a pipeline may have both at once:
 *
 *   - a cron schedule  — GET/PUT/DELETE `/pipeline_schedules/prompt_lib/{p}/{v}`
 *   - an inbound trigger — GET/POST/DELETE `/pipeline_triggers/prompt_lib/{p}/{v}`
 *     plus GET `/pipeline_triggers/secret/prompt_lib/{p}/{v}` (reveal)
 *
 * so this hook exposes them as two kinds rather than one `type` field. The
 * secret is returned ONLY by create/rotate and by reveal, both of which
 * carry the write permission — the plain read never carries a live
 * credential (that is the backend's rule, not a choice made here), which is
 * why `revealWebhook` exists at all instead of the secret simply being part
 * of `webhook`.
 *
 * A pipeline with neither facility answers 200 with `configured: false` on
 * both reads, never 404 — so "no trigger yet" is a normal state and not an
 * error the settings panel has to render.
 */
import { useCallback } from 'react';

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
import type { PipelineInboundTrigger, PipelineInboundTriggerModeRequest, PipelineSchedule } from '@/shared/api/generated/model';

export interface UsePipelineTriggersResult {
  /** The cron schedule, or `undefined` while the read has not answered. */
  readonly schedule: PipelineSchedule | undefined;
  /** The inbound trigger WITHOUT its secret — the plain read never carries one. */
  readonly webhook: PipelineInboundTrigger | undefined;
  readonly isFetching: boolean;
  /** Create or replace the schedule. The caller becomes its author. */
  readonly saveSchedule: (cron: string) => Promise<void>;
  readonly removeSchedule: () => Promise<void>;
  /**
   * Create the trigger, or rotate an existing one. Answers WITH the new
   * credential.
   *
   * The mode is passed on EVERY call, including a rotation, because a
   * rotation rewrites it server-side (#970): omitting it on a rotate would
   * silently move a signing trigger back to the bearer mode.
   */
  readonly rotateWebhook: (mode?: PipelineInboundTriggerModeRequest) => Promise<PipelineInboundTrigger>;
  /** Hand back the live credential of an existing trigger. */
  readonly revealWebhook: () => Promise<PipelineInboundTrigger>;
  readonly removeWebhook: () => Promise<void>;
}

/** The scope both facilities are keyed by, resolved once for every call below. */
interface TriggerScope {
  readonly projectId: number;
  readonly versionId: number;
}

/**
 * Both reads answer a DISCRIMINATED union — the 200 body or the error body of
 * a 403 — even though `eliteaFetch` rejects on every non-2xx, so the error arm
 * is unreachable at runtime. Narrowing on `status` rather than casting the
 * union away keeps that arm from silently becoming a `PipelineSchedule` shaped
 * `{error: '...'}` if the mutator's rejection behaviour ever changes.
 */
function okBody<T>(answer: { readonly status: number; readonly data: unknown } | undefined): T | undefined {
  return answer !== undefined && answer.status === 200 ? (answer.data as T) : undefined;
}

/** The write answers narrow the same way the reads do; see {@link okBody}. */
function requireTrigger(answer: { readonly status: number; readonly data: unknown }): PipelineInboundTrigger {
  const body = okBody<PipelineInboundTrigger>(answer);
  if (body === undefined) throw new Error('usePipelineTriggers: the trigger write answered without a trigger');
  return body;
}

function toScope(projectId: string | undefined, versionId: number | undefined): TriggerScope | undefined {
  if (projectId === undefined || projectId === '' || versionId === undefined) return undefined;
  const project = Number(projectId);
  if (!Number.isFinite(project) || !Number.isFinite(versionId)) return undefined;
  return { projectId: project, versionId };
}

export function usePipelineTriggers(projectId: string | undefined, versionId: number | undefined): UsePipelineTriggersResult {
  const queryClient = useQueryClient();
  const scope = toScope(projectId, versionId);
  const enabled = scope !== undefined;

  const scheduleQuery = useGetPipelineSchedule(scope?.projectId ?? 0, scope?.versionId ?? 0, { query: { enabled, retry: false } });
  const webhookQuery = useGetPipelineInboundTrigger(scope?.projectId ?? 0, scope?.versionId ?? 0, { query: { enabled, retry: false } });

  const requireScope = useCallback((): TriggerScope => {
    if (scope === undefined) throw new Error('usePipelineTriggers: projectId/versionId are required to change a trigger');
    return scope;
  }, [scope]);

  const invalidate = useCallback(
    async (target: TriggerScope, kind: 'schedule' | 'webhook'): Promise<void> => {
      const queryKey = kind === 'schedule'
        ? getGetPipelineScheduleQueryKey(target.projectId, target.versionId)
        : getGetPipelineInboundTriggerQueryKey(target.projectId, target.versionId);
      await queryClient.invalidateQueries({ queryKey });
    },
    [queryClient],
  );

  const saveSchedule = useCallback(
    async (cron: string): Promise<void> => {
      const target = requireScope();
      await savePipelineSchedule(target.projectId, target.versionId, { cron, active: true });
      await invalidate(target, 'schedule');
    },
    [requireScope, invalidate],
  );

  const removeSchedule = useCallback(
    async (): Promise<void> => {
      const target = requireScope();
      await deletePipelineSchedule(target.projectId, target.versionId);
      await invalidate(target, 'schedule');
    },
    [requireScope, invalidate],
  );

  const rotateWebhook = useCallback(
    async (mode?: PipelineInboundTriggerModeRequest): Promise<PipelineInboundTrigger> => {
      const target = requireScope();
      const answer = await rotatePipelineInboundTrigger(target.projectId, target.versionId, mode);
      await invalidate(target, 'webhook');
      return requireTrigger(answer);
    },
    [requireScope, invalidate],
  );

  const revealWebhook = useCallback(
    async (): Promise<PipelineInboundTrigger> => {
      const target = requireScope();
      const answer = await revealPipelineInboundTrigger(target.projectId, target.versionId);
      return requireTrigger(answer);
    },
    [requireScope],
  );

  const removeWebhook = useCallback(
    async (): Promise<void> => {
      const target = requireScope();
      await revokePipelineInboundTrigger(target.projectId, target.versionId);
      await invalidate(target, 'webhook');
    },
    [requireScope, invalidate],
  );

  return {
    schedule: okBody<PipelineSchedule>(scheduleQuery.data),
    webhook: okBody<PipelineInboundTrigger>(webhookQuery.data),
    isFetching: scheduleQuery.isFetching || webhookQuery.isFetching,
    saveSchedule,
    removeSchedule,
    rotateWebhook,
    revealWebhook,
    removeWebhook,
  };
}
