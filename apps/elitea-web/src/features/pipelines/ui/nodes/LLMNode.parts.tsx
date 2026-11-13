/**
 * `useLLMNodeModel` + `LLMNodeHandles` for `./LLMNode.tsx`, split out purely
 * to satisfy the §3.5 complexity budget (12) — same technique
 * `./HITLNode.parts.tsx`'s own `useHITLNodeModel` uses; see that hook's own
 * doc comment for the full rationale (moving `useContext`/`useMemo`/
 * `useCallback`-heavy derivation into a dedicated custom hook keeps it out
 * of the calling component's own complexity count).
 */
import type { ReactNode } from 'react';
import { useCallback, useContext, useMemo } from 'react';

import { useEdges } from '@xyflow/react';

import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import { FlowEditorContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import { useGetToolkitNameFromSchema } from '../../lib/flow-editor/hooks/useGetToolkitNameFromSchema';
import { useLLMInputMapping, type LLMInputMapping } from '../../lib/flow-editor/hooks/useLLMInputMapping';
import { useNodeAiAssistantConfig } from '../../lib/flow-editor/hooks/useNodeAiAssistantConfig';
import { useSelectedProjectId } from '../../lib/flow-editor/hooks/useSelectedProjectId';
import { useToolkitTypeSchemas } from '../../lib/flow-editor/hooks/useToolkitTypeSchemas';
import type { AiAssistantLlmSettings } from '../../api/aiAssistantPredict';
import type { VersionTool } from '../../lib/flow-editor/hooks/useFunctionInputMapping';
import type { SimpleLLMInputMappingValue } from '../settings/SimpleLLMInputItem';
import type { SimpleLLMInputMappingSpec } from '../settings/SimpleLLMInputs';
import { CustomHandle } from './CustomHandle';

/** `FlowEditorContext`, defaulted once — see `./AgentNode.tsx`'s identical constant for the full rationale. */
const EMPTY_FLOW_EDITOR_CONTEXT: FlowEditorContextValue = {
  yamlJsonObject: {},
  setFlowNodes: () => {},
  setFlowEdges: () => {},
  setYamlJsonObject: () => {},
};
const EMPTY_INPUT_MAPPING: Readonly<Record<string, SimpleLLMInputMappingSpec | undefined>> = {};

export interface LLMNodeHandlesProps {
  readonly isRunningPipeline: boolean;
  readonly disabled: boolean;
  readonly isSourceConnectable: boolean;
  readonly isPerforming: boolean;
}

/** Extracted purely to keep `LLMNode`'s own cyclomatic complexity under the §3.5 budget (12) — same technique `./AgentNode.tsx`'s `AgentNodeHandles` uses. */
export function LLMNodeHandles(props: LLMNodeHandlesProps): ReactNode {
  const { isRunningPipeline, disabled, isSourceConnectable, isPerforming } = props;
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
    </>
  );
}

interface LLMToolkitToolRow {
  readonly toolkitName: string;
  readonly tools: readonly string[];
}

/** Pure extraction of the baseline's per-toolkit tool-list resolution (`LLMNode.jsx:102-123`) — kept out of `useLLMNodeModel`'s own body purely to stay under the §3.5 complexity budget (12). */
function resolveToolkitToolRows(
  toolNames: readonly string[],
  allToolkits: readonly VersionTool[],
  getToolkitNameFromSchema: (toolkit: VersionTool) => string,
  getSelectedTools: (type: string | undefined) => readonly string[],
): readonly LLMToolkitToolRow[] {
  return toolNames.map(toolkitName => {
    // `tool_names` keys are written by ToolkitsSelect, whose option VALUE is the schema-derived name,
    // so that match wins; an explicit `toolkit_name` key (older YAML) is the fallback (#2003).
    const toolkitObj =
      allToolkits.find(tk => getToolkitNameFromSchema(tk) === toolkitName) ?? allToolkits.find(tk => tk.toolkit_name === toolkitName);
    const settings = toolkitObj?.settings as { readonly selected_tools?: readonly (string | { readonly name?: string })[] } | undefined;
    const rawTools = ((toolkitObj as unknown as { tools?: readonly (string | { name?: string })[] })?.tools ?? settings?.selected_tools ?? []) as readonly (
      | string
      | { readonly name?: string }
    )[];
    const allTools = rawTools.map(tool => (typeof tool === 'string' ? tool : (tool.name ?? '')));
    const availableTools = getSelectedTools(toolkitObj?.type);
    const tools = availableTools.length > 0 ? allTools.filter(tool => availableTools.includes(tool)) : allTools;
    return { toolkitName, tools };
  });
}

export interface UseLLMNodeModelArgs {
  readonly id: string;
  readonly versionTools: readonly VersionTool[] | undefined;
  readonly llmSettings: AiAssistantLlmSettings | null | undefined;
}

export interface LLMNodeModel {
  readonly isRunningPipeline: boolean;
  readonly disabled: boolean;
  readonly isFieldsDisabled: boolean;
  readonly isEntrypoint: boolean;
  readonly isSourceConnectable: boolean;
  readonly resolvedLlmSettings: AiAssistantLlmSettings | null;
  readonly inputMappings: LLMInputMapping;
  readonly inputMappingValues: Readonly<Record<string, SimpleLLMInputMappingSpec | undefined>>;
  readonly defaultValues: Readonly<Record<string, unknown>>;
  readonly handleSimpleLLMChange: (variable: string, value: SimpleLLMInputMappingValue) => void;
  readonly toolkitToolRows: readonly LLMToolkitToolRow[];
}

