import type { ReactNode } from 'react';
import { useCallback } from 'react';

import type { Control, UseFormSetValue } from 'react-hook-form';
import { useWatch } from 'react-hook-form';

import type { ApplicationCreationInput } from '@/entities/application-form';
import { ApplicationMcpAccessToggle } from '@/features/agents';
import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import { AgentModelSettings } from '@/widgets/agent-model-settings';

const mcpTagName = 'mcp';
const emptyPipelineTags: readonly string[] = [];

function withMcpExposure(tags: readonly string[], enabled: boolean): readonly string[] {
  const withoutMcp = tags.filter((tag) => tag !== mcpTagName);
  return enabled ? [...withoutMcp, mcpTagName] : withoutMcp;
}

interface EditPipelineConfigurationFormProps {
  readonly control: Control<ApplicationCreationInput>;
  readonly setValue: UseFormSetValue<ApplicationCreationInput>;
  readonly projectId: string | undefined;
  readonly modelSettings: AgentLlmSettings | undefined;
  readonly onModelSettingsChange: (next: AgentLlmSettings) => void;
  readonly disabled: boolean;
}

/** Version controls mounted in the pipeline editor's configuration slot. */
export function EditPipelineConfigurationForm({
  control,
  setValue,
  projectId,
  modelSettings,
  onModelSettingsChange,
  disabled,
}: EditPipelineConfigurationFormProps): ReactNode {
  const tags = useWatch({ control, name: 'version_details.tags' }) ?? emptyPipelineTags;
  const handleMcpAccessChange = useCallback(
    (enabled: boolean) => {
      setValue('version_details.tags', [...withMcpExposure(tags, enabled)], {
        shouldDirty: true,
        shouldValidate: true,
      });
    },
    [setValue, tags],
  );

  return (
    <>
      <ApplicationMcpAccessToggle
        checked={tags.includes(mcpTagName)}
        onChange={handleMcpAccessChange}
        disabled={disabled}
        entityType="pipeline"
      />
      <AgentModelSettings projectId={projectId} value={modelSettings} onChange={onModelSettingsChange} disabled={disabled} />
    </>
  );
}
