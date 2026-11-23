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
import { CommonInterruptSettings } from '../settings/CommonInterruptSettings';
import { InputMapping } from '../settings/InputMappings/InputMapping';
import { NodeSettings } from '../settings/NodeSettings';
import { InputSelect } from '../select/InputSelect';
import { OutputSelect } from '../select/OutputSelect';
import type { PipelineToolEntry } from '../select/pipelineToolEntry.types';
import { ProjectToolkitPicker } from '../tool-picker/ProjectToolkitPicker';
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
  readonly yamlJsonObject: FlowEditorContextValue['yamlJsonObject'] | undefined;
  readonly setYamlJsonObject: FlowEditorContextValue['setYamlJsonObject'] | undefined;
  readonly yamlNode: import('../../lib/flow-editor/helpers/pipelineFlow.types').YamlPipelineNode | undefined;
}

/** Groups every `FlowEditorContext`-derived read this component needs -- split out purely to keep `BaseToolNode` under the §3.5 complexity ceiling (each optional-chain read counts as a branch). */
function useBaseToolNodeYamlState(id: string): BaseToolNodeYamlState {
  const flowEditorContext = useContext(FlowEditorContext);
  const yamlJsonObject = flowEditorContext?.yamlJsonObject;
  const yamlNode = useMemo(() => yamlJsonObject?.nodes?.find(node => node.id === id), [id, yamlJsonObject?.nodes]);

  return {
    isRunningPipeline: flowEditorContext?.isRunningPipeline,
    yamlJsonObject,
    setYamlJsonObject: flowEditorContext?.setYamlJsonObject,
    yamlNode,
  };
}

interface BaseToolNodeHandlesProps {
  readonly isRunningPipeline: boolean | undefined;
  readonly isPerforming: boolean | undefined;
}

/** `BaseToolNode.jsx:121-140` (the `handles` render-prop body) as its own named component -- see `useBaseToolNodeYamlState`'s doc comment for why. */
function BaseToolNodeHandles({ isRunningPipeline, isPerforming }: BaseToolNodeHandlesProps): ReactNode {
  return (
    <>
      <CustomHandle
        type="target"
        id="target"
        isConnectable={!isRunningPipeline}
        isRunningPipeline={Boolean(isRunningPipeline)}
        isPerforming={Boolean(isPerforming)}
      />
      <CustomHandle
        type="source"
        id="source"
        isConnectable={!isRunningPipeline}
        isRunningPipeline={Boolean(isRunningPipeline)}
        isPerforming={Boolean(isPerforming)}
      />
    </>
  );
}

