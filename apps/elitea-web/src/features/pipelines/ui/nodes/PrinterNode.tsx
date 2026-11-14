/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * nodes/PrinterNode.jsx` (119 lines, unit A2e).
 *
 * DISCLOSED DEVIATIONS, each forced by a real, verified constraint:
 *
 *  - `AIAssistantInput` (baseline: `@/[fsd]/features/pipelines/ai-assistant/
 *    ui`) -> `../../ui/AIAssistantInput` (unit A2a, already landed in this
 *    slice, R-L3 intra-slice import). That component's own real, narrower
 *    prop surface (12-prop budget, its own doc comment "DEVIATION 1")
 *    drops several baseline props with no equivalent: `multiline`/
 *    `fullWidth`/`autoComplete`/`showexpandicon`/`collapseContent`/
 *    `showCopyAction`/`showExpandAction`/`variant`/`placeholder`/
 *    `hasActionsToolBar`/`containerProps` all have no landed counterpart --
 *    dropped here, not invented. `value`/`label`/`fieldName`/`language`/
 *    `modelConfig`/`disabled` are preserved as direct props (all present on
 *    the real component); `AIAssistantInputProps` has no direct `name`/
 *    `onInput` props at all (verified against that file's own interface) --
 *    the "final message" edit handler is wired through
 *    `fieldBinding={{ name: 'final_message', onInput: handleFinalMessageChange }}`
 *    instead, the same real, established mechanism `RouterNode.tsx`'s own
 *    `handleConditionFilling`/`StateModifierNode.tsx`'s own
 *    `handleTemplateFilling` are wired through.
 *  - `useNodeAiAssistantConfig()` (baseline: ambient `useFormikContext()`
 *    read) -> takes an explicit `llmSettings` parameter (that hook's own
 *    header covers the no-Formik rationale); the caller (out of this pure-
 *    node-scaffolding sub-unit's scope) reads `version_details.llm_settings`
 *    off its own react-hook-form instance and passes it in as a new
 *    `llmSettings` prop.
 *  - `onChangeMapping`/`modelConfig` are cast (`as never`) where passed to
 *    the landed `SimpleLLMInputs` (unit A2h) -- same "two independently-
 *    authored TS types don't structurally unify, runtime shapes match"
 *    situation `CodeNode.tsx`'s own header documents in full.
 */
import type { ReactNode } from 'react';
import { memo, useCallback, useContext, useMemo } from 'react';

import { useEdges } from '@xyflow/react';

import { AIAssistantInput } from '../../ui/AIAssistantInput';
import { FlowEditorContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import { PipelineNodeTypes } from '../../lib/flow-editor/constants/flowEditor.constants';
import { updateYamlNode } from '../../lib/flow-editor/helpers/flowEditor.helpers';
import type { YamlPipelineNode } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import { useNodeAiAssistantConfig } from '../../lib/flow-editor/hooks/useNodeAiAssistantConfig';
import { usePrinterInputMapping } from '../../lib/flow-editor/hooks/usePrinterInputMapping';
import { NodeCard } from './BaseNode/NodeCard';
import { CustomHandle } from './CustomHandle';
import { SimpleLLMInputs } from '../settings/SimpleLLMInputs';
import { t } from '@/shared/i18n';

export interface PrinterNodeProps {
  readonly id: string;
  readonly data?: { readonly isPerforming?: boolean };
  readonly selected?: boolean;
  /** Replaces the baseline's ambient `useFormikContext()` llm-settings read -- see module doc comment. */
  readonly llmSettings?: Record<string, unknown> | null;
}

interface PrinterNodeYamlState {
  readonly isRunningPipeline: boolean | undefined;
  readonly disabled: boolean | undefined;
  readonly yamlJsonObject: FlowEditorContextValue['yamlJsonObject'] | undefined;
  readonly setYamlJsonObject: FlowEditorContextValue['setYamlJsonObject'] | undefined;
  readonly yamlNode: YamlPipelineNode | undefined;
}

/** Groups every `FlowEditorContext`-derived read this component needs -- split out purely to keep `PrinterNode` under the §3.5 complexity ceiling (each optional-chain read counts as a branch, matching `BaseToolNode.tsx`'s own `useBaseToolNodeYamlState`). */
function usePrinterNodeYamlState(id: string): PrinterNodeYamlState {
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

interface PrinterNodeHandlesProps {
  readonly isRunningPipeline: boolean | undefined;
  readonly disabled: boolean | undefined;
  readonly isSourceConnectable: boolean;
  readonly isPerforming: boolean | undefined;
}

/** `PrinterNode.jsx:59-78` (the `handles` render-prop body) as its own named component -- see `usePrinterNodeYamlState`'s doc comment for why. */
function PrinterNodeHandles({ isRunningPipeline, disabled, isSourceConnectable, isPerforming }: PrinterNodeHandlesProps): ReactNode {
  return (
    <>
      <CustomHandle
        type="target"
        id="target"
        isConnectable={!isRunningPipeline && !disabled}
        isRunningPipeline={Boolean(isRunningPipeline)}
        isPerforming={Boolean(isPerforming)}
      />
      <CustomHandle
        type="source"
        id="source"
        isConnectable={isSourceConnectable && !isRunningPipeline && !disabled}
        isRunningPipeline={Boolean(isRunningPipeline)}
        isPerforming={Boolean(isPerforming)}
      />
    </>
  );
}

export const PrinterNode = memo(function PrinterNode({ id, data, selected, llmSettings }: PrinterNodeProps): ReactNode {
  const { isRunningPipeline, disabled, yamlJsonObject, setYamlJsonObject, yamlNode } = usePrinterNodeYamlState(id);
  const pipelineLLMConfig = useNodeAiAssistantConfig(llmSettings);

  const flowEdges = useEdges();
  const isSourceConnectable = useMemo(
    () => !flowEdges.find(edge => edge.source === id && edge.target !== PipelineNodeTypes.End),
    [flowEdges, id],
  );

  const { inputMappings, onChangeMapping, defaultValues } = usePrinterInputMapping({ id });

  const finalMessageValue = yamlNode?.['final_message'] as string | undefined;

  const handleFinalMessageChange = useCallback(
    (event: { readonly target: { readonly value: string } }) => {
      if (!setYamlJsonObject || !yamlJsonObject) return;
      updateYamlNode(id, 'final_message', event.target.value, yamlJsonObject, setYamlJsonObject);
    },
    [id, setYamlJsonObject, yamlJsonObject],
  );

  const finalMessageLabel = t('pipelines.flowEditor.printerNode.finalMessageLabel', 'Final Message');

  return (
    <NodeCard
      name={id}
      isEntrypoint={yamlJsonObject?.entry_point === id}
      selected={Boolean(selected)}
      type={PipelineNodeTypes.Printer}
      isPerforming={Boolean(data?.isPerforming)}
      id={id}
      handles={() => (
        <PrinterNodeHandles
          isRunningPipeline={isRunningPipeline}
          disabled={disabled}
          isSourceConnectable={isSourceConnectable}
          isPerforming={data?.isPerforming}
        />
      )}
    >
      <SimpleLLMInputs
        inputMappings={inputMappings}
        values={yamlNode?.input_mapping ?? {}}
        onChangeMapping={onChangeMapping as never}
        defaultValues={defaultValues}
        disabled={isRunningPipeline || disabled}
        enableAIAssistant
        modelConfig={pipelineLLMConfig as never}
      />
      <AIAssistantInput
        disabled={Boolean(isRunningPipeline || disabled)}
        label={finalMessageLabel}
        placeholder={t('pipelines.flowEditor.printerNode.finalMessagePlaceholder', 'The message shown when the flow reaches this node')}
        fieldName={finalMessageLabel}
        value={finalMessageValue ?? ''}
        fieldBinding={{ name: 'final_message', onInput: handleFinalMessageChange }}
        language="text"
        modelConfig={pipelineLLMConfig as never}
      />
    </NodeCard>
  );
});
