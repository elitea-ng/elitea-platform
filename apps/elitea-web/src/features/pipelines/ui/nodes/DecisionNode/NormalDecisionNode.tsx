/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * nodes/DecisionNode/NormalDecisionNode.jsx` (160 lines) — unit A2f. Live
 * code (not deprecated-vs-current): rendered whenever a node id does NOT
 * end with `FlowEditorConstants.DECISION_NODE_ID_SUFFIX` — see `./index.tsx`.
 *
 * See `../AgentNode.tsx`'s module doc comment for the shared account of
 * not-yet-landed sibling modules (`NodeCard`, `CustomHandle`,
 * `../../select/InputSelect`, `../../settings/CommonInterruptSettings`),
 * `llmSettings`'s "ambient -> explicit prop" redesign, and the
 * `FlowEditorContext`-default-once + dedup pattern used to stay under the
 * §3.5 complexity budget (12).
 */
import type { ReactNode } from 'react';
import { memo, useCallback, useContext, useMemo } from 'react';

import Box from '@mui/material/Box';
import { useEdges, type NodeProps } from '@xyflow/react';

import { t } from '@/shared/i18n';
import { InfoTooltip } from '@/shared/ui/InfoTooltip';

import { FlowEditorConstants } from '../../../lib/flow-editor/constants';
import { getNodeFieldInfo, NODE_FIELD_KEYS } from '../../../lib/flow-editor/constants/nodeFieldInfo.constants';
import { updateYamlNode } from '../../../lib/flow-editor/helpers/flowEditor.helpers';
import { FlowEditorContext, type FlowEditorContextValue } from '../../../lib/flow-editor/flowEditorContext';
import { useNodeAiAssistantConfig } from '../../../lib/flow-editor/hooks/useNodeAiAssistantConfig';
import type { AiAssistantLlmSettings } from '../../../api/aiAssistantPredict';
import type { FlowNode } from '../../../lib/flow-editor/reactFlowTypes';
import { NodeConfigSection, NodeFieldHeading } from '../../node-config-drawer/NodeConfigSection';
import { AIAssistantInput } from '../../AIAssistantInput';
import { CommonInterruptSettings } from '../../settings/CommonInterruptSettings';
import { NodeSettings } from '../../settings/NodeSettings';
import { InputSelect } from '../../select/InputSelect';
import { CustomHandle } from '../CustomHandle';
import { NodeCard } from '../BaseNode/NodeCard';
import { DecisionOutputs, commonComponentStyles } from './DecisionNodeShared';

/** `FlowEditorContext`, defaulted once — see `../AgentNode.tsx`'s identical constant for the full rationale (§3.5 complexity budget). */
const EMPTY_FLOW_EDITOR_CONTEXT: FlowEditorContextValue = {
  yamlJsonObject: {},
  setFlowNodes: () => {},
  setFlowEdges: () => {},
  setYamlJsonObject: () => {},
};

export interface NormalDecisionNodeProps extends NodeProps<FlowNode> {
  readonly llmSettings?: AiAssistantLlmSettings | null | undefined;
}

interface NormalDecisionNodeHandlesProps {
  readonly isRunningPipeline: boolean;
  readonly disabled: boolean;
  readonly isElseConnectable: boolean;
  readonly isPerforming: boolean;
  readonly nodesHandleStyle: { readonly left: string };
  readonly defaultOutputHandleStyle: { readonly left: string };
}

/** Extracted purely to keep `NormalDecisionNode`'s own cyclomatic complexity under the §3.5 budget (12) — same technique `../AgentNode.tsx`'s `AgentNodeHandles` uses. */
function NormalDecisionNodeHandles(props: NormalDecisionNodeHandlesProps): ReactNode {
  const { isRunningPipeline, disabled, isElseConnectable, isPerforming, nodesHandleStyle, defaultOutputHandleStyle } = props;
  return (
    <>
      <CustomHandle
        type="target"
        id="target"
        isConnectable={!isRunningPipeline && !disabled}
        isRunningPipeline={isRunningPipeline}
        isPerforming={isPerforming}
      />
      <CustomHandle
        type="source"
        id="nodes"
        isConnectable={!isRunningPipeline && !disabled}
        isRunningPipeline={isRunningPipeline}
        isPerforming={isPerforming}
        style={nodesHandleStyle}
      />
      <CustomHandle
        type="source"
        id="default_output"
        label={t('pipelines.flowEditor.decisionNode.defaultOutput', 'Default output')}
        isConnectable={isElseConnectable && !isRunningPipeline && !disabled}
        isRunningPipeline={isRunningPipeline}
        isPerforming={isPerforming}
        style={defaultOutputHandleStyle}
      />
    </>
  );
}

export const NormalDecisionNode = memo(function NormalDecisionNode(props: NormalDecisionNodeProps): ReactNode {
  const { id, data, selected, llmSettings } = props;
  const styles = componentStyles();
  const edges = useEdges();
  const flowEditorContext = useContext(FlowEditorContext) ?? EMPTY_FLOW_EDITOR_CONTEXT;
  const { yamlJsonObject, setYamlJsonObject, setFlowEdges } = flowEditorContext;
  const isRunningPipeline = Boolean(flowEditorContext.isRunningPipeline);
  const disabled = Boolean(flowEditorContext.disabled);
  const isPerforming = Boolean(data?.isPerforming);
  const isFieldsDisabled = isRunningPipeline || disabled;
  const resolvedLlmSettings = useNodeAiAssistantConfig(llmSettings as Record<string, unknown> | null | undefined) as AiAssistantLlmSettings | null;

  const yamlNode = useMemo(() => yamlJsonObject.nodes?.find(node => node.id === id), [yamlJsonObject.nodes, id]);
  const description = yamlNode?.description ?? '';
  const decisionOutput = useMemo(() => yamlNode?.nodes ?? [], [yamlNode?.nodes]);
  const isElseConnectable = !edges.find(edge => edge.source === id && edge.sourceHandle === FlowEditorConstants.DEFAULT_OUTPUT);

  const onChangeDecisionDescription = useCallback(
    (event: { readonly preventDefault: () => void; readonly target: { readonly value: string } }) => {
      event.preventDefault();
      if (!yamlNode) return;
      updateYamlNode(yamlNode.id, 'description', event.target.value, yamlJsonObject, setYamlJsonObject);
    },
    // baseline's own deps array (faithfully ported) -- FlowEditorContext's yamlJsonObject/setYamlJsonObject are not memoized upstream, matching this batch's established `useLLMInputMapping.ts` precedent.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [setYamlJsonObject, yamlJsonObject, yamlNode],
  );

  const onRemoveOutput = useCallback(
    (output: string) => () => {
      if (!yamlNode) return;
      updateYamlNode(
        yamlNode.id,
        'nodes',
        decisionOutput.filter(item => item !== output),
        yamlJsonObject,
        setYamlJsonObject,
      );
      setFlowEdges(prevEdges => prevEdges.filter(edge => edge.source !== id || edge.target !== output));
    },
    // baseline's own deps array (faithfully ported) -- FlowEditorContext's yamlJsonObject/setYamlJsonObject are not memoized upstream, matching this batch's established `useLLMInputMapping.ts` precedent.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [yamlNode, decisionOutput, yamlJsonObject, setYamlJsonObject, setFlowEdges, id],
  );

  return (
    <NodeCard
      name={data?.label ?? id}
      isEntrypoint={false}
      selected={selected}
      type={FlowEditorConstants.PipelineNodeTypes.Decision}
      isPerforming={isPerforming}
      id={id}
      handles={() => (
        <NormalDecisionNodeHandles
          isRunningPipeline={isRunningPipeline}
          disabled={disabled}
          isElseConnectable={isElseConnectable}
          isPerforming={isPerforming}
          nodesHandleStyle={styles.nodesHandle}
          defaultOutputHandleStyle={styles.defaultOutputHandle}
        />
      )}
    >
      <NodeConfigSection section="basic">
        <Box sx={{ display: 'flex', alignItems: 'flex-start', gap: '0.25rem', width: '100%' }}>
          <Box sx={{ flex: 1, minWidth: 0 }}>
            <AIAssistantInput
              value={description}
              disabled={isFieldsDisabled}
              label={t('pipelines.flowEditor.decisionNode.decisionCriteria', 'Decision criteria')}
              fieldName="description"
              modelConfig={resolvedLlmSettings}
              fieldBinding={{ name: 'description', id: 'description', onInput: onChangeDecisionDescription }}
            />
          </Box>
          <InfoTooltip
            title={getNodeFieldInfo(FlowEditorConstants.PipelineNodeTypes.Decision, NODE_FIELD_KEYS.decisionCriteria)}
            placement="top"
            data-testid="pipeline-node-field-info"
          />
        </Box>
        <DecisionOutputs
          id={id}
          decisionOutput={decisionOutput}
          onRemoveOutput={onRemoveOutput}
          isRunningPipeline={isRunningPipeline}
          disabled={disabled}
        />
      </NodeConfigSection>
      <NodeConfigSection section="advanced" defaultExpanded={false}>
        <NodeFieldHeading
          label={t('pipelines.flowEditor.decisionNode.inputLabel', 'Input')}
          nodeType={FlowEditorConstants.PipelineNodeTypes.Decision}
          fieldKey={NODE_FIELD_KEYS.input}
        />
        <InputSelect
          id={id}
          inputFieldName="input"
          disabled={isFieldsDisabled}
        />
        <CommonInterruptSettings
          id={id}
          showStructuredOutput={false}
          type={FlowEditorConstants.PipelineNodeTypes.Decision}
          disabled={isFieldsDisabled}
        />
        <NodeSettings
          id={id}
          type={FlowEditorConstants.PipelineNodeTypes.Decision}
          disabled={isFieldsDisabled}
        />
      </NodeConfigSection>
    </NodeCard>
  );
});

function componentStyles(): ReturnType<typeof commonComponentStyles> & {
  readonly nodesHandle: { readonly left: string };
  readonly defaultOutputHandle: { readonly left: string };
} {
  return {
    ...commonComponentStyles(),
    nodesHandle: { left: 'calc(50% - 3.125rem)' },
    defaultOutputHandle: { left: 'calc(50% + 3.125rem)' },
  };
}
