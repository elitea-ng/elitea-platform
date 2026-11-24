/**
 * Pipeline canvas & config-drawer renderer for `type: agent` nodes.
 * Uses `ProjectAgentPicker`, read-only/callback-gated `AgentVersionSelect`, and
 * Basic/Advanced `InputMappingFields` sections with Q6 interrupts and `NodeSettings`.
 */
import type { ReactNode } from 'react';
import { memo, useCallback, useContext, useMemo } from 'react';

import WarningAmberIcon from '@mui/icons-material/WarningAmber';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useEdges, type NodeProps } from '@xyflow/react';

import { ToolTypes } from '@/entities/toolkit';
import { t } from '@/shared/i18n';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import { getNodeFieldInfo, NODE_FIELD_KEYS } from '../../lib/flow-editor/constants/nodeFieldInfo.constants';
import { batchUpdateYamlNode } from '../../lib/flow-editor/helpers/flowEditor.helpers';
import { FlowEditorContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import type { YamlInputMappingEntry } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import { useFunctionInputMapping } from '../../lib/flow-editor/hooks/useFunctionInputMapping';
import { useGetToolkitNameFromSchema } from '../../lib/flow-editor/hooks/useGetToolkitNameFromSchema';
import { useSelectedProjectId } from '../../lib/flow-editor/hooks/useSelectedProjectId';
import type { FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import type { ApplicationListRow, PickerItem, PickerToolEntry } from '../../lib/tool-picker/toolPicker.types';
import { NodeConfigSection, NodeFieldHeading } from '../node-config-drawer/NodeConfigSection';
import { CommonInterruptSettings } from '../settings/CommonInterruptSettings';
import { InputMappingFields, type InputMappingFieldsProps } from '../settings/InputMappings/InputMappingFields';
import { NodeSettings } from '../settings/NodeSettings';
import { InputSelect } from '../select/InputSelect';
import { OutputSelect } from '../select/OutputSelect';
import type { PipelineToolEntry } from '../select/pipelineToolEntry.types';
import { AgentVersionSelect, type AgentVersionSelectProps } from '../tool-picker/AgentVersionSelect';
import { ProjectAgentPicker } from '../tool-picker/ProjectAgentPicker';
import { CustomHandle } from './CustomHandle';
import { NodeCard } from './BaseNode/NodeCard';

const toolkitFilter = (tool: PipelineToolEntry): boolean => tool.type === ToolTypes.application.value;
const EMPTY_TOOLKIT_PATCH: Record<string, unknown> = { toolkit_name: undefined, tool: undefined, input_mapping: undefined };
/** Typed to match `yamlNode?.input_mapping`'s own `Readonly<Record<string, YamlInputMappingEntry>>` (`pipelineFlow.types.ts`), not a bare `Record<string, unknown>` -- so `inputMappingValues = yamlNode?.input_mapping ?? EMPTY_RECORD` stays assignable to `InputMapping`'s `values` prop below without a cast. */
const EMPTY_RECORD: Readonly<Record<string, YamlInputMappingEntry>> = {};
/** Stable empty-array identity — `resolvedVersionTools = versionTools ?? EMPTY_TOOLS` avoids a fresh `[]` literal (and the resulting "changes every render" `useMemo`/`useCallback` dep warning) whenever the caller omits `versionTools`. */
const EMPTY_TOOLS: readonly PipelineToolEntry[] = [];

export interface AgentNodeProps extends NodeProps<FlowNode> {
  /** Replaces the baseline's ambient `values.version_details.tools` read — see module doc comment. */
  readonly versionTools?: readonly PipelineToolEntry[] | undefined;
  /** Optional version-change persistence callback; when omitted, `AgentVersionSelect` renders read-only. */
  readonly onVersionChange?: AgentVersionSelectProps['onVersionChange'] | undefined;
}

interface AgentNodeHandlesProps {
  readonly isRunningPipeline: boolean | undefined;
  readonly disabled: boolean | undefined;
  readonly isSourceConnectable: boolean;
  readonly isPerforming: boolean;
}

/** Extracted purely to keep `AgentNode`'s own cyclomatic complexity under the §3.5 budget (12) — same technique `ui/nodes/BaseNode/NodeCardHeader.tsx` (A2e) already uses for its own split-out pieces. */
function AgentNodeHandles(props: AgentNodeHandlesProps): ReactNode {
  const { isRunningPipeline, disabled, isSourceConnectable, isPerforming } = props;
  return (
    <>
      <CustomHandle
        type="target"
        id="target"
        isConnectable={!isRunningPipeline && !disabled}
        isRunningPipeline={Boolean(isRunningPipeline)}
        isPerforming={isPerforming}
      />
      <CustomHandle
        type="source"
        id="source"
        isConnectable={isSourceConnectable && !isRunningPipeline && !disabled}
        isRunningPipeline={Boolean(isRunningPipeline)}
        isPerforming={isPerforming}
      />
    </>
  );
}

/** Pure extraction of the baseline's `isOrphan` ternary chain — kept out of `AgentNode`'s own body purely to stay under the §3.5 complexity budget (12). */
function computeIsOrphan(boundTool: string | undefined, versionTools: readonly PipelineToolEntry[] | undefined): boolean {
  if (!boundTool) return false; // nothing bound — not an orphan
  const configuredTools = (versionTools ?? []).filter(toolkitFilter);
  return configuredTools.length === 0 || !configuredTools.some(tool => tool.name === boundTool);
}

/** Pure extraction of the baseline's `onSelectToolkit` patch-building ternaries — same complexity-budget reason as {@link computeIsOrphan}. */
function computeToolkitPatch(
  newToolkit: PipelineToolEntry,
  getToolkitNameFromSchema: (toolkit: PipelineToolEntry) => string,
): Record<string, unknown> {
  const isApplication = newToolkit.type === ToolTypes.application.value;
  return {
    toolkit_name: isApplication ? undefined : (newToolkit.toolkit_name ?? getToolkitNameFromSchema(newToolkit)),
    tool: isApplication ? newToolkit.name : undefined,
  };
}

/** `FlowEditorContext`, defaulted once — every one of `AgentNode`'s own reads becomes a plain property access with no further `?.`/`??`, to stay under the §3.5 complexity budget (12). */
const EMPTY_FLOW_EDITOR_CONTEXT: FlowEditorContextValue = {
  yamlJsonObject: {},
  setFlowNodes: () => {},
  setFlowEdges: () => {},
  setYamlJsonObject: () => {},
};

function AgentOrphanWarning(): ReactNode {
  return (
    <Box sx={agentNodeStyles.orphanWarning}>
      <WarningAmberIcon fontSize="small" />
      <Typography variant="bodySmall">
        {t(
          'pipelines.flowEditor.agentNode.orphanWarning',
          'Agent not found — select a replacement or delete this node',
        )}
      </Typography>
    </Box>
  );
}

function resolveAgentTooltip(): string | undefined {
  return getNodeFieldInfo(FlowEditorConstants.PipelineNodeTypes.Agent, NODE_FIELD_KEYS.agent) || undefined;
}

function resolveVersionTooltip(): string | undefined {
  return getNodeFieldInfo(FlowEditorConstants.PipelineNodeTypes.Agent, NODE_FIELD_KEYS.version) || undefined;
}

const TASK_KEY = 'task';
const CHAT_HISTORY_KEY = 'chat_history';

export function partitionAgentMappingKeys(
  inputMappings: Readonly<Record<string, unknown>> | undefined,
  requiredInputs: readonly string[],
): { readonly basicKeys: readonly string[]; readonly advancedKeys: readonly string[] } {
  const allKeys = Object.keys(inputMappings ?? {});
  const basicKeys = Array.from(new Set([TASK_KEY, ...requiredInputs]));
  const basicSet = new Set(basicKeys);
  const advancedCandidates = [
    ...(basicSet.has(CHAT_HISTORY_KEY) ? [] : [CHAT_HISTORY_KEY]),
    ...allKeys.filter((key) => !basicSet.has(key)),
  ];
  const advancedKeys = Array.from(new Set(advancedCandidates));
  return { basicKeys, advancedKeys };
}

function findAttachedAgentTool(
  boundTool: string | undefined,
  versionTools: readonly PipelineToolEntry[],
): PickerToolEntry | undefined {
  if (!boundTool) return undefined;
  return versionTools.find((tool) => toolkitFilter(tool) && tool.name === boundTool);
}

function AgentNodeVersionAndBasicMappings({
  isOrphan,
  attachedTool,
  projectId,
  disabled,
  onVersionChange,
  basicKeys,
  mappingFieldsProps,
}: {
  readonly isOrphan: boolean;
  readonly attachedTool: PickerToolEntry | undefined;
  readonly projectId: string | undefined;
  readonly disabled: boolean;
  readonly onVersionChange: AgentVersionSelectProps['onVersionChange'] | undefined;
  readonly basicKeys: readonly string[];
  readonly mappingFieldsProps: Omit<InputMappingFieldsProps, 'keys'>;
}): ReactNode {
  if (isOrphan) return null;
  return (
    <>
      {attachedTool ? (
        <AgentVersionSelect
          tool={attachedTool}
          tooltip={resolveVersionTooltip()}
          projectId={projectId}
          disabled={disabled || !onVersionChange}
          {...(onVersionChange ? { onVersionChange } : {})}
          data-testid="pipeline-agent-node-version-select"
        />
      ) : null}
      <InputMappingFields keys={basicKeys} {...mappingFieldsProps} />
    </>
  );
}

export const AgentNode = memo(function AgentNode(props: AgentNodeProps): ReactNode {
  const { id, data, selected, type: nodeType = FlowEditorConstants.PipelineNodeTypes.Agent, versionTools, onVersionChange } = props;

  const edges = useEdges();
  const flowEditorContext = useContext(FlowEditorContext) ?? EMPTY_FLOW_EDITOR_CONTEXT;
  const { yamlJsonObject, setYamlJsonObject, isRunningPipeline, disabled } = flowEditorContext;
  const resolvedVersionTools = versionTools ?? EMPTY_TOOLS;
  const isPerforming = Boolean(data?.isPerforming);
  const isFieldsDisabled = Boolean(isRunningPipeline) || Boolean(disabled);

  const yamlNode = useMemo(() => yamlJsonObject.nodes?.find(node => node.id === id), [id, yamlJsonObject.nodes]);
  const boundTool = yamlNode?.tool;

  const isSourceConnectable = useMemo(
    () => !edges.find(edge => edge.source === id && edge.target !== FlowEditorConstants.PipelineNodeTypes.End),
    [edges, id],
  );

  const isOrphan = useMemo(() => computeIsOrphan(boundTool, resolvedVersionTools), [boundTool, resolvedVersionTools]);

  const { toolkitTypes, onChangeMapping, requiredInputs, mappingInfo, inputMappings, defaultValues } =
    useFunctionInputMapping({
      id,
      yamlJsonObject,
      setYamlJsonObject,
      // `PipelineToolEntry` (A2h) and `VersionTool` (A2d) describe the same
      // real `version_details.tools[]` entry from two sub-units' narrower
      // read surfaces — see module doc comment.
      versionTools: resolvedVersionTools,
    });

  const projectId = useSelectedProjectId();
  const { getToolkitNameFromSchema } = useGetToolkitNameFromSchema(toolkitTypes as never);

  const onSelectAgentItem = useCallback(
    (item: PickerItem<PickerToolEntry | ApplicationListRow>) => {
      const matched = resolvedVersionTools.find((tool) => tool.name === item.name);
      const patch = matched
        ? computeToolkitPatch(matched, getToolkitNameFromSchema)
        : { toolkit_name: undefined, tool: item.name };
      batchUpdateYamlNode(id, patch, yamlJsonObject, setYamlJsonObject);
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [id, yamlJsonObject, setYamlJsonObject, getToolkitNameFromSchema, resolvedVersionTools],
  );

  const onClearAgent = useCallback(() => {
    batchUpdateYamlNode(id, EMPTY_TOOLKIT_PATCH, yamlJsonObject, setYamlJsonObject);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id, yamlJsonObject, setYamlJsonObject]);

  const pickerContext = useMemo(
    () => ({
      attachedTools: resolvedVersionTools as readonly PickerToolEntry[],
      projectId,
      yamlJsonObject,
    }),
    [projectId, resolvedVersionTools, yamlJsonObject],
  );

  const selectedToolkitValue = boundTool ?? '';
  const inputMappingValues = yamlNode?.input_mapping ?? EMPTY_RECORD;
  const attachedTool = useMemo(() => findAttachedAgentTool(boundTool, resolvedVersionTools), [boundTool, resolvedVersionTools]);
  const { basicKeys, advancedKeys } = useMemo(
    () => partitionAgentMappingKeys(inputMappings, requiredInputs),
    [inputMappings, requiredInputs],
  );
  const mappingFieldsProps: Omit<InputMappingFieldsProps, 'keys'> = {
    input_mapping: inputMappings as InputMappingFieldsProps['input_mapping'],
    mappingInfo: mappingInfo as InputMappingFieldsProps['mappingInfo'],
    defaultValues,
    values: inputMappingValues,
    onChangeMapping,
    disabled: isFieldsDisabled,
    nodeType: FlowEditorConstants.PipelineNodeTypes.Agent,
  };

  return (
    <NodeCard
      name={id}
      isEntrypoint={yamlJsonObject.entry_point === id}
      selected={selected}
      type={nodeType}
      isPerforming={isPerforming}
      id={id}
      requiredInputs={requiredInputs}
      handles={() => (
        <AgentNodeHandles
          isRunningPipeline={isRunningPipeline}
          disabled={disabled}
          isSourceConnectable={isSourceConnectable}
          isPerforming={isPerforming}
        />
      )}
    >
      <NodeConfigSection section="basic">
        {isOrphan ? <AgentOrphanWarning /> : null}
        <ProjectAgentPicker
          label={t('pipelines.flowEditor.agentNode.agentLabel', 'Agent')}
          tooltip={resolveAgentTooltip()}
          value={selectedToolkitValue}
          onSelect={onSelectAgentItem}
          onClear={onClearAgent}
          disabled={isFieldsDisabled}
          attachedOnly
          pickerContext={pickerContext}
          data-testid="pipeline-agent-node-picker"
        />
        <AgentNodeVersionAndBasicMappings
          isOrphan={isOrphan}
          attachedTool={attachedTool}
          projectId={projectId}
          disabled={isFieldsDisabled}
          onVersionChange={onVersionChange}
          basicKeys={basicKeys}
          mappingFieldsProps={mappingFieldsProps}
        />
      </NodeConfigSection>
      <NodeConfigSection section="advanced" defaultExpanded={false}>
        <NodeFieldHeading
          label={t('pipelines.flowEditor.agentNode.inputLabel', 'Input')}
          nodeType={FlowEditorConstants.PipelineNodeTypes.Agent}
          fieldKey={NODE_FIELD_KEYS.input}
        />
        <InputSelect
          id={id}
          label={t('pipelines.flowEditor.agentNode.inputLabel', 'Input')}
          inputFieldName="input"
          disabled={isFieldsDisabled}
        />
        <NodeFieldHeading
          label={t('pipelines.flowEditor.agentNode.outputLabel', 'Output')}
          nodeType={FlowEditorConstants.PipelineNodeTypes.Agent}
          fieldKey={NODE_FIELD_KEYS.output}
        />
        <OutputSelect
          id={id}
          label={t('pipelines.flowEditor.agentNode.outputLabel', 'Output')}
          outputFieldName="output"
          disabled={isFieldsDisabled}
        />
        {!isOrphan && (
          <InputMappingFields keys={advancedKeys} {...mappingFieldsProps} />
        )}
        <CommonInterruptSettings
          id={id}
          type={FlowEditorConstants.PipelineNodeTypes.Agent}
          disabled={isFieldsDisabled}
          showStructuredOutput={false}
        />
        <NodeSettings
          id={id}
          type={FlowEditorConstants.PipelineNodeTypes.Agent}
          disabled={isFieldsDisabled}
        />
      </NodeConfigSection>
    </NodeCard>
  );
});

const agentNodeStyles: { readonly orphanWarning: SxProps<Theme> } = {
  orphanWarning: (theme: Theme) => ({
    display: 'flex',
    alignItems: 'center',
    gap: theme.spacing(0.75),
    padding: theme.spacing(0.75, 1.5),
    borderRadius: theme.vars.shape.radiusMd,
    backgroundColor: theme.vars.palette.status.onModeration,
    color: theme.vars.palette.text.secondary,
    width: '100%',
    boxSizing: 'border-box',
  }),
};
