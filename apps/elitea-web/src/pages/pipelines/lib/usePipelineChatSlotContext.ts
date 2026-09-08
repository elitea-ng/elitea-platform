/**
 * What the editor's test-chat slot needs to identify the pipeline it is
 * talking to, derived once from the page's loaded detail/version.
 *
 * Its own hook rather than a `useMemo` inside `EditPipeline` for the reason
 * `useEditPipelineData`/`useEditPipelineForm` already give for living here:
 * the four `?`/`??` narrowings below each cost that component a branch, and
 * it is at the §3.5 cyclomatic-complexity ceiling (12).
 */
import { useMemo } from 'react';

import type { ApplicationDetail, ApplicationVersionDetail } from '@/shared/api/generated/model';

import { pipelineDetailDisplayName } from './editPipelineMappers';
import type { PipelineChatSlotContext } from './pipelineConfigurationTabSlots';
import type { PipelineEditorUser } from './usePipelineEditorUser';

export interface UsePipelineChatSlotContextArgs {
  readonly projectId: string | undefined;
  /** The pipeline's id as the ROUTE carries it — the participant's `entity_meta.id` is that string, not the detail's own field. */
  readonly applicationId: string | undefined;
  readonly detail: ApplicationDetail | undefined;
  readonly activeVersion: ApplicationVersionDetail | undefined;
  readonly user: PipelineEditorUser | undefined;
}

export function usePipelineChatSlotContext(args: UsePipelineChatSlotContextArgs): PipelineChatSlotContext {
  const { projectId, applicationId, detail, activeVersion, user } = args;
  const pipelineName = detail ? pipelineDetailDisplayName(detail) : undefined;
  const versionId = activeVersion?.id !== undefined ? String(activeVersion.id) : undefined;
  // The version's own `agent_type` is what routes the worker to the graph
  // assembler; `'pipeline'` is the fallback only for the window before the
  // version has loaded, and this page never edits anything else.
  const agentType = activeVersion?.agent_type ?? 'pipeline';

  return useMemo(
    () => ({ identity: { projectId, applicationId, pipelineName, versionId, agentType }, user }),
    [projectId, applicationId, pipelineName, versionId, agentType, user],
  );
}
