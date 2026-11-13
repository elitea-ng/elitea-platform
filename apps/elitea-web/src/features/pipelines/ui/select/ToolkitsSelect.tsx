import type { ReactNode } from 'react';
import { useCallback, useContext, useMemo } from 'react';

import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import * as FlowEditorHelpers from '../../lib/flow-editor/helpers/flowEditor.helpers';
import { useGetToolkitNameFromSchema } from '../../lib/flow-editor/hooks/useGetToolkitNameFromSchema';
import { useSelectedProjectId } from '../../lib/flow-editor/hooks/useSelectedProjectId';
import { useToolkitTypeSchemas } from '../../lib/flow-editor/hooks/useToolkitTypeSchemas';
import { PipelineMultiSelect, type PipelineMultiSelectOption } from './PipelineMultiSelect';
import type { PipelineToolEntry } from './pipelineToolEntry.types';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * select/ToolkitsSelect.jsx` (unit A2h).
 *
 * DEVIATIONS FROM BASELINE (both matching this batch's established
 * conventions, see `../../lib/flow-editor/hooks/useFunctionInputMapping.ts`'s
 * own header for the identical rationale on both points):
 *
 *  1. `useFormikContext()` (`values?.version_details?.tools`) -> an
 *     explicit `versionTools` prop. No Formik dependency exists in this
 *     app.
 *  2. `useGetToolkitNameFromSchema()` (baseline: reads a Redux-cached
 *     `schemaOfTools` ambiently) -> `useGetToolkitNameFromSchema(toolkitTypeSchemas)`,
 *     `toolkitTypeSchemas` resolved locally via `useToolkitTypeSchemas`/
 *     `useSelectedProjectId` (this slice's own already-landed hooks).
 *
 * `Select.SingleSelect` (`multiple`) -> `PipelineMultiSelect`, see
 * `InputSelect.tsx`'s doc comment.
 */
export interface ToolkitsSelectProps {
  readonly id: string;
  readonly label?: string;
  readonly disabled?: boolean | undefined;
  readonly onValueChange?: (value: string[]) => void;
  readonly allowApplications?: boolean;
  readonly versionTools?: readonly PipelineToolEntry[];
}

export function ToolkitsSelect(props: ToolkitsSelectProps): ReactNode {
  const { id, label = 'Toolkits', disabled = false, onValueChange, allowApplications = false, versionTools = [] } = props;

  const context = useContext(FlowEditorContext);
  const setYamlJsonObject = context?.setYamlJsonObject;
  const yamlJsonObject = context?.yamlJsonObject;

  const projectId = useSelectedProjectId();
  const { toolkitTypeSchemas } = useToolkitTypeSchemas(projectId);
  const { getToolkitNameFromSchema } = useGetToolkitNameFromSchema(toolkitTypeSchemas);

  const yamlNode = useMemo(
    () => yamlJsonObject?.nodes?.find(node => node.id === id),
    [id, yamlJsonObject?.nodes],
  );

  const toolkitOptions = useMemo<PipelineMultiSelectOption[]>(
    () =>
      versionTools
        .filter(tool => allowApplications || tool.type !== 'application')
        .map(tool => {
          const nameFromSchema = getToolkitNameFromSchema(tool);
          return { label: tool.toolkit_name ?? nameFromSchema, value: nameFromSchema };
        })
        .sort((a, b) => a.label.localeCompare(b.label)),
    [allowApplications, getToolkitNameFromSchema, versionTools],
  );

  const selectedToolkits = useMemo(() => {
    const toolNames = (yamlNode?.['tool_names'] as Record<string, readonly string[]> | undefined) ?? {};
    return Object.keys(toolNames);
  }, [yamlNode]);

  const handleToolkitsChange = useCallback(
    (newValue: string[]) => {
      if (!setYamlJsonObject) return;
      const currentToolNames = (yamlNode?.['tool_names'] as Record<string, readonly string[]> | undefined) ?? {};
      const updatedToolNames: Record<string, readonly string[]> = {};

      for (const toolkitName of newValue) {
        const availableTools = availableToolNames(findToolkit(versionTools, toolkitName, getToolkitNameFromSchema));
        const existing = currentToolNames[toolkitName];
        updatedToolNames[toolkitName] = existing ? existing.filter(tool => availableTools.includes(tool)) : availableTools;
      }

      FlowEditorHelpers.updateYamlNode(id, 'tool_names', updatedToolNames, yamlJsonObject, setYamlJsonObject);
      onValueChange?.(newValue);
    },
    [id, onValueChange, setYamlJsonObject, yamlJsonObject, yamlNode, versionTools, getToolkitNameFromSchema],
  );

  return (
    <PipelineMultiSelect
      label={label}
      value={selectedToolkits}
      onValueChange={handleToolkitsChange}
      options={toolkitOptions}
      disabled={disabled || toolkitOptions.length === 0}
      className="nopan nodrag"
    />
  );
}

/** The option value is the schema-derived name, so that match wins; `toolkit_name` is the fallback (#2003). */
function findToolkit(
  versionTools: readonly PipelineToolEntry[],
  toolkitName: string,
  nameFromSchema: (tool: PipelineToolEntry) => string,
): PipelineToolEntry | undefined {
  return versionTools.find(candidate => nameFromSchema(candidate) === toolkitName) ?? versionTools.find(candidate => candidate.toolkit_name === toolkitName);
}

function availableToolNames(toolkit: PipelineToolEntry | undefined): string[] {
  return (toolkit?.tools ?? toolkit?.settings?.selected_tools ?? []).map(tool => (typeof tool === 'string' ? tool : (tool.name ?? '')));
}
