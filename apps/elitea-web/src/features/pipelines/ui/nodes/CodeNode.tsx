/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * nodes/CodeNode.jsx` (104 lines, unit A2e).
 *
 * DISCLOSED DEVIATIONS, each forced by a real, verified constraint:
 *
 *  - `useNodeAiAssistantConfig()` (baseline: ambient `useFormikContext()`
 *    read) -> explicit `llmSettings` prop -- same rationale as
 *    `PrinterNode.tsx`'s own header (that hook's own header covers the
 *    no-Formik rationale in full).
 *  - `inputMappings`/`onChangeMapping`/`modelConfig` are cast (`as never`)
 *    where passed to the landed `SimpleLLMInputs` (unit A2h): this file's
 *    own `useCodeInputMapping` (unit A2e) and `useNodeAiAssistantConfig`
 *    (unit A2d) both predate A2h and were typed independently against the
 *    baseline's own field shapes, not against A2h's later, more specific
 *    `SimpleLLMInputMappingValue`/`AiAssistantLlmSettings` types -- the
 *    runtime shapes match (both are `{ type, value }` pairs / a plain
 *    settings record), only the two sub-units' independently-authored TS
 *    types don't structurally unify.
 */
import type { ReactNode } from 'react';
import { memo, useContext, useMemo } from 'react';

import { useEdges } from '@xyflow/react';

import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import { PipelineNodeTypes } from '../../lib/flow-editor/constants/flowEditor.constants';
import type { YamlPipelineNode } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import { useCodeInputMapping } from '../../lib/flow-editor/hooks/useCodeInputMapping';
import { useNodeAiAssistantConfig } from '../../lib/flow-editor/hooks/useNodeAiAssistantConfig';
import { NodeCard } from './BaseNode/NodeCard';
import { CustomHandle } from './CustomHandle';
import { CodeLanguageSelect } from './CodeLanguageSelect';
import { InputSelect } from '../select/InputSelect';
import { OutputSelect } from '../select/OutputSelect';
import { CommonInterruptSettings } from '../settings/CommonInterruptSettings';
import { SimpleLLMInputs } from '../settings/SimpleLLMInputs';
import { t } from '@/shared/i18n';

export interface CodeNodeProps {
  readonly id: string;
  readonly data?: { readonly isPerforming?: boolean };
  readonly selected?: boolean;
  /** Replaces the baseline's ambient `useFormikContext()` llm-settings read -- see module doc comment. */
  readonly llmSettings?: Record<string, unknown> | null;
}

interface CodeNodeYamlState {
  readonly isRunningPipeline: boolean | undefined;
  readonly disabled: boolean | undefined;
  readonly entryPoint: string | undefined;
  readonly yamlNode: YamlPipelineNode | undefined;
}

/** Groups every `FlowEditorContext`-derived read this component needs -- split out purely to keep `CodeNode` under the §3.5 complexity ceiling (matching `BaseToolNode.tsx`'s own `useBaseToolNodeYamlState`). */
function useCodeNodeYamlState(id: string): CodeNodeYamlState {
  const flowEditorContext = useContext(FlowEditorContext);
  const yamlJsonObject = flowEditorContext?.yamlJsonObject;
  const yamlNode = useMemo(() => yamlJsonObject?.nodes?.find(node => node.id === id), [id, yamlJsonObject?.nodes]);

  return {
    isRunningPipeline: flowEditorContext?.isRunningPipeline,
    disabled: flowEditorContext?.disabled,
    entryPoint: yamlJsonObject?.entry_point,
    yamlNode,
  };
}

interface CodeNodeHandlesProps {
  readonly isRunningPipeline: boolean | undefined;
  readonly disabled: boolean | undefined;
  readonly isSourceConnectable: boolean;
  readonly isPerforming: boolean | undefined;
}

/** `CodeNode.jsx:42-69` (the `handles` render-prop body) as its own named component. */
function CodeNodeHandles({ isRunningPipeline, disabled, isSourceConnectable, isPerforming }: CodeNodeHandlesProps): ReactNode {
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

export const CodeNode = memo(function CodeNode({ id, data, selected, llmSettings }: CodeNodeProps): ReactNode {
  const { isRunningPipeline, disabled, entryPoint, yamlNode } = useCodeNodeYamlState(id);
  const pipelineLLMConfig = useNodeAiAssistantConfig(llmSettings);

  const flowEdges = useEdges();
  const isSourceConnectable = useMemo(
    () => !flowEdges.find(edge => edge.source === id && edge.target !== PipelineNodeTypes.End),
    [flowEdges, id],
  );

  const { inputMappings, onChangeMapping, defaultValues } = useCodeInputMapping({ id });

  // `SimpleLLMInputs` expects `{ code: { type, value } }` (baseline: `CodeNode.jsx:34-40`).
  const codeValues = useMemo(
    () => ({ code: yamlNode?.code ?? { type: 'fixed', value: '' } }),
    [yamlNode?.code],
  );

  return (
    <NodeCard
      name={id}
      isEntrypoint={entryPoint === id}
      selected={Boolean(selected)}
      type={PipelineNodeTypes.Code}
      isPerforming={Boolean(data?.isPerforming)}
      id={id}
      handles={() => (
        <CodeNodeHandles
          isRunningPipeline={isRunningPipeline}
          disabled={disabled}
          isSourceConnectable={isSourceConnectable}
          isPerforming={data?.isPerforming}
        />
      )}
    >
      <SimpleLLMInputs
        enableAIAssistant
        inputMappings={inputMappings}
        values={codeValues}
        onChangeMapping={onChangeMapping as never}
        defaultValues={defaultValues}
        disabled={isRunningPipeline || disabled}
        modelConfig={pipelineLLMConfig as never}
      />
      <CodeLanguageSelect id={id} disabled={Boolean(isRunningPipeline || disabled)} />
      <InputSelect
        id={id}
        label={t('pipelines.flowEditor.codeNode.inputLabel', 'Input')}
        disabled={Boolean(isRunningPipeline || disabled)}
        inputFieldName="input"
      />
      <OutputSelect
        id={id}
        label={t('pipelines.flowEditor.codeNode.outputLabel', 'Output')}
        outputFieldName="output"
        disabled={Boolean(isRunningPipeline || disabled)}
      />
      <CommonInterruptSettings
        id={id}
        type={PipelineNodeTypes.Code}
        disabled={Boolean(isRunningPipeline || disabled)}
      />
    </NodeCard>
  );
});
