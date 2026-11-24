import type { ReactNode } from 'react';
import { useCallback, useContext, useMemo } from 'react';

import { t } from '@/shared/i18n';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import { getNodeFieldInfo, NODE_FIELD_KEYS } from '../../lib/flow-editor/constants/nodeFieldInfo.constants';
import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import * as FlowEditorHelpers from '../../lib/flow-editor/helpers/flowEditor.helpers';
import { useGetToolkitNameFromSchema } from '../../lib/flow-editor/hooks/useGetToolkitNameFromSchema';
import { useSelectedProjectId } from '../../lib/flow-editor/hooks/useSelectedProjectId';
import { useToolkitTypeSchemas } from '../../lib/flow-editor/hooks/useToolkitTypeSchemas';
import type { PickerToolEntry, ToolkitListRow } from '../../lib/tool-picker/toolPicker.types';
import { ProjectToolkitsMultiPicker } from '../tool-picker/ProjectToolkitsMultiPicker';
import type { PipelineToolEntry } from './pipelineToolEntry.types';

export interface ToolkitsSelectProps {
  readonly id: string;
  readonly label?: string;
  readonly disabled?: boolean | undefined;
  readonly onValueChange?: (value: string[]) => void;
  readonly allowApplications?: boolean;
  readonly versionTools?: readonly PipelineToolEntry[];
}

const FORBIDDEN_TOOLKIT_KEYS = new Set(['__proto__', 'constructor', 'prototype']);

function isSafeToolkitKey(key: string): boolean {
  return key.length > 0 && !FORBIDDEN_TOOLKIT_KEYS.has(key);
}

/**
 * Grouped Toolkits & MCPs multi-picker for LLM nodes (Wave U2 Part A).
 * Backed by Wave U1's `ProjectToolkitsMultiPicker` (`attachedOnly`) while preserving
 * `ToolkitsSelect`'s YAML `tool_names` write semantics and prototype-key safety.
 */
export function ToolkitsSelect(props: ToolkitsSelectProps): ReactNode {
  const { id, label, disabled = false, onValueChange, versionTools = [] } = props;

  const context = useContext(FlowEditorContext);
  const setYamlJsonObject = context?.setYamlJsonObject;
  const yamlJsonObject = context?.yamlJsonObject;

  const projectId = useSelectedProjectId();
  const { toolkitTypeSchemas } = useToolkitTypeSchemas(projectId);
  const { getToolkitNameFromSchema } = useGetToolkitNameFromSchema(toolkitTypeSchemas);

  const yamlNode = useMemo(
    () => yamlJsonObject?.nodes?.find((node) => node.id === id),
    [id, yamlJsonObject?.nodes],
  );

  const selectedToolkits = useMemo(() => {
    const toolNames = (yamlNode?.['tool_names'] as Record<string, readonly string[]> | undefined) ?? {};
    return Object.keys(toolNames).filter(isSafeToolkitKey);
  }, [yamlNode]);

  const applySelection = useCallback(
    (nextNames: string[]) => {
      if (!setYamlJsonObject) return;
      const rawCurrent = yamlNode?.['tool_names'];
      const currentToolNames =
        rawCurrent && typeof rawCurrent === 'object' && !Array.isArray(rawCurrent)
          ? (rawCurrent as Record<string, unknown>)
          : {};
      const entries: [string, readonly string[]][] = [];

      for (const toolkitName of nextNames) {
        if (!isSafeToolkitKey(toolkitName)) continue;
        const availableTools = availableToolNames(findToolkit(versionTools, toolkitName, getToolkitNameFromSchema));
        const existing = Object.hasOwn(currentToolNames, toolkitName) ? currentToolNames[toolkitName] : undefined;
        const nextTools = Array.isArray(existing)
          ? existing.filter((tool): tool is string => typeof tool === 'string' && (availableTools.length === 0 || availableTools.includes(tool)))
          : availableTools;
        entries.push([toolkitName, nextTools]);
      }

      const updatedToolNames = Object.fromEntries(entries);
      FlowEditorHelpers.updateYamlNode(id, 'tool_names', updatedToolNames, yamlJsonObject, setYamlJsonObject);
      onValueChange?.(entries.map(([key]) => key));
    },
    [id, onValueChange, setYamlJsonObject, yamlJsonObject, yamlNode, versionTools, getToolkitNameFromSchema],
  );

  const handleAdd = useCallback(
    (_source: PickerToolEntry | ToolkitListRow, toolkitName: string) => {
      if (!isSafeToolkitKey(toolkitName) || selectedToolkits.includes(toolkitName)) return;
      applySelection([...selectedToolkits, toolkitName]);
    },
    [applySelection, selectedToolkits],
  );

  const handleRemove = useCallback(
    (toolkitName: string) => {
      applySelection(selectedToolkits.filter((name) => name !== toolkitName));
    },
    [applySelection, selectedToolkits],
  );

  const handleClear = useCallback(() => {
    applySelection([]);
  }, [applySelection]);

  const pickerContext = useMemo(
    () => ({
      attachedTools: versionTools as readonly PickerToolEntry[],
      projectId,
      yamlJsonObject,
    }),
    [projectId, versionTools, yamlJsonObject],
  );

  const tooltipText = getNodeFieldInfo(FlowEditorConstants.PipelineNodeTypes.LLM, NODE_FIELD_KEYS.toolkits);

  return (
    <ProjectToolkitsMultiPicker
      label={label ?? t('pipelines.flowEditor.llmNode.toolkitsLabel', 'Toolkits')}
      tooltip={tooltipText || undefined}
      value={selectedToolkits}
      onAdd={handleAdd}
      onRemove={handleRemove}
      onClear={handleClear}
      disabled={disabled}
      attachedOnly
      pickerContext={pickerContext}
      data-testid="pipeline-llm-toolkits-picker"
    />
  );
}

/** The option value is the schema-derived name, so that match wins; `toolkit_name` is the fallback (#2003). */
function findToolkit(
  versionTools: readonly PipelineToolEntry[],
  toolkitName: string,
  nameFromSchema: (tool: PipelineToolEntry) => string,
): PipelineToolEntry | undefined {
  return (
    versionTools.find((candidate) => nameFromSchema(candidate) === toolkitName) ??
    versionTools.find((candidate) => (candidate.toolkit_name || nameFromSchema(candidate)) === toolkitName)
  );
}

function availableToolNames(toolkit: PipelineToolEntry | undefined): string[] {
  return (toolkit?.tools ?? toolkit?.settings?.selected_tools ?? []).map((tool) =>
    typeof tool === 'string' ? tool : (tool.name ?? ''),
  );
}
