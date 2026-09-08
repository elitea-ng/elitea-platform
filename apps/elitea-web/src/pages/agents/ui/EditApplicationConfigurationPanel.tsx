/**
 * The agent editor's Configuration panel — the form and the tools list.
 *
 * Lifted verbatim out of `EditApplication.tsx` when the Evaluation tab landed
 * beside it: the page now composes two panels rather than one, and the
 * §3.5 400-line budget leaves no room to hold both inline. Nothing about the
 * panel changed in the move; every comment below is the one the page carried.
 */
import type { ReactNode } from 'react';
import { useCallback } from 'react';

import Box from '@mui/material/Box';

import type { Tag } from '@/entities/tag';
import { AgentTagEditor, ApplicationMcpAccessToggle, CreateAgentForm } from '@/features/agents';
import { AgentSkillsPanel } from '@/features/agent-skills';
import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';
import { AgentModelSettings } from '@/widgets/agent-model-settings';

import type { EditApplicationEditorBridge } from '../lib/useEditApplicationEditorBridge';
import type { EditApplicationVersionFieldsState } from '../lib/useEditApplicationVersionFields';
import { EditApplicationToolsPanel } from './EditApplicationToolsPanel';

const mcpTagName = 'mcp';

function withMcpExposure(tags: readonly Tag[], enabled: boolean): readonly Tag[] {
  const withoutMcp = tags.filter((tag) => tag.name !== mcpTagName);
  if (!enabled) return withoutMcp;
  const existing = tags.find((tag) => tag.name === mcpTagName);
  return [...withoutMcp, existing ?? { id: -1, name: mcpTagName, data: null }];
}

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

export function EditApplicationConfigurationPanel(props: EditApplicationConfigurationPanelProps): ReactNode {
  const { projectId, applicationId, activeVersion, editor, versionFields, isEditorDisabled, isDirty, isReadOnly, onModelSettingsChange } =
    props;
  const tags = versionFields.fields.tags;
  const setTags = versionFields.setTags;
  const handleMcpAccessChange = useCallback((enabled: boolean) => setTags(withMcpExposure(tags, enabled)), [setTags, tags]);
  const mcpAccessEnabled = tags.some((tag) => tag.name === mcpTagName);

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
          <>
            <ApplicationMcpAccessToggle
              checked={mcpAccessEnabled}
              onChange={handleMcpAccessChange}
              disabled={isEditorDisabled}
              entityType="agent"
            />
            <AgentTagEditor projectId={projectId} value={tags} onChange={setTags} />
          </>
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
    </Box>
  );
}
