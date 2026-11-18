/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * nodes/StateModifierNode.jsx` (115 lines) — unit A2f. See `./AgentNode.tsx`'s
 * module doc comment for the shared account of not-yet-landed sibling
 * modules (`NodeCard`, `CustomHandle`, `../select/{InputSelect,
 * OutputSelect}`), `llmSettings`'s "ambient -> explicit prop" redesign, and
 * the `FlowEditorContext`-default-once + dedup pattern used to stay under
 * the §3.5 complexity budget (12).
 * `AIAssistantInput`'s no-direct-typing constraint is the same one
 * `./RouterNode.tsx`'s doc comment discloses in full.
 */
import type { ReactNode } from 'react';
import { memo, useCallback, useContext, useMemo } from 'react';

import { useEdges, type NodeProps } from '@xyflow/react';

import { t } from '@/shared/i18n';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import { updateYamlNode } from '../../lib/flow-editor/helpers/flowEditor.helpers';
import { FlowEditorContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import { useNodeAiAssistantConfig } from '../../lib/flow-editor/hooks/useNodeAiAssistantConfig';
import type { AiAssistantLlmSettings } from '../../api/aiAssistantPredict';
import type { FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import { AIAssistantInput } from '../AIAssistantInput';
import { InputSelect } from '../select/InputSelect';
import { OutputSelect } from '../select/OutputSelect';
import { NodeSettings } from '../settings/NodeSettings';
import { CustomHandle } from './CustomHandle';
import { NodeErrorOutputHandle } from './NodeErrorOutputHandle';
import { NodeCard } from './BaseNode/NodeCard';

/** `FlowEditorContext`, defaulted once — see `./AgentNode.tsx`'s identical constant for the full rationale (§3.5 complexity budget). */
const EMPTY_FLOW_EDITOR_CONTEXT: FlowEditorContextValue = {
  yamlJsonObject: {},
  setFlowNodes: () => {},
  setFlowEdges: () => {},
  setYamlJsonObject: () => {},
};

export interface StateModifierNodeProps extends NodeProps<FlowNode> {
  readonly llmSettings?: AiAssistantLlmSettings | null | undefined;
}

interface StateModifierNodeHandlesProps {
  readonly id: string;
  readonly isRunningPipeline: boolean;
  readonly disabled: boolean;
  readonly isSourceConnectable: boolean;
  readonly isPerforming: boolean;
}

/** Extracted purely to keep `StateModifierNode`'s own cyclomatic complexity under the §3.5 budget (12) — same technique `./AgentNode.tsx`'s `AgentNodeHandles` uses. */
function StateModifierNodeHandles(props: StateModifierNodeHandlesProps): ReactNode {
  const { id, isRunningPipeline, disabled, isSourceConnectable, isPerforming } = props;
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
        id="source"
        isConnectable={isSourceConnectable && !isRunningPipeline && !disabled}
        isRunningPipeline={isRunningPipeline}
        isPerforming={isPerforming}
      />
      <NodeErrorOutputHandle id={id} isRunningPipeline={isRunningPipeline} disabled={disabled} isPerforming={isPerforming} />
    </>
  );
}

export const StateModifierNode = memo(function StateModifierNode(props: StateModifierNodeProps): ReactNode {
  const { id, data, selected, type: nodeType = FlowEditorConstants.PipelineNodeTypes.StateModifier, llmSettings } = props;

  const edges = useEdges();
  const flowEditorContext = useContext(FlowEditorContext) ?? EMPTY_FLOW_EDITOR_CONTEXT;
  const { yamlJsonObject, setYamlJsonObject } = flowEditorContext;
  const isRunningPipeline = Boolean(flowEditorContext.isRunningPipeline);
  const disabled = Boolean(flowEditorContext.disabled);
  const isPerforming = Boolean(data?.isPerforming);
  const isFieldsDisabled = isRunningPipeline || disabled;
  const resolvedLlmSettings = useNodeAiAssistantConfig(llmSettings as Record<string, unknown> | null | undefined) as AiAssistantLlmSettings | null;

  const yamlNode = useMemo(() => yamlJsonObject.nodes?.find(node => node.id === id), [id, yamlJsonObject.nodes]);
  const isSourceConnectable = useMemo(
    () => !edges.find(edge => edge.source === id && edge.target !== FlowEditorConstants.PipelineNodeTypes.End),
    [edges, id],
  );

  const templateValue = yamlNode?.template ?? '';

  const handleTemplateFilling = useCallback(
    (event: { readonly target: { readonly value: string } }) => {
      updateYamlNode(id, 'template', event.target.value, yamlJsonObject, setYamlJsonObject);
    },
    // baseline's own deps array (faithfully ported) -- FlowEditorContext's yamlJsonObject/setYamlJsonObject are not memoized upstream, matching this batch's established `useLLMInputMapping.ts` precedent.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [id, setYamlJsonObject, yamlJsonObject],
  );

  return (
    <NodeCard
      name={id}
      isEntrypoint={yamlJsonObject.entry_point === id}
      selected={selected}
      type={nodeType}
      isPerforming={isPerforming}
      id={id}
      handles={() => (
        <StateModifierNodeHandles
          id={id}
          isRunningPipeline={isRunningPipeline}
          disabled={disabled}
          isSourceConnectable={isSourceConnectable}
          isPerforming={isPerforming}
        />
      )}
    >
      <AIAssistantInput
        value={templateValue}
        disabled={isFieldsDisabled}
        label={t('pipelines.flowEditor.stateModifierNode.jinjaTemplate', 'Jinja Template')}
        fieldName="Template"
        language="jinja"
        modelConfig={resolvedLlmSettings}
        fieldBinding={{ name: 'template', onInput: handleTemplateFilling }}
      />
      <InputSelect
        id={id}
        label={t('pipelines.flowEditor.stateModifierNode.variablesToClean', 'Variables to clean')}
        inputFieldName="variables_to_clean"
        disabled={isFieldsDisabled}
      />
      <InputSelect
        id={id}
        label={t('pipelines.flowEditor.stateModifierNode.input', 'Input')}
        inputFieldName="input"
        disabled={isFieldsDisabled}
      />
      <OutputSelect
        id={id}
        label={t('pipelines.flowEditor.stateModifierNode.output', 'Output')}
        outputFieldName="output"
        disabled={isFieldsDisabled}
      />
      <NodeSettings id={id} type={FlowEditorConstants.PipelineNodeTypes.StateModifier} disabled={isFieldsDisabled} />
    </NodeCard>
  );
});
