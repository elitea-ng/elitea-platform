/**
 * The agent editor's Configuration panel — the form and the tools list.
 *
 * Lifted verbatim out of `EditApplication.tsx` when the Evaluation tab landed
 * beside it: the page now composes two panels rather than one, and the
 * §3.5 400-line budget leaves no room to hold both inline. Nothing about the
 * panel changed in the move; every comment below is the one the page carried.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';

import { AgentSkillsPanel } from '@/features/agent-skills';
import { AgentTagEditor, ApplicationInformation, CreateAgentForm } from '@/features/agents';
import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';
import { AgentModelSettings } from '@/widgets/agent-model-settings';

import type { EditApplicationEditorBridge } from '../lib/useEditApplicationEditorBridge';
import type { EditApplicationVersionFieldsState } from '../lib/useEditApplicationVersionFields';
import { EditApplicationToolsPanel } from './EditApplicationToolsPanel';

export interface EditApplicationConfigurationPanelProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly activeVersion: ApplicationVersionDetail | undefined;
  readonly editor: EditApplicationEditorBridge;
  readonly versionFields: EditApplicationVersionFieldsState;
  readonly isEditorDisabled: boolean;
  readonly isDirty: boolean;
  readonly isReadOnly: boolean;
  readonly onModelSettingsChange: (next: AgentLlmSettings) => void;
  /**
   * The AI-assisted instructions edit. Passed IN rather than built here: that
   * affordance gates itself on backend capability, and this panel has no
   * business knowing the rule.
   */
  readonly instructionsAiEditSlot?: ReactNode | undefined;
}

/**
 * The `meta` keys the Information panel's "Forked from" row reads, narrowed to
 * the string ids it needs.
 *
 * Deliberately a local twin of `pages/pipelines/ui/
 * EditPipelineConfigurationPanel.tsx`'s function of the same name rather than a
 * shared export: `features/agents`' curated public API is full (20/20 under the
 * §3.3 budget), and the two page slices may not import one another. The body is
 * five lines of `typeof` narrowing over an opaque jsonb map — the same
 * "deliberate duplication" `entities/application/model/types.ts` documents for
 * the version shapes themselves.
 */
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

export function EditApplicationConfigurationPanel(props: EditApplicationConfigurationPanelProps): ReactNode {
  const {
    projectId,
    applicationId,
    activeVersion,
    editor,
    versionFields,
    isEditorDisabled,
    isDirty,
    isReadOnly,
    onModelSettingsChange,
  } = props;
  const fork = forkOrigin(activeVersion);

  return (
    <Box data-testid="edit-application-configuration-tab-panel">
      <CreateAgentForm
        values={editor.values}
        onFieldChange={editor.onFieldChange}
        disabled={isEditorDisabled}
        /* #345 — the tag control. It reaches the wire through
           `toVersionSaveBody`'s `tags`, which `UpdateVersion` now
           writes as association rows. */
        instructionsAiEditSlot={props.instructionsAiEditSlot}
        tagsSlot={
          <AgentTagEditor
            projectId={projectId}
            value={versionFields.fields.tags}
            onChange={versionFields.setTags}
          />
        }
        modelSettingsSlot={
          <AgentModelSettings
            projectId={projectId}
            value={editor.values.version_details.llm_settings}
            onChange={onModelSettingsChange}
            disabled={isEditorDisabled}
          />
        }
      />
      {/*
       * #307 — tool attach/detach, the last of the "correctly-wired
       * components with no mount point". See
       * `./EditApplicationToolsPanel.tsx` for what this page owns
       * and `features/agents`' `AgentToolsPanel` for the composition
       * itself. Both writes hit the server immediately (the
       * `entity_tool_mapping` relation endpoint), independently of
       * this page's Save button.
       */}
      <EditApplicationToolsPanel
        projectId={projectId}
        applicationId={applicationId}
        activeVersion={activeVersion}
        versionFields={versionFields}
        isDirty={isDirty}
        isReadOnly={isReadOnly}
      />
      {/*
       * The SKILLS section (gap 11). Skills could be created, versioned,
       * published and exported in this app and attached to NOTHING — the
       * agent editor had no section for them, so the whole feature was
       * write-only. `entity_skill_mapping` is keyed by the VERSION id, which
       * is why the panel takes `activeVersion.id` and not the agent id, and
       * why it says so rather than rendering a picker when there is no saved
       * version yet.
       *
       * Placed after the tools panel, which is where production puts it
       * (SKILLS follows TOOLS/MODULES and precedes CHAT STARTERS).
       */}
      <AgentSkillsPanel
        projectId={projectId}
        appVersionId={activeVersion?.id}
        disabled={isReadOnly}
      />
      {/*
       * #846 — the Information accordion (agent id, version id, "Forked
       * from"). It was ported, unit-tested and exported, and its ONLY caller
       * was the PIPELINE editor: the agent editor — the screen the baseline
       * writes it for — never mounted it, so 17 legacy UI tests timed out on
       * `agent-information-section` and a person editing an agent had no way
       * to read the ids an external caller needs.
       *
       * Last, after SKILLS, because that is where the baseline puts it:
       * `frontends/EliteaUI/.../ApplicationConfigurationForm.jsx:72` renders
       * `<ApplicationInformation/>` as the final child of the configuration
       * column, below every other section.
       *
       * `isPipeline={false}` — an agent has no `pipeline_trigger` row, so the
       * trigger/schedule rows and the "Show pipeline" link stay off and the
       * panel issues no trigger request. Everything else is the same data the
       * pipeline panel passes: the page's own `applicationId`, the open
       * version's `id`, and the fork origin read off `version_details.meta`.
       */}
      <ApplicationInformation
        id={applicationId === undefined ? undefined : String(applicationId)}
        versionId={activeVersion?.id}
        isPipeline={false}
        isForked={fork.isForked}
        forkedProjectId={fork.forkedProjectId}
        forkedApplicationId={fork.forkedApplicationId}
      />
    </Box>
  );
}
