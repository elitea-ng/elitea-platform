/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * nodes/RouterNode.jsx` (190 lines) — unit A2f. See `./AgentNode.tsx`'s
 * module doc comment for the shared account of not-yet-landed sibling
 * modules (`NodeCard`, `CustomHandle`, `../select/{RouteSelect,
 * InputSelect}`), `llmSettings`'s "ambient -> explicit prop" redesign, and
 * the `FlowEditorContext`-default-once + dedup pattern used to stay under
 * the §3.5 complexity budget (12).
 *
 * `Chip.HeadingChip`/`Select.SingleSelect` -> the same already-landed
 * `@/shared/ui/HeadingChip`/`@/shared/ui/SingleSelect` substitution
 * `./HITLNode.tsx`'s doc comment covers in full (`onChange` not
 * `onValueChange`, no `labelNode`/`showBorder`/`className` — the baseline's
 * `labelNode={<Chip.HeadingChip label="Default output" />}` becomes a
 * `HeadingChip` rendered as a sibling above the `SingleSelect` instead of
 * inside its label slot).
 *
 * `AIAssistantInput` is this unit's already-landed sibling
 * (`../AIAssistantInput`, unit A2a). It previously forwarded no `onChange`/
 * `onInput` to its own base field at all (only `fieldBinding.{onChange,
 * onInput}`, fired from the AI Assistant modal's own apply/blur handlers),
 * which was reported and CONFIRMED as an adversarial-review blocker: users
 * could no longer type directly into this Condition field (or the
 * equivalent Description/system/task/user_message fields on
 * Legacy/NormalDecisionNode and LLMNode/HITLNode), only edit it through the
 * separate AI Assistant modal. `AIAssistantInput.tsx`'s own module doc
 * comment ("FIX (confirmed adversarial-review finding #1, this file:60)")
 * now records the real fix, made there (that file, unit A2a, is outside
 * this cluster's `ui/nodes/` scope and is not touched here): its
 * `buildInputBaseProps` now forwards a real `onChange` that routes through
 * `fieldBinding.onChange`/`onInput` on every keystroke, not just on modal
 * apply/blur. This call site needed no further change once that landed —
 * `fieldBinding.onInput: handleConditionFilling` below now fires from
 * direct typing too, in addition to the AI Assistant modal's apply/close.
 */
import type { ReactNode } from 'react';
import { memo, useCallback, useContext, useMemo } from 'react';

import { useEdges, type NodeProps } from '@xyflow/react';

import Box from '@mui/material/Box';

import { HeadingChip } from '@/shared/ui/HeadingChip';
import { SingleSelect } from '@/shared/ui/SingleSelect';
import { t } from '@/shared/i18n';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import { updateYamlNode } from '../../lib/flow-editor/helpers/flowEditor.helpers';
import { FlowEditorContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import { useNodeAiAssistantConfig } from '../../lib/flow-editor/hooks/useNodeAiAssistantConfig';
import type { AiAssistantLlmSettings } from '../../api/aiAssistantPredict';
import { useNodeOptions } from '../../lib/flow-editor/hooks/useNodeOptions';
import type { FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import { AIAssistantInput } from '../AIAssistantInput';
import { InputSelect } from '../select/InputSelect';
import { RouteSelect } from '../select/RouteSelect';
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

export interface RouterNodeProps extends NodeProps<FlowNode> {
  readonly llmSettings?: AiAssistantLlmSettings | null | undefined;
}

interface RouterNodeHandlesProps {
  readonly id: string;
  readonly isRunningPipeline: boolean;
  readonly disabled: boolean;
  readonly isTargetConnectable: boolean;
  readonly isDefaultConnectable: boolean;
  readonly isPerforming: boolean;
}

/** Extracted purely to keep `RouterNode`'s own cyclomatic complexity under the §3.5 budget (12) — same technique `./AgentNode.tsx`'s `AgentNodeHandles` uses. */
function RouterNodeHandles(props: RouterNodeHandlesProps): ReactNode {
  const { id, isRunningPipeline, disabled, isTargetConnectable, isDefaultConnectable, isPerforming } = props;
  return (
    <>
      <CustomHandle
        type="target"
        id="routerNode"
        isConnectable={!isRunningPipeline && isTargetConnectable && !disabled}
        isRunningPipeline={isRunningPipeline}
        isPerforming={isPerforming}
      />
      <CustomHandle
        type="source"
        id="routerNode_routes"
        isConnectable={!isRunningPipeline && !disabled}
        isRunningPipeline={isRunningPipeline}
        isPerforming={isPerforming}
        style={{ left: 'calc(50% - 3.125rem)' }}
      />
      <CustomHandle
        type="source"
        id="routerNode_default_output"
        label={t('pipelines.flowEditor.routerNode.defaultOutput', 'Default output')}
        isConnectable={!isRunningPipeline && isDefaultConnectable && !disabled}
        isRunningPipeline={isRunningPipeline}
        isPerforming={isPerforming}
        style={{ left: 'calc(50% + 3.125rem)' }}
      />
      <NodeErrorOutputHandle id={id} isRunningPipeline={isRunningPipeline} disabled={disabled} isPerforming={isPerforming} left="calc(50% + 6.25rem)" />
    </>
  );
}

interface RouterEdgeLike {
  readonly id: string;
  readonly source: string;
  readonly target: string;
  readonly sourceHandle?: string | null | undefined;
  readonly data?: { readonly label?: string } | undefined;
}

/** Pure extraction of `handleDefaultOutput`'s edge-list rebuild — kept out of `RouterNode`'s own body purely to stay under the §3.5 complexity budget (12). */
function buildDefaultOutputEdges<TEdge extends RouterEdgeLike>(
  prevEdges: readonly TEdge[],
  id: string,
  value: string,
  isInterrupt: boolean,
): TEdge[] {
  const filteredEdges = prevEdges.filter(edge => edge.source !== id || edge.sourceHandle !== 'routerNode_default_output');
  if (!value) return filteredEdges;
  const existingEdge = filteredEdges.find(
    edge => edge.target === value && edge.source === id && edge.sourceHandle === 'routerNode_default_output',
  );
  if (existingEdge) return filteredEdges;
  return [
    ...filteredEdges,
    {
      id: `xy-edge__${id}default_output---${value}`,
      source: id,
      sourceHandle: 'routerNode_default_output',
      target: value,
      type: 'custom',
      // `exactOptionalPropertyTypes` forbids `{ label: undefined }` against `FlowEdgeData`'s `label?: string` — the key is omitted entirely instead.
      data: isInterrupt ? { label: 'interrupt' } : {},
    } as unknown as TEdge,
  ];
}

export const RouterNode = memo(function RouterNode(props: RouterNodeProps): ReactNode {
  const { id, data, selected, type: nodeType = FlowEditorConstants.PipelineNodeTypes.Router, llmSettings } = props;

  const edges = useEdges();
  const flowEditorContext = useContext(FlowEditorContext) ?? EMPTY_FLOW_EDITOR_CONTEXT;
  const { yamlJsonObject, setYamlJsonObject, setFlowEdges } = flowEditorContext;
  const isRunningPipeline = Boolean(flowEditorContext.isRunningPipeline);
  const disabled = Boolean(flowEditorContext.disabled);
  const isPerforming = Boolean(data?.isPerforming);
  const isFieldsDisabled = isRunningPipeline || disabled;
  const resolvedLlmSettings = useNodeAiAssistantConfig(llmSettings as Record<string, unknown> | null | undefined) as AiAssistantLlmSettings | null;

  const yamlNode = useMemo(() => yamlJsonObject.nodes?.find(node => node.id === id), [id, yamlJsonObject.nodes]);
  const routes = useNodeOptions(node => node.id !== id, true);
  const isTargetConnectable = useMemo(() => !edges.find(edge => edge.target === id), [edges, id]);
  const isDefaultConnectable = useMemo(
    () => !edges.find(edge => edge.source === id && edge.sourceHandle === 'routerNode_default_output'),
    [edges, id],
  );

  const defaultOutputNode = yamlNode?.default_output ?? 'END';

  const handleDefaultOutput = useCallback(
    (value: string) => {
      updateYamlNode(id, FlowEditorConstants.DEFAULT_OUTPUT, value, yamlJsonObject, setYamlJsonObject);
      const isInterrupt = yamlJsonObject.interrupt_before?.includes(value) ?? false;
      setFlowEdges(prevEdges => buildDefaultOutputEdges(prevEdges, id, value, isInterrupt));
    },
    // baseline's own deps array (faithfully ported) -- FlowEditorContext's yamlJsonObject/setYamlJsonObject are not memoized upstream, matching this batch's established `useLLMInputMapping.ts` precedent.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [id, setFlowEdges, setYamlJsonObject, yamlJsonObject],
  );

  // `yamlNode.condition` is typed as `YamlConditionSpec` (`pipelineFlow.types.ts`,
  // sized for the legacy Condition-node's inline sub-object) — for a Router
  // node the baseline stores a plain string under this SAME key
  // (`RouterNode.jsx:84-93`, `updateYamlNode(id, 'condition', ...)`), so it
  // is read/written here as `unknown` cast to `string`, matching baseline
  // behaviour exactly rather than the (Condition-node-shaped) declared type.
  const conditionValue = (yamlNode?.condition as unknown as string | undefined) ?? '';

  const handleConditionFilling = useCallback(
    (event: { readonly target: { readonly value: string } }) => {
      updateYamlNode(id, 'condition', event.target.value, yamlJsonObject, setYamlJsonObject);
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
        <RouterNodeHandles
          id={id}
          isRunningPipeline={isRunningPipeline}
          disabled={disabled}
          isTargetConnectable={isTargetConnectable}
          isDefaultConnectable={isDefaultConnectable}
          isPerforming={isPerforming}
        />
      )}
    >
      <AIAssistantInput
        value={conditionValue}
        disabled={isFieldsDisabled}
        label={t('pipelines.flowEditor.routerNode.condition', 'Condition')}
        fieldName="Condition"
        language="jinja"
        modelConfig={resolvedLlmSettings}
        fieldBinding={{ name: 'condition', onInput: handleConditionFilling }}
      />
      <RouteSelect
        id={id}
        label={t('pipelines.flowEditor.routerNode.routes', 'Routes')}
        fieldName="routes"
        disabled={isFieldsDisabled}
        nodesFilter={(node: { readonly id: string }) => node.id !== id}
        addEndNode
      />
      <InputSelect
        id={id}
        label={t('pipelines.flowEditor.routerNode.input', 'Input')}
        inputFieldName="input"
        disabled={isFieldsDisabled}
      />
      <HeadingChip label={t('pipelines.flowEditor.routerNode.defaultOutput', 'Default output')} />
      {/*
        BUG FIX (found while writing this file's tests, `RouterNode.test.tsx`):
        every other node-level `SingleSelect`/`RouteSelect`/`InputSelect` call
        site in this slice is wrapped in (or itself applies) a `className=
        "nopan nodrag"` shield -- see `./HITLNode.tsx`'s identical "Edit state
        key" `SingleSelect`, `./HITLNode.parts.tsx`'s `HITLRouteRow`, and
        `../select/{InputSelect,OutputSelect,RouteSelect}.tsx`'s own
        `className` props -- so a mousedown on the control does not bubble to
        React Flow's node wrapper and start a canvas drag. This one call site
        had no such wrapper: reproduced empirically -- opening this exact
        dropdown inside a real `<ReactFlow>` node throws out of `d3-drag`'s
        `mousedowned` (`Cannot read properties of null (reading 'document')`),
        the node-drag-start handler firing on a bubbled mousedown the Select's
        own portal-rendered menu does not expect. Wrapped here to match the
        established pattern, not a stylistic change.
      */}
      <Box className="nopan nodrag">
        <SingleSelect
          sx={{ marginBottom: 0 }}
          value={defaultOutputNode}
          onChange={handleDefaultOutput}
          options={[...routes]}
          disabled={isFieldsDisabled}
        />
      </Box>
      <NodeSettings id={id} type={FlowEditorConstants.PipelineNodeTypes.Router} disabled={isFieldsDisabled} />
    </NodeCard>
  );
});