/**
 * `useLLMInputMapping`'s own `defaultValues` factory
 * (`../../lib/flow-editor/hooks/useLLMInputMapping.ts`'s `getDefaultLLMInputMapping`)
 * returns the full `{type, value}` *entry* pairs used to seed the YAML
 * `input_mapping` itself, NOT the flat `{variable: rawValue}` shape
 * `SimpleLLMInputs`'s own `defaultValues` prop expects (`../settings/
 * SimpleLLMInputs.tsx`'s `defaultValues[key]` reads, used both as the
 * field's fallback `value` and — critically — as `SimpleLLMInputItem.tsx`'s
 * `onChange`'s `defaultValue` written straight into YAML on every Type
 * dropdown change: `onChangeMapping(variable, { type: newValue, value:
 * shouldPreserveValue ? value : defaultValue })`). Calling `defaultValues()`
 * and handing the *entry* object straight through (as this file previously
 * did — `defaultValues: defaultValues()`) fed e.g. `{ type: 'fixed', value:
 * '' }` into that slot, so switching a field's Type away from fstring/fixed
 * wrote the wrapper object itself into `input_mapping[key].value`,
 * corrupting the prompt once re-read as a stringified JSON blob
 * (`SimpleLLMInputItem.tsx`'s `resolvedValue = typeof value !== 'string' ?
 * JSON.stringify(value) : value`). Unwrapped to each entry's own `.value`
 * here, matching the already-landed sibling pattern
 * `lib/flow-editor/hooks/useCodeInputMapping.ts`/`usePrinterInputMapping.ts`
 * both already use ("Return only the value part for defaultValues, not the
 * entire object").
 */
function toFlatDefaultValues(entryDefaults: LLMInputMapping): Readonly<Record<string, unknown>> {
  return Object.fromEntries(Object.entries(entryDefaults).map(([key, entry]) => [key, entry.value]));
}

/**
 * Every piece of `LLMNode`'s own derived state/handlers, gathered behind
 * one custom hook — see `./HITLNode.parts.tsx`'s `useHITLNodeModel` doc
 * comment for the full complexity-budget rationale this mirrors.
 */
export function useLLMNodeModel(args: UseLLMNodeModelArgs): LLMNodeModel {
  const { id, versionTools, llmSettings } = args;

  const flowEditorContext = useContext(FlowEditorContext) ?? EMPTY_FLOW_EDITOR_CONTEXT;
  const { yamlJsonObject } = flowEditorContext;
  const isRunningPipeline = Boolean(flowEditorContext.isRunningPipeline);
  const disabled = Boolean(flowEditorContext.disabled);

  const projectId = useSelectedProjectId();
  const { toolkitTypeSchemas } = useToolkitTypeSchemas(projectId);
  const { getToolkitNameFromSchema, getSelectedTools } = useGetToolkitNameFromSchema(toolkitTypeSchemas);
  const resolvedLlmSettings = useNodeAiAssistantConfig(llmSettings as Record<string, unknown> | null | undefined) as AiAssistantLlmSettings | null;

  const flowEdges = useEdges();
  const yamlNode = useMemo(() => yamlJsonObject.nodes?.find(node => node.id === id), [id, yamlJsonObject.nodes]);
  const isSourceConnectable = useMemo(
    () => !flowEdges.find(edge => edge.source === id && edge.target !== FlowEditorConstants.PipelineNodeTypes.End),
    [flowEdges, id],
  );

  const allToolkits = useMemo(() => versionTools ?? [], [versionTools]);
  const { inputMappings, onChangeMapping, defaultValues } = useLLMInputMapping({ id });
  // `SimpleLLMInputs.tsx`'s (A2h) `SimpleLLMInputMappingValue.type` is optional;
  // `useLLMInputMapping.ts`'s (A2f) own `LLMInputMappingEntry.type` is required
  // (matching `getDefaultLLMInputMapping`'s always-fully-populated entries) --
  // a thin adapter bridges the two contracts rather than loosening either.
  const handleSimpleLLMChange = useCallback(
    (key: string, value: SimpleLLMInputMappingValue) => {
      onChangeMapping(key, { type: value.type ?? 'fixed', value: value.value });
    },
    [onChangeMapping],
  );

  const toolNames = useMemo(() => Object.keys((yamlNode?.tool_names as Record<string, unknown> | undefined) ?? {}), [yamlNode]);
  const toolkitToolRows = useMemo(
    () => resolveToolkitToolRows(toolNames, allToolkits, getToolkitNameFromSchema, getSelectedTools),
    [toolNames, allToolkits, getToolkitNameFromSchema, getSelectedTools],
  );

  return {
    isRunningPipeline,
    disabled,
    isFieldsDisabled: isRunningPipeline || disabled,
    isEntrypoint: yamlJsonObject.entry_point === id,
    isSourceConnectable,
    resolvedLlmSettings,
    inputMappings,
    inputMappingValues: yamlNode?.input_mapping ?? EMPTY_INPUT_MAPPING,
    defaultValues: toFlatDefaultValues(defaultValues()),
    handleSimpleLLMChange,
    toolkitToolRows,
  };
}
