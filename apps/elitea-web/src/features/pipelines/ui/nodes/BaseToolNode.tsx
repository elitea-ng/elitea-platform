/**
 * Shared body for `McpNode` and `ToolkitNode` (Wave U2 Part A).
 * Uses `ProjectToolkitPicker` inside Basic and `InputSelect`/`OutputSelect`/
 * `CommonInterruptSettings`/`NodeSettings` inside Advanced.
 */
import type { ReactNode } from 'react';
import { memo, useCallback, useContext, useMemo } from 'react';

import { SingleSelect } from '@/shared/ui/SingleSelect';
import { ToolListError } from '@/shared/ui/ToolListError';
import { ToolTypes } from '@/entities/toolkit';

import { PipelineNodeTypes } from '../../lib/flow-editor/constants/flowEditor.constants';
import { getNodeFieldInfo, NODE_FIELD_KEYS } from '../../lib/flow-editor/constants/nodeFieldInfo.constants';
import { FlowEditorContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import { batchUpdateYamlNode, getDefaultInputMappingOfTool, getToolName } from '../../lib/flow-editor/helpers/flowEditor.helpers';
import { useFunctionInputMapping } from '../../lib/flow-editor/hooks/useFunctionInputMapping';
import { useGetToolkitNameFromSchema } from '../../lib/flow-editor/hooks/useGetToolkitNameFromSchema';
import { useToolkitTypeSchemas } from '../../lib/flow-editor/hooks/useToolkitTypeSchemas';
import { useSelectedProjectId } from '../../lib/flow-editor/hooks/useSelectedProjectId';
import type { PickerToolEntry, ToolkitListRow } from '../../lib/tool-picker/toolPicker.types';
import { NodeConfigSection, NodeFieldHeading } from '../node-config-drawer/NodeConfigSection';
import type { InputMappingFieldsProps } from '../settings/InputMappings/InputMappingFields';
import type { PipelineToolEntry } from '../select/pipelineToolEntry.types';
import { ProjectToolkitPicker } from '../tool-picker/ProjectToolkitPicker';
import {
  BaseToolNodeAdvancedSection,
  BaseToolNodeBasicMappings,
  McpDisconnectedHint,
  useToolNodeParameterPartition,
} from './BaseToolNode.parts';
import { NodeCard } from './BaseNode/NodeCard';
import { CustomHandle } from './CustomHandle';
import { t } from '@/shared/i18n';

const defaultFilterTypes = (tool: PipelineToolEntry): boolean => tool.type !== ToolTypes.application.value;

interface FunctionOption {
  readonly label: string;
  readonly value: string;
}

/** One `settings.selected_tools[]` entry -- either a bare tool name, or the object shape `{name, description, path}` `getToolName` exists to normalize (real, documented shape; the same union `../../lib/flow-editor/helpers/flowEditor.helpers.ts`'s `getToolName` itself, and `deprecated/useToolNodeEditing.ts`/`select/LoopToolSelect.tsx`/`DefaultNode.tsx`'s own `computeFunctionOptions`-equivalents, already handle). */
type SelectedToolEntry = string | { readonly name?: string; readonly description?: string; readonly path?: string };

/**
 * Normalizes and sorts enabled tool names for the selected toolkit.
 */
export function computeFunctionOptions(
  selectedToolkit: { readonly settings?: Readonly<Record<string, unknown>>; readonly type?: string } | undefined,
  getSelectedTools: (type: string) => readonly string[],
  dynamicToolNames: readonly string[],
): readonly FunctionOption[] {
  const explicitSelected = selectedToolkit?.settings?.['selected_tools'] as readonly SelectedToolEntry[] | undefined;
  const hasExplicitSelection = Array.isArray(explicitSelected) && explicitSelected.length > 0;
  const availableTools = getSelectedTools(selectedToolkit?.type ?? '');
  const hasAvailableCheck = Array.isArray(availableTools) && availableTools.length > 0;

  const enabledTools: readonly SelectedToolEntry[] =
    hasExplicitSelection && hasAvailableCheck
      ? explicitSelected.filter((tool: SelectedToolEntry) => availableTools.includes(getToolName(tool)))
      : hasExplicitSelection
        ? explicitSelected
        : dynamicToolNames;

  return enabledTools
    .map(item => {
      const name = getToolName(item);
      return { label: name, value: name };
    })
    .sort((a, b) => a.label.localeCompare(b.label));
}

interface ApplyToolkitSelectionArgs {
  readonly id: string;
  readonly newToolkit: PipelineToolEntry | null;
  readonly toolkitTypes: unknown;
  readonly currentInputMapping: unknown;
  readonly yamlJsonObject: FlowEditorContextValue['yamlJsonObject'] | undefined;
  readonly setYamlJsonObject: FlowEditorContextValue['setYamlJsonObject'] | undefined;
  readonly getToolkitNameFromSchema: (toolkit: PipelineToolEntry) => string;
}

/** Clears `toolkit_name`/`tool`/`input_mapping` together when toolkit is unset (#2432). */
export function applyToolkitSelection({
  id,
  newToolkit,
  toolkitTypes,
  currentInputMapping,
  yamlJsonObject,
  setYamlJsonObject,
  getToolkitNameFromSchema,
}: ApplyToolkitSelectionArgs): void {
  if (!yamlJsonObject || !setYamlJsonObject) return;
  if (!newToolkit) {
    batchUpdateYamlNode(id, { toolkit_name: undefined, tool: undefined, input_mapping: undefined }, yamlJsonObject, setYamlJsonObject);
    return;
  }
  const { mapping } = getDefaultInputMappingOfTool(toolkitTypes as never, undefined, currentInputMapping as never, newToolkit as never);
  batchUpdateYamlNode(
    id,
    { toolkit_name: newToolkit.toolkit_name ?? getToolkitNameFromSchema(newToolkit), tool: undefined, input_mapping: { ...mapping } },
    yamlJsonObject,
    setYamlJsonObject,
  );
}

export interface BaseToolNodeProps {
  readonly id: string;
  readonly data?: { readonly isPerforming?: boolean };
  readonly selected?: boolean;
  readonly nodeType: string;
  readonly showStructuredOutput?: boolean;
  readonly customFilterTypes?: (tool: PipelineToolEntry) => boolean;
  readonly versionTools?: readonly PipelineToolEntry[];
}

interface BaseToolNodeYamlState {
  readonly isRunningPipeline: boolean | undefined;
  readonly disabled: boolean | undefined;
  readonly yamlJsonObject: FlowEditorContextValue['yamlJsonObject'] | undefined;
  readonly setYamlJsonObject: FlowEditorContextValue['setYamlJsonObject'] | undefined;
  readonly yamlNode: import('../../lib/flow-editor/helpers/pipelineFlow.types').YamlPipelineNode | undefined;
}

function useBaseToolNodeYamlState(id: string): BaseToolNodeYamlState {
  const flowEditorContext = useContext(FlowEditorContext);
  const yamlJsonObject = flowEditorContext?.yamlJsonObject;
  const yamlNode = useMemo(() => yamlJsonObject?.nodes?.find(node => node.id === id), [id, yamlJsonObject?.nodes]);

  return {
    isRunningPipeline: flowEditorContext?.isRunningPipeline,
    disabled: flowEditorContext?.disabled,
    yamlJsonObject,
    setYamlJsonObject: flowEditorContext?.setYamlJsonObject,
    yamlNode,
  };
}

interface BaseToolNodeHandlesProps {
  readonly isRunningPipeline: boolean | undefined;
  readonly disabled: boolean | undefined;
  readonly isPerforming: boolean | undefined;
}

function BaseToolNodeHandles({ isRunningPipeline, disabled, isPerforming }: BaseToolNodeHandlesProps): ReactNode {
  const isConnectable = !isRunningPipeline && !disabled;
  return (
    <>
      <CustomHandle
        type="target"
        id="target"
        isConnectable={isConnectable}
        isRunningPipeline={Boolean(isRunningPipeline)}
        isPerforming={Boolean(isPerforming)}
      />
      <CustomHandle
        type="source"
        id="source"
        isConnectable={isConnectable}
        isRunningPipeline={Boolean(isRunningPipeline)}
        isPerforming={Boolean(isPerforming)}
      />
    </>
  );
}

interface ToolFunctionSelectProps {
  readonly nodeType: string;
  readonly functionOptions: readonly FunctionOption[];
  readonly selectedTool: string;
  readonly onChangeTool: (newValue: string | undefined) => void;
  readonly onClearTool: () => void;
  readonly disabled: boolean;
  /** The tool catalogue read failed (#440). */
  readonly catalogueReadFailed: boolean;
  /** The catalogue read failed because this deployment turned tool discovery off (#440). */
  readonly catalogueDiscoveryDisabled: boolean;
  /** The toolkit type schema read failed (#440) — it supplies the tool list of a statically-declared type. */
  readonly typeSchemasReadFailed: boolean;
  readonly onRetry: () => void;
}

function ToolFunctionSelect({ nodeType, functionOptions, selectedTool, onChangeTool, onClearTool, disabled, catalogueReadFailed, catalogueDiscoveryDisabled, typeSchemasReadFailed, onRetry }: ToolFunctionSelectProps): ReactNode {
  if (functionOptions.length > 0) {
    const toolLabel = t('pipelines.flowEditor.baseToolNode.toolLabel', 'Tool');
    return (
      <div className="nopan nodrag nowheel">
        <NodeFieldHeading label={toolLabel} nodeType={nodeType} fieldKey={NODE_FIELD_KEYS.tool} />
        <SingleSelect
          label={toolLabel}
          value={selectedTool}
          onChange={onChangeTool}
          options={[...functionOptions]}
          disabled={disabled}
          onClear={onClearTool}
        />
      </div>
    );
  }
  if (catalogueReadFailed || typeSchemasReadFailed) {
    return (
      <ToolListError
        onRetry={onRetry}
        discoveryDisabled={catalogueDiscoveryDisabled && !typeSchemasReadFailed}
      />
    );
  }
  return null;
}

function resolvePickerToolkitMeta(nodeType: string): {
  readonly isMcpNode: boolean;
  readonly tooltip: string | undefined;
  readonly testId: string;
  readonly testIdPrefix: string;
} {
  const isMcpNode = nodeType === PipelineNodeTypes.Mcp;
  const tooltip = getNodeFieldInfo(nodeType, isMcpNode ? NODE_FIELD_KEYS.mcp : NODE_FIELD_KEYS.toolkit) || undefined;
  const testIdPrefix = isMcpNode ? 'pipeline-mcp-node' : 'pipeline-toolkit-node';
  const testId = `${testIdPrefix}-picker`;
  return { isMcpNode, tooltip, testId, testIdPrefix };
}

function resolvePickerValue(
  selectedToolkit: PipelineToolEntry | undefined,
  toolkit: string | undefined,
  customFilterTypes: (tool: PipelineToolEntry) => boolean,
): string {
  return selectedToolkit && customFilterTypes(selectedToolkit) ? (toolkit ?? '') : '';
}

const EMPTY_MAPPING: Readonly<Record<string, import('../../lib/flow-editor/helpers/pipelineFlow.types').YamlInputMappingEntry>> = {};

export const BaseToolNode = memo(function BaseToolNode(props: BaseToolNodeProps): ReactNode {
  const { id, data, selected, nodeType, showStructuredOutput = false, customFilterTypes = defaultFilterTypes, versionTools } = props;

  const { isRunningPipeline, disabled, yamlJsonObject, setYamlJsonObject, yamlNode } = useBaseToolNodeYamlState(id);

  const {
    onChangeTool,
    onChangeMapping,
    toolkitTypes,
    requiredInputs,
    mappingInfo,
    selectedTool,
    toolkit,
    selectedToolkit,
    dynamicToolNames,
    dynamicToolsReadFailed,
    dynamicToolsDiscoveryDisabled,
    retryDynamicToolsRead,
    inputMappings,
    defaultValues,
  } = useFunctionInputMapping({
    id,
    yamlJsonObject: yamlJsonObject ?? {},
    setYamlJsonObject: setYamlJsonObject ?? (() => {}),
    versionTools,
  });

  const projectId = useSelectedProjectId();
  const { toolkitTypeSchemas, isError: typeSchemasReadFailed, refetch: retryTypeSchemasRead } = useToolkitTypeSchemas(projectId);
  const { getToolkitNameFromSchema, getSelectedTools } = useGetToolkitNameFromSchema(toolkitTypeSchemas);

  const functionOptions = useMemo(
    () => computeFunctionOptions(selectedToolkit, getSelectedTools, dynamicToolNames),
    [dynamicToolNames, getSelectedTools, selectedToolkit],
  );

  const onSelectToolkit = useCallback(
    (newToolkit: PipelineToolEntry | null) => {
      applyToolkitSelection({
        id,
        newToolkit,
        toolkitTypes,
        currentInputMapping: yamlNode?.input_mapping,
        yamlJsonObject,
        setYamlJsonObject,
        getToolkitNameFromSchema,
      });
    },
    [getToolkitNameFromSchema, id, setYamlJsonObject, toolkitTypes, yamlJsonObject, yamlNode?.input_mapping],
  );

  const onClearTool = useCallback(() => onChangeTool(undefined), [onChangeTool]);
  const onRetryToolList = useCallback(() => {
    retryDynamicToolsRead();
    retryTypeSchemasRead();
  }, [retryDynamicToolsRead, retryTypeSchemasRead]);
  const onSelectPickerToolkit = useCallback(
    (source: PickerToolEntry | ToolkitListRow, toolkitName: string) => {
      const matched = (versionTools ?? []).find(
        (entry) => (entry.toolkit_name || getToolkitNameFromSchema(entry)) === toolkitName,
      );
      onSelectToolkit(matched ?? { ...(source as unknown as PipelineToolEntry), toolkit_name: toolkitName });
    },
    [getToolkitNameFromSchema, onSelectToolkit, versionTools],
  );
  const onClearToolkit = useCallback(() => onSelectToolkit(null), [onSelectToolkit]);

  const pickerMeta = resolvePickerToolkitMeta(nodeType);
  const pickerContext = useMemo(
    () => ({
      attachedTools: (versionTools ?? []).filter(customFilterTypes) as readonly PickerToolEntry[],
      projectId,
      yamlJsonObject,
    }),
    [customFilterTypes, projectId, versionTools, yamlJsonObject],
  );
  const isDisabled = Boolean(isRunningPipeline || disabled);
  const resolvedPickerValue = resolvePickerValue(selectedToolkit, toolkit, customFilterTypes);
  const isDeprecatedLayout = nodeType === PipelineNodeTypes.Function;
  const currentValues = yamlNode?.input_mapping ?? EMPTY_MAPPING;
  const { requiredKeys, shownOptionalKeys, addableOptionalParameters, onRevealOptionalParameter } =
    useToolNodeParameterPartition({
      toolkit,
      selectedTool,
      inputMappings,
      requiredInputs,
      currentMappingValues: currentValues,
    });

  const mappingFieldsProps: Omit<InputMappingFieldsProps, 'keys'> = {
    input_mapping: inputMappings as never,
    mappingInfo: mappingInfo as never,
    defaultValues,
    values: currentValues,
    onChangeMapping,
    disabled: isDisabled,
    nodeType,
  };

  return (
    <NodeCard
      name={id}
      isEntrypoint={yamlJsonObject?.entry_point === id}
      selected={Boolean(selected)}
      type={nodeType}
      isPerforming={Boolean(data?.isPerforming)}
      id={id}
      requiredInputs={requiredInputs}
      handles={() => <BaseToolNodeHandles isRunningPipeline={isRunningPipeline} disabled={disabled} isPerforming={data?.isPerforming} />}
    >
      <NodeConfigSection section="basic">
        <ProjectToolkitPicker
          mcp={pickerMeta.isMcpNode}
          tooltip={pickerMeta.tooltip}
          value={resolvedPickerValue}
          onSelect={onSelectPickerToolkit}
          onClear={onClearToolkit}
          disabled={isDisabled}
          attachedOnly
          pickerContext={pickerContext}
          data-testid={pickerMeta.testId}
        />
        {pickerMeta.isMcpNode ? <McpDisconnectedHint mcp={selectedToolkit} /> : null}
        <ToolFunctionSelect
          nodeType={nodeType}
          functionOptions={functionOptions}
          selectedTool={selectedTool}
          onChangeTool={onChangeTool}
          onClearTool={onClearTool}
          disabled={isDisabled}
          catalogueReadFailed={dynamicToolsReadFailed}
          catalogueDiscoveryDisabled={dynamicToolsDiscoveryDisabled}
          typeSchemasReadFailed={typeSchemasReadFailed}
          onRetry={onRetryToolList}
        />
        <BaseToolNodeBasicMappings
          isDeprecatedLayout={isDeprecatedLayout}
          requiredKeys={requiredKeys}
          requiredInputs={requiredInputs}
          mappingProps={mappingFieldsProps}
        />
      </NodeConfigSection>
      <BaseToolNodeAdvancedSection
        id={id}
        nodeType={nodeType}
        showStructuredOutput={showStructuredOutput}
        disabled={isDisabled}
        isDeprecatedLayout={isDeprecatedLayout}
        shownOptionalKeys={shownOptionalKeys}
        addableOptionalParameters={addableOptionalParameters}
        onRevealOptionalParameter={onRevealOptionalParameter}
        mappingProps={mappingFieldsProps}
        testIdPrefix={pickerMeta.testIdPrefix}
      />
    </NodeCard>
  );
});
