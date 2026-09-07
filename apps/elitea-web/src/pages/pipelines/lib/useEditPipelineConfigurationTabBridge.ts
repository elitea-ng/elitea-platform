import { useCallback, useMemo, useState } from 'react';

import type { UseFormSetValue } from 'react-hook-form';

import type { ApplicationCreationInput } from '@/entities/application-form';
import type { ConfigurationTabProps } from '@/features/pipelines';
import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';

import { toChatPipelineVersionDetails } from './editPipelineMappers';
import type { EditPipelineVersionFieldsState } from './useEditPipelineVersionFields';

/**
 * Bridges `ConfigurationTab`'s generic `setFieldValue: (field: string, value:
 * unknown) => void` contract into the page's two real stores — the RHF form
 * (`name`/`description`/`conversation_starters`) and
 * `useEditPipelineVersionFields` (everything else) — and feeds the result
 * back into the `versionDetails` object `ConfigurationTab` reads from.
 *
 * **What changed, and why the old note here was wrong.** This module used to
 * hold its OWN `llmSettings` override map, on the stated grounds that
 * "`useSaveApplicationVersion` cannot carry `llm_settings`/`tools` writes on
 * this route either way, so silently dropping an unknown path here loses
 * nothing a save could have kept". Both halves were false by the time it was
 * read: `VersionWriteRequest` carries `llm_settings` (and the page's own
 * model picker had been sending it since the graph-persistence fix), so the
 * chat panel's per-key writes and the picker's whole-object writes were
 * landing in two DIFFERENT pieces of state and the last render won. They now
 * land in one — `useEditPipelineVersionFields` — which is also the state the
 * save body reads and the nav blocker watches.
 *
 * `version_details.tools` is still an OVERLAY held here rather than saved
 * state, and that is deliberate rather than a gap: its only writer is
 * `usePipelineMCPToolsStatusMonitor`, which republishes the attached tools
 * with a live `online` flag for display. The version PUT reads no `tools`
 * key at all (`internal/api/v2/applications/handler.go`'s `UpdateVersion` has
 * no branch for it — attach/detach goes through the `entity_tool_mapping`
 * relation endpoints), so routing it into saved state would only invent a
 * dirty flag for something Save cannot persist.
 */
export interface EditPipelineConfigurationTabBridge {
  readonly setFieldValue: (field: string, value: unknown) => void;
  readonly versionDetails: ConfigurationTabProps['versionDetails'];
}

type ChatVersionDetails = NonNullable<ConfigurationTabProps['versionDetails']>;
type ChatLlmSettings = NonNullable<ChatVersionDetails['llm_settings']>;

/**
 * The live model pick re-expressed in the chat shape. Built key by key rather
 * than spread: under `exactOptionalPropertyTypes` the wire shape's
 * `model_name?: string | undefined` is not assignable to the chat shape's
 * `model_name?: string` (TS2375), the same per-key treatment
 * `editPipelineMappers.ts`'s own `buildChatLlmSettings` already documents.
 */
function toChatLlmSettings(settings: AgentLlmSettings): ChatLlmSettings {
  return {
    ...(settings.model_name !== undefined ? { model_name: settings.model_name } : {}),
    ...(settings.model_project_id !== undefined ? { model_project_id: settings.model_project_id } : {}),
    ...(settings.temperature !== undefined ? { temperature: settings.temperature } : {}),
    ...(settings.max_tokens !== undefined ? { max_tokens: settings.max_tokens } : {}),
    ...(settings.reasoning_effort !== undefined ? { reasoning_effort: settings.reasoning_effort } : {}),
  };
}

/**
 * Overlays the live edits onto the query-fetched `versionDetails` so the test
 * chat, the flow editor's node inputs and the MCP monitor all read what the
 * form currently shows rather than what the server last sent.
 *
 * Extracted to keep `useEditPipelineConfigurationTabBridge`'s own cyclomatic
 * complexity under this codebase's gate.
 */
function applyOverrides(
  base: ConfigurationTabProps['versionDetails'],
  fields: EditPipelineVersionFieldsState['fields'],
  tools: readonly unknown[] | undefined,
): ConfigurationTabProps['versionDetails'] {
  if (!base) return base;
  return {
    ...base,
    ...(tools === undefined ? {} : { tools }),
    welcome_message: fields.welcomeMessage,
    variables: fields.variables.map((variable) => ({ name: variable.name, value: variable.value })),
    ...(fields.llmSettings === undefined ? {} : { llm_settings: toChatLlmSettings(fields.llmSettings) }),
    meta: { ...base.meta, internal_tools: [...fields.internalTools] },
  };
}

export function useEditPipelineConfigurationTabBridge(
  activeVersion: ApplicationVersionDetail | undefined,
  setValue: UseFormSetValue<ApplicationCreationInput>,
  versionFields: EditPipelineVersionFieldsState,
): EditPipelineConfigurationTabBridge {
  const [tools, setTools] = useState<readonly unknown[] | undefined>(undefined);
  const { fields, applyFieldChange } = versionFields;

  const setFieldValue = useCallback(
    (field: string, value: unknown) => {
      if (field === 'name' || field === 'description') {
        setValue(field, typeof value === 'string' ? value : '', { shouldValidate: true, shouldDirty: true });
        return;
      }
      if (field === 'version_details.tools') {
        setTools((previous) => (Array.isArray(value) ? value : previous));
        return;
      }
      applyFieldChange(field, value);
    },
    [setValue, applyFieldChange],
  );

  const versionDetails = useMemo(
    () => applyOverrides(toChatPipelineVersionDetails(activeVersion), fields, tools),
    [activeVersion, fields, tools],
  );

  return { setFieldValue, versionDetails };
}
