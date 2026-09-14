import type { ReactNode } from 'react';
import { useCallback } from 'react';

import Box from '@mui/material/Box';

import { AgentTagEditor, ApplicationInformation, ApplicationMcpAccessToggle, CreateAgentForm } from '@/features/agents';
import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';
import { AgentModelSettings } from '@/widgets/agent-model-settings';

import type { EditPipelineEditorBridge } from '../lib/useEditPipelineEditorBridge';
import type { EditPipelineVersionFieldsState } from '../lib/useEditPipelineVersionFields';
import { EditPipelineToolsPanel } from './EditPipelineToolsPanel';
import { EditPipelineTriggersPanel } from './EditPipelineTriggersPanel';

/**
 * The pipeline editor's left pane — the real configuration form, replacing
 * the "Configuration form is not available yet" notice that used to stand
 * here (`../lib/pipelineConfigurationTabGaps.tsx`).
 *
 * **The gap notice's stated reason was stale, not merely optimistic.** It
 * said the six `features/agents`-owned panels were "NOT exported from
 * `features/agents/index.ts` (verified: `grep -n "^export"`, no such names)".
 * That grep was run before `CreateAgentForm`, `AgentTagEditor` and
 * `AgentToolsPanel` landed on that curated API for the AGENT editor's own
 * mount; by the time it was read, `pages/agents/ui/
 * EditApplicationConfigurationPanel.tsx` was already composing the identical
 * set. A pipeline IS an application row, so the same panels compose here.
 *
 * Reference parity (`apps/elitea-ui/src/pages/Applications/Components/
 * Applications/PipelineConfigurationForm.jsx`) and the two deliberate
 * differences:
 *  - **General/Tools/MODULES/Welcome message/Chat starters/Advanced** — all
 *    rendered, through `CreateAgentForm` plus `EditPipelineToolsPanel`.
 *  - **`showInstructions={false}`** — the reference does not put an
 *    instructions box on a pipeline either. A pipeline's `instructions`
 *    column IS the flow graph's YAML, authored on the canvas beside this
 *    pane; a second editor for the same column would let the two disagree.
 *  - **`entityType="pipeline"`** — suppresses the "generate with AI"
 *    affordance in the General header, matching the reference's own
 *    `isFromPipeline` suppression.
 *  - **Information** — rendered, read-only. `showPipeline` opens the stored
 *    document; the trigger rows come from the `pipeline_trigger` endpoint
 *    `ApplicationInformation` already queries.
 *  - **Triggers & schedules** — the pipeline's two UNATTENDED entry points
 *    (issues 192 and 193): an inbound signed URL an external system calls,
 *    and a cron the platform fires. Both are new capability rather than
 *    parity: legacy had them, the Go stack had neither, and the three
 *    `/pipeline_trigger/` routes issue 126 deleted were the legacy API
 *    surface. `EditPipelineTriggersPanel` owns them for the same reason
 *    `EditPipelineToolsPanel` owns the tools grid — this file is a
 *    composition root and that one is a stateful feature with its own
 *    queries.
 *  - **EDITOR NOTES is absent, and that is a backend contract gap, not an
 *    omission.** `features/agents/ui/ApplicationEditorNotes.tsx` exists and
 *    is tested, but `version_details.notes` has no column on
 *    `application_versions` (`001_initial.sql`), no property on
 *    `VersionWriteRequest` (`services/elitea-main/api/openapi/v2.yaml`), and
 *    no branch in `UpdateVersion`. Mounting it would give a person a text box
 *    the next save silently discards — the exact defect class this form
 *    exists to end.
 */
export interface EditPipelineConfigurationPanelProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly activeVersion: ApplicationVersionDetail | undefined;
  readonly editor: EditPipelineEditorBridge;
  readonly versionFields: EditPipelineVersionFieldsState;
  readonly isEditorDisabled: boolean;
  readonly isDirty: boolean;
  readonly isReadOnly: boolean;
  readonly onModelSettingsChange: (next: AgentLlmSettings) => void;
}

/** The `meta` keys the Information panel's "Forked from" row reads, narrowed to the string ids it needs. */
function forkOrigin(activeVersion: ApplicationVersionDetail | undefined): {
  readonly isForked: boolean;
  readonly forkedProjectId: string | undefined;
  readonly forkedApplicationId: string | undefined;
} {
  const meta: Record<string, unknown> = activeVersion?.meta ?? {};
  const parentProjectId = meta['parent_project_id'];
  const parentEntityId = meta['parent_entity_id'];
  const forkedProjectId = typeof parentProjectId === 'string' || typeof parentProjectId === 'number' ? String(parentProjectId) : undefined;
  const forkedApplicationId = typeof parentEntityId === 'string' || typeof parentEntityId === 'number' ? String(parentEntityId) : undefined;
  return {
    isForked: forkedProjectId !== undefined && forkedApplicationId !== undefined,
    forkedProjectId,
    forkedApplicationId,
  };
}

export function EditPipelineConfigurationPanel(props: EditPipelineConfigurationPanelProps): ReactNode {
  const { projectId, applicationId, activeVersion, editor, versionFields, isEditorDisabled, isDirty, isReadOnly, onModelSettingsChange } = props;
  const fork = forkOrigin(activeVersion);
  const tags = versionFields.fields.tags;
  const setTags = versionFields.setTags;
  const handleMcpAccessChange = useCallback((enabled: boolean) => {
    const withoutMcp = tags.filter((tag) => tag.name !== 'mcp');
    const mcpTag = tags.find((tag) => tag.name === 'mcp') ?? { id: -1, name: 'mcp', data: null };
    setTags(enabled ? [...withoutMcp, mcpTag] : withoutMcp);
  }, [setTags, tags]);

  return (
    <Box data-testid="edit-pipeline-configuration-panel">
      <CreateAgentForm
        entityType="pipeline"
        showInstructions={false}
        values={editor.values}
        onFieldChange={editor.onFieldChange}
        disabled={isEditorDisabled}
        tagsSlot={
          <>
            <ApplicationMcpAccessToggle
              checked={tags.some((tag) => tag.name === 'mcp')}
              onChange={handleMcpAccessChange}
              disabled={isEditorDisabled}
              entityType="pipeline"
            />
            <AgentTagEditor projectId={projectId} value={tags} onChange={setTags} />
          </>
        }
        modelSettingsSlot={
          <AgentModelSettings
            projectId={projectId}
            value={versionFields.fields.llmSettings}
            onChange={onModelSettingsChange}
            disabled={isEditorDisabled}
          />
        }
      />
      <EditPipelineToolsPanel
        projectId={projectId}
        applicationId={applicationId}
        activeVersion={activeVersion}
        versionFields={versionFields}
        isDirty={isDirty}
        isReadOnly={isReadOnly}
      />
      <EditPipelineTriggersPanel
        projectId={projectId}
        versionId={activeVersion === undefined ? undefined : Number(activeVersion.id)}
        isReadOnly={isReadOnly}
      />
      <ApplicationInformation
        id={applicationId === undefined ? undefined : String(applicationId)}
        versionId={activeVersion?.id}
        isPipeline
        isForked={fork.isForked}
        forkedProjectId={fork.forkedProjectId}
        forkedApplicationId={fork.forkedApplicationId}
        pipelineInstructions={activeVersion?.instructions ?? ''}
        showPipeline
      />
    </Box>
  );
}
