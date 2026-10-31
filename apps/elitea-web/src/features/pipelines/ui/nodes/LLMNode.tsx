/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * nodes/LLMNode.jsx` (135 lines) — unit A2f.
 *
 * See `./AgentNode.tsx`'s module doc comment for the full, shared account of
 * (a) which sibling modules this file imports that do not yet exist
 * anywhere in this worktree (`NodeCard`, `CustomHandle`,
 * `../select/{InputSelect,OutputSelect,ToolkitsSelect,LLMToolsSelect}`,
 * `../settings/{SimpleLLMInputs,CommonInterruptSettings}`) and (b) the
 * "ambient context -> explicit prop" redesign convention this batch
 * established for the baseline's `useFormikContext()`/`useNodeAiAssistantConfig()`
 * reads. Applied here as two props: `versionTools` (baseline:
 * `values?.version_details?.tools`, `LLMNode.jsx:41`) and `llmSettings`
 * (baseline: ambient inside `useNodeAiAssistantConfig()` itself, now that
 * hook's own required parameter, `lib/flow-editor/hooks/
 * useNodeAiAssistantConfig.ts`). Also passed to `ToolkitsSelect`'s own
 * `versionTools` prop (`ToolkitsSelect.tsx`, A2h) — cast once, same
 * `VersionTool`(A2d)/`PipelineToolEntry`(A2h) bridge `./AgentNode.tsx`'s own
 * doc comment documents in full.
 *
 * `useLLMInputMapping` -> this unit's own `../../lib/flow-editor/hooks/
 * useLLMInputMapping.ts` (the gap-fill hook, see that file's header). All
 * derived state/handlers live in `useLLMNodeModel` (`./LLMNode.parts.tsx`)
 * — this component itself is pure hook-call-and-render, purely to stay
 * under the §3.5 complexity budget (12); see that hook's own doc comment.
 */
import type { ReactNode } from 'react';
import { memo } from 'react';

import type { NodeProps } from '@xyflow/react';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import type { AiAssistantLlmSettings } from '../../api/aiAssistantPredict';
import type { FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import type { VersionTool } from '../../lib/flow-editor/hooks/useFunctionInputMapping';
import { CommonInterruptSettings } from '../settings/CommonInterruptSettings';
import { LlmOutputContractSettings } from '../settings/LlmOutputContractSettings';
import { SimpleLLMInputs } from '../settings/SimpleLLMInputs';
import { InputSelect } from '../select/InputSelect';
import { LLMToolsSelect } from '../select/LLMToolsSelect';
import { OutputSelect } from '../select/OutputSelect';
import { ToolkitsSelect } from '../select/ToolkitsSelect';
import type { PipelineToolEntry } from '../select/pipelineToolEntry.types';
import { NodeCard } from './BaseNode/NodeCard';
import { LLMNodeHandles, useLLMNodeModel } from './LLMNode.parts';
import { t } from '@/shared/i18n';

export interface LLMNodeProps extends NodeProps<FlowNode> {
  readonly versionTools?: readonly VersionTool[] | undefined;
  readonly llmSettings?: AiAssistantLlmSettings | null | undefined;
}

export const LLMNode = memo(function LLMNode(props: LLMNodeProps): ReactNode {
  const { id, data, selected, versionTools, llmSettings } = props;
  const isPerforming = Boolean(data?.isPerforming);
  const model = useLLMNodeModel({ id, versionTools, llmSettings });
  // `VersionTool` (A2d) and `PipelineToolEntry` (A2h) describe the same real
  // `version_details.tools[]` entry from two sub-units' narrower read
  // surfaces — see module doc comment. Structurally compatible directly (no
  // cast needed), same as `./AgentNode.tsx`'s identical bridge.
  const versionToolsForSelect: readonly PipelineToolEntry[] = versionTools ?? [];

  return (
    <NodeCard
      name={id}
      isEntrypoint={model.isEntrypoint}
      selected={selected}
      type={FlowEditorConstants.PipelineNodeTypes.LLM}
      isPerforming={isPerforming}
      id={id}
      handles={() => (
        <LLMNodeHandles
          isRunningPipeline={model.isRunningPipeline}
          disabled={model.disabled}
          isSourceConnectable={model.isSourceConnectable}
          isPerforming={isPerforming}
        />
      )}
    >
      <SimpleLLMInputs
        inputMappings={model.inputMappings}
        values={model.inputMappingValues}
        onChangeMapping={model.handleSimpleLLMChange}
        defaultValues={model.defaultValues}
        disabled={model.isFieldsDisabled}
        enableAIAssistant
        modelConfig={model.resolvedLlmSettings}
        gap="1rem"
      />

      <InputSelect
        id={id}
        label={t('pipelines.flowEditor.llmNode.inputLabel', 'Input')}
        disabled={model.isFieldsDisabled}
        inputFieldName="input"
      />
      <OutputSelect
        id={id}
        label={t('pipelines.flowEditor.llmNode.outputLabel', 'Output')}
        outputFieldName="output"
        disabled={model.isFieldsDisabled}
      />
      <ToolkitsSelect
        id={id}
        disabled={model.isFieldsDisabled}
        versionTools={versionToolsForSelect}
      />
      {model.toolkitToolRows.map(row => (
        <LLMToolsSelect
          key={row.toolkitName}
          toolkitName={row.toolkitName}
          id={id}
          tools={row.tools}
          disabled={model.isFieldsDisabled}
        />
      ))}
      <LlmOutputContractSettings
        id={id}
        disabled={model.isFieldsDisabled}
      />
      <CommonInterruptSettings
        id={id}
        type={FlowEditorConstants.PipelineNodeTypes.LLM}
        disabled={model.isFieldsDisabled}
      />
    </NodeCard>
  );
});