interface ToolFunctionSelectProps {
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

/**
 * `BaseToolNode.jsx:149-161` (the conditional "Tool" select) as its own
 * named component -- split out purely to keep `BaseToolNode` under the §3.5
 * complexity ceiling.
 *
 * NO EMPTY LIST FOR A FAILED READ (#440). The node used to render nothing at
 * all when the option list was empty, whatever the cause. An empty list now
 * keeps one meaning — the toolkit offers no tools — and a failed read gets
 * its own visible state with a retry. A failed read that still produced
 * options (the toolkit carries its own `selected_tools`, and only the type
 * schemas were lost) keeps the working picker: the list on screen is real.
 */
function ToolFunctionSelect({ functionOptions, selectedTool, onChangeTool, onClearTool, disabled, catalogueReadFailed, catalogueDiscoveryDisabled, typeSchemasReadFailed, onRetry }: ToolFunctionSelectProps): ReactNode {
  if (functionOptions.length > 0) {
    return (
      <div className="nopan nodrag nowheel">
        <SingleSelect
          label={t('pipelines.flowEditor.baseToolNode.toolLabel', 'Tool')}
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
} {
  const isMcpNode = nodeType === PipelineNodeTypes.Mcp;
  const tooltip = getNodeFieldInfo(nodeType, isMcpNode ? NODE_FIELD_KEYS.mcp : NODE_FIELD_KEYS.toolkit) || undefined;
  const testId = isMcpNode ? 'pipeline-mcp-node-picker' : 'pipeline-toolkit-node-picker';
  return { isMcpNode, tooltip, testId };
}

function resolvePickerValue(
  selectedToolkit: PipelineToolEntry | undefined,
  toolkit: string | undefined,
  customFilterTypes: (tool: PipelineToolEntry) => boolean,
): string {
  return selectedToolkit && customFilterTypes(selectedToolkit) ? (toolkit ?? '') : '';
}

function BaseToolNodeAdvancedSection({
  id,
  nodeType,
  showStructuredOutput,
  disabled,
}: {
  readonly id: string;
  readonly nodeType: string;
  readonly showStructuredOutput: boolean;
  readonly disabled: boolean;
}): ReactNode {
  return (
    <NodeConfigSection section="advanced" defaultExpanded={false}>
      <NodeFieldHeading label={t('pipelines.flowEditor.baseToolNode.inputLabel', 'Input')} nodeType={nodeType} fieldKey={NODE_FIELD_KEYS.input} />
      <InputSelect id={id} inputFieldName="input" disabled={disabled} />
      <NodeFieldHeading label={t('pipelines.flowEditor.baseToolNode.outputLabel', 'Output')} nodeType={nodeType} fieldKey={NODE_FIELD_KEYS.output} />
      <OutputSelect id={id} label={t('pipelines.flowEditor.baseToolNode.outputLabel', 'Output')} outputFieldName="output" />
      <CommonInterruptSettings id={id} showStructuredOutput={showStructuredOutput} type={nodeType} disabled={disabled} />
      <NodeSettings id={id} type={nodeType} disabled={disabled} />
    </NodeConfigSection>
  );
}

export const BaseToolNode = memo(function BaseToolNode(props: BaseToolNodeProps): ReactNode {
  const { id, data, selected, nodeType, showStructuredOutput = false, customFilterTypes = defaultFilterTypes, versionTools } = props;

  const { isRunningPipeline, yamlJsonObject, setYamlJsonObject, yamlNode } = useBaseToolNodeYamlState(id);

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

  const onClearTool = useCallback(() => {
    onChangeTool(undefined);
  }, [onChangeTool]);

  // Both reads feed the same picker, so one retry control must run both.
  const onRetryToolList = useCallback(() => {
    retryDynamicToolsRead();
    retryTypeSchemasRead();
  }, [retryDynamicToolsRead, retryTypeSchemasRead]);

  const onSelectPickerToolkit = useCallback(
    (source: PickerToolEntry | ToolkitListRow, toolkitName: string) => {
      const matched = (versionTools ?? []).find(
        (entry) => (entry.toolkit_name || getToolkitNameFromSchema(entry)) === toolkitName,
      );
      const nextToolkit: PipelineToolEntry = matched ?? {
        ...(source as unknown as PipelineToolEntry),
        toolkit_name: toolkitName,
      };
      onSelectToolkit(nextToolkit);
    },
    [getToolkitNameFromSchema, onSelectToolkit, versionTools],
  );

  const onClearToolkit = useCallback(() => {
    onSelectToolkit(null);
  }, [onSelectToolkit]);

  const pickerMeta = resolvePickerToolkitMeta(nodeType);
  const pickerContext = useMemo(
    () => ({
      attachedTools: (versionTools ?? []).filter(customFilterTypes) as readonly PickerToolEntry[],
      projectId,
      yamlJsonObject,
    }),
    [customFilterTypes, projectId, versionTools, yamlJsonObject],
  );
  const isDisabled = Boolean(isRunningPipeline);

  const resolvedPickerValue = resolvePickerValue(selectedToolkit, toolkit, customFilterTypes);

  return (
    <NodeCard
      name={id}
      isEntrypoint={yamlJsonObject?.entry_point === id}
      selected={Boolean(selected)}
      type={nodeType}
      isPerforming={Boolean(data?.isPerforming)}
      id={id}
      requiredInputs={requiredInputs}
      handles={() => <BaseToolNodeHandles isRunningPipeline={isRunningPipeline} isPerforming={data?.isPerforming} />}
    >
      <NodeConfigSection section="basic">
        <ProjectToolkitPicker
          mcp={pickerMeta.isMcpNode}
          tooltip={pickerMeta.tooltip}
          value={resolvedPickerValue}
          onSelect={onSelectPickerToolkit}
          onClear={onClearToolkit}
          disabled={isDisabled}
          pickerContext={pickerContext}
          data-testid={pickerMeta.testId}
        />
        <ToolFunctionSelect
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
        <InputMapping
          requiredInputs={requiredInputs}
          mappingInfo={mappingInfo as never}
          input_mapping={inputMappings as never}
          defaultValues={defaultValues}
          values={yamlNode?.input_mapping ?? {}}
          onChangeMapping={onChangeMapping}
          disabled={isDisabled}
        />
      </NodeConfigSection>
      <BaseToolNodeAdvancedSection id={id} nodeType={nodeType} showStructuredOutput={showStructuredOutput} disabled={isDisabled} />
    </NodeCard>
  );
});
