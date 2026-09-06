/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * nodes/BaseToolNode.jsx` (197 lines, unit A2e). Shared body for
 * `McpNode`/`ToolkitNode` (this sub-unit's own siblings, see those files).
 *
 * `../settings/InputMappings/InputMapping` and `../settings/
 * CommonInterruptSettings` (unit A2h/A2i) have since landed and are used
 * directly, imported and rendered below with prop shapes preserved verbatim
 * from the baseline call sites (`BaseToolNode.jsx:172-186`). Likewise,
 * `useFunctionInputMapping`/`useGetToolkitNameFromSchema` (unit A2d/A2g,
 * `lib/flow-editor/hooks/`), `ToolSelect` (unit A2h, `ui/select/`),
 * `SingleSelect` (`shared/ui`), `ToolTypes` (`entities/toolkit`, Wave-2
 * promotion), `NodeCard`/`CustomHandle` (this sub-unit's own siblings) all
 * landed and are used directly -- no compile blockers remain in this file.
 *
 * DISCLOSED REDESIGN: `useFunctionInputMapping` takes an explicit
 * `versionTools`/`yamlJsonObject`/`setYamlJsonObject` argument instead of
 * the baseline's ambient `useContext(FlowEditorContext)` + `useFormikContext()`
 * reads -- that hook's own header covers the no-Formik rationale; this
 * component reads `FlowEditorContext` itself (still needed for
 * `isRunningPipeline`/yaml access) and threads `versionTools` through as a
 * new prop (the baseline read it from `useFormikContext().values.
 * version_details.tools`; the real caller, out of this sub-unit's scope,
 * supplies it via `useWatch('version_details.tools')` on its
 * react-hook-form instance).
 */
import type { ReactNode } from 'react';
import { memo, useCallback, useContext, useMemo } from 'react';

import { SingleSelect } from '@/shared/ui/SingleSelect';
import { ToolListError } from '@/shared/ui/ToolListError';
import { ToolTypes } from '@/entities/toolkit';

import { FlowEditorContext, type FlowEditorContextValue } from '../../lib/flow-editor/flowEditorContext';
import { batchUpdateYamlNode, getDefaultInputMappingOfTool, getToolName } from '../../lib/flow-editor/helpers/flowEditor.helpers';
import { useFunctionInputMapping } from '../../lib/flow-editor/hooks/useFunctionInputMapping';
import { useGetToolkitNameFromSchema } from '../../lib/flow-editor/hooks/useGetToolkitNameFromSchema';
import { useToolkitTypeSchemas } from '../../lib/flow-editor/hooks/useToolkitTypeSchemas';
import { useSelectedProjectId } from '../../lib/flow-editor/hooks/useSelectedProjectId';
import { CommonInterruptSettings } from '../settings/CommonInterruptSettings';
import { InputMapping } from '../settings/InputMappings/InputMapping';
import { InputSelect } from '../select/InputSelect';
import { OutputSelect } from '../select/OutputSelect';
import { ToolSelect } from '../select/ToolSelect';
import type { PipelineToolEntry } from '../select/pipelineToolEntry.types';
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
 * `BaseToolNode.jsx:52-75` -- the selected toolkit's enabled tool names,
 * sorted for the "Tool" select. Split out purely to keep the component
 * under the §3.5 complexity ceiling.
 *
 * FIX (confirmed adversarial-review finding #3, this file:61): entries were
 * previously treated as plain strings with no `getToolName` normalization
 * (baseline: `BaseToolNode.jsx:57,63,68-69` calls `FlowEditorHelpers.
 * getToolName(tool)` in both the `.filter(...).includes(...)` intersection
 * check AND the final `.map(...)` label/value -- this port had dropped
 * both calls). Object-shaped entries would either never survive the
 * intersection (`{...} !== 'the_same_string'` in `availableTools.
 * includes(...)`) or render literal `"[object Object]"` as both the
 * option's label and value once coerced to a string by JSX/string
 * concatenation. `getToolName` is called at both sites now, matching every
 * sibling caller.
 *
 * ROUTING NOTE (not fixed here, out of this file's scope): `../select/
 * pipelineToolEntry.types.ts`'s `PipelineToolEntry.settings.selected_tools`
 * is typed `readonly string[]` -- narrower than the real runtime shape this
 * fix (and its sibling callers above) defend against. That file belongs to
 * a different sub-unit (A2h, per its own header) and is outside this
 * cluster's scope; it should be widened to `readonly SelectedToolEntry[]`
 * (or equivalent) so a real component-level regression test can construct
 * an object-shaped `versionTools` fixture without a cast -- today only a
 * direct unit test of `computeFunctionOptions` (below, exported for that
 * reason) can exercise the object-shaped path, because this function's own
 * local parameter type is intentionally loose (`Record<string, unknown>`)
 * while the public `BaseToolNodeProps.versionTools: readonly
 * PipelineToolEntry[]` is not.
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

/**
 * `BaseToolNode.jsx:77-107` (`onSelectToolkit`) -- split out purely to keep
 * the component under the §3.5 complexity ceiling. The `yamlJsonObject`/
 * `setYamlJsonObject` presence guard (baseline has none -- `FlowEditorContext`
 * is never optional there) lives here too, for the same reason (matching
 * `DefaultNode.tsx`'s own `applyDefaultNodeToolkitSelection`).
 */
function applyToolkitSelection({
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
function ToolFunctionSelect({ functionOptions, selectedTool, onChangeTool, onClearTool, disabled, catalogueReadFailed, typeSchemasReadFailed, onRetry }: ToolFunctionSelectProps): ReactNode {
  if (functionOptions.length > 0) {
    return (
      <SingleSelect
        label={t('pipelines.flowEditor.baseToolNode.toolLabel', 'Tool')}
        value={selectedTool}
        onChange={onChangeTool}
        options={[...functionOptions]}
        disabled={disabled}
        onClear={onClearTool}
      />
    );
  }
  if (catalogueReadFailed || typeSchemasReadFailed) return <ToolListError onRetry={onRetry} />;
  return null;
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
    retryDynamicToolsRead,
    inputMappings,
    defaultValues,
  } = useFunctionInputMapping({
    id,
    yamlJsonObject: yamlJsonObject ?? {},
    setYamlJsonObject: setYamlJsonObject ?? (() => {}),
    // `PipelineToolEntry` is a locally-scoped SUPERSET of `VersionTool`
    // (that type's own doc comment) -- structurally assignable as-is, no
    // cast needed.
    versionTools,
  });

  const projectId = useSelectedProjectId();
  // `isError` has a reader now (#440). The type schemas supply the tool list
  // of a statically-declared toolkit type, so a lost read can empty this
  // node's picker just as a lost catalogue read can.
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

  return (
    <NodeCard
      name={id}
      isEntrypoint={yamlJsonObject?.entry_point === id}
      selected={Boolean(selected)}
      type={nodeType}
      isPerforming={Boolean(data?.isPerforming)}
      id={id}
      handles={() => (
        <BaseToolNodeHandles
          isRunningPipeline={isRunningPipeline}
          isPerforming={data?.isPerforming}
        />
      )}
    >
      <ToolSelect
        onSelectTool={onSelectToolkit}
        selectedToolkit={toolkit ?? ''}
        disabled={Boolean(isRunningPipeline)}
        filterTypes={customFilterTypes}
        versionTools={versionTools ?? []}
      />
      <ToolFunctionSelect
        functionOptions={functionOptions}
        selectedTool={selectedTool}
        onChangeTool={onChangeTool}
        onClearTool={onClearTool}
        disabled={Boolean(isRunningPipeline)}
        catalogueReadFailed={dynamicToolsReadFailed}
        typeSchemasReadFailed={typeSchemasReadFailed}
        onRetry={onRetryToolList}
      />
      <InputSelect
        id={id}
        inputFieldName="input"
        disabled={Boolean(isRunningPipeline)}
      />
      <OutputSelect
        id={id}
        label={t('pipelines.flowEditor.baseToolNode.outputLabel', 'Output')}
        outputFieldName="output"
      />
      <InputMapping
        requiredInputs={requiredInputs}
        mappingInfo={mappingInfo as never}
        input_mapping={inputMappings as never}
        defaultValues={defaultValues}
        values={yamlNode?.input_mapping ?? {}}
        onChangeMapping={onChangeMapping}
        disabled={Boolean(isRunningPipeline)}
      />
      <CommonInterruptSettings
        id={id}
        showStructuredOutput={showStructuredOutput}
        type={nodeType}
        disabled={Boolean(isRunningPipeline)}
      />
    </NodeCard>
  );
});
