/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * nodes/deprecated/ToolNode.jsx` (~250 lines) — unit A2g. NOT dead code:
 * still actively registered by the not-yet-built `FlowEditor.jsx` canvas
 * sub-unit (A2k) for pipelines whose stored YAML still uses the legacy
 * `tool` node type — see this unit's mission NOTES.
 *
 * DISCLOSED REDESIGNS, each forced by a real, verified constraint:
 *
 *  1. `useFormikContext()` (baseline: `values.version_details.tools`) ->
 *     an explicit `versionTools` prop — this app has no Formik
 *     (react-hook-form + zod). Matches the already-landed sibling
 *     `ui/select/ToolSelect.tsx`'s (unit A2h) own identical deviation #1,
 *     and `ui/nodes/BaseNode/NodeCardHeader.tsx`'s `toolNames` prop —
 *     the same "ambient context -> parameter" convention this whole batch
 *     established.
 *  2. `useGetToolkitNameFromSchema()` (baseline: ambient Redux-cached
 *     schema read) -> `toolkitTypeSchemas` resolved locally via the
 *     already-landed `useToolkitTypeSchemas`/`useSelectedProjectId` and
 *     passed into `useGetToolkitNameFromSchema(toolkitTypeSchemas)` —
 *     matching that hook's own current, already-landed signature (unit
 *     A2d), and `ui/select/ToolSelect.tsx`'s identical wiring.
 *  3. `FlowEditorSelect.ToolSelect`/`InputSelect`/`OutputSelect` ->
 *     `ui/select/ToolSelect.tsx`/`InputSelect.tsx`/`OutputSelect.tsx`
 *     (unit A2h, all landed) — used with the SAME prop names the baseline
 *     passed (`onSelectTool`/`selectedToolkit`/`filterTypes`,
 *     `id`/`inputFieldName`, `id`/`label`/`outputFieldName`), now plus the
 *     `versionTools` prop those components' own deviation #1 requires.
 *  4. `FlowEditorSettings.CommonInterruptSettings` ->
 *     `ui/settings/CommonInterruptSettings.tsx` (unit A2h, landed) — same
 *     prop names (`id`/`type`/`disabled`).
 *  5. `Input.StyledInputEnhancer` -> `shared/ui/StyledInputEnhancer` (unit
 *     S1-F). That component's props are grouped (`actions`/`expand`
 *     option objects, no bare `showexpandicon`/`maxRows`/`hasActionsToolBar`
 *     booleans, no `onInput` — plain `onChange` instead, forwarded straight
 *     to the underlying `TextField`) — see that component's own doc
 *     comment. `containerProps`/`fieldName`/`hasActionsToolBar` (baseline)
 *     have no equivalent and are dropped; `expand={{minRows:1,maxRows:3}}`
 *     + `actions={{showCopy:false,showExpand:false}}` reproduce the
 *     baseline's rendered behaviour (a 1-3 row growable field, copy/expand
 *     toolbar actions hidden, full-screen edit still available — that part
 *     cannot be turned off, `StyledInputEnhancer.tsx`'s own doc comment:
 *     "the toolbar's entire purpose is the full-screen escape hatch").
 *  6. The dynamic tool-name discovery is wired (#440). This node reads the
 *     catalogue with `entities/toolkit`'s `toolkitTools.useToolkitTools`,
 *     hands the names to `./useToolNodeEditing.ts`, and shows a failed read
 *     as its own state with a retry instead of an empty picker.
 *
 * The editing callbacks/`functionOptions` derivation live in
 * `./useToolNodeEditing.ts`, and the target/source handle pair in the
 * shared `./SimpleNodeHandles.tsx` — both split out purely to keep this
 * file under the §3.5 `complexity` budget (12).
 */
import type { ReactNode } from 'react';
import { memo, useCallback } from 'react';

import { SingleSelect } from '@/shared/ui/SingleSelect';
import { StyledInputEnhancer } from '@/shared/ui/StyledInputEnhancer';
import { ToolListError } from '@/shared/ui/ToolListError';
import { t } from '@/shared/i18n';

import { toolkitTools } from '@/entities/toolkit';

import { PipelineNodeTypes } from '../../../lib/flow-editor/constants/flowEditor.constants';
import { useGetToolkitNameFromSchema } from '../../../lib/flow-editor/hooks/useGetToolkitNameFromSchema';
import { useSelectedProjectId } from '../../../lib/flow-editor/hooks/useSelectedProjectId';
import { useToolkitTypeSchemas } from '../../../lib/flow-editor/hooks/useToolkitTypeSchemas';
import type { FlowEdge } from '../../../lib/flow-editor/reactFlowTypes';
import { CommonInterruptSettings } from '../../settings/CommonInterruptSettings';
import { InputSelect } from '../../select/InputSelect';
import { OutputSelect } from '../../select/OutputSelect';
import { ToolSelect } from '../../select/ToolSelect';
import type { PipelineToolEntry } from '../../select/pipelineToolEntry.types';
import { NodeCard } from '../BaseNode/NodeCard';
import { SimpleNodeHandles } from './SimpleNodeHandles';
import { useFlowEditorNodeContext } from './useFlowEditorNodeContext';
import { useToolNodeEditing, useToolNodeState } from './useToolNodeEditing';

import { useEdges } from '@xyflow/react';

const isNotApplicationTool = (tool: PipelineToolEntry): boolean => tool.type !== 'application';

interface ToolNodeFunctionSelectProps {
  readonly options: readonly { readonly label: string; readonly value: string }[];
  readonly value: string;
  readonly onChange: (value: string) => void;
  readonly disabled: boolean;
  /** A read that feeds this picker failed — the tool catalogue, or the toolkit type schemas (#440). */
  readonly readFailed: boolean;
  readonly onRetry: () => void;
}

/**
 * The "Tool" picker, or the error that replaces it (#440).
 *
 * This node used to render nothing when the option list was empty, whatever
 * the cause: a toolkit with no tools, and a lost read, looked the same. An
 * empty list keeps one meaning now, and a failed read gets a visible state
 * with a retry. A failed read that still produced options keeps the working
 * picker, because the list on screen is real. Split out as its own component
 * to keep `ToolNode` under the §3.5 complexity budget.
 */
function ToolNodeFunctionSelect({ options, value, onChange, disabled, readFailed, onRetry }: ToolNodeFunctionSelectProps): ReactNode {
  if (options.length === 0 && readFailed) {
    return (
      <ToolListError
        onRetry={onRetry}
        testId="tool-node-tool-list-error"
      />
    );
  }
  if (options.length === 0) return null;
  return (
    <SingleSelect
      label={t('pipelines.flowEditor.deprecated.toolNode.tool', 'Tool')}
      value={value}
      onChange={onChange}
      options={[...options]}
      disabled={disabled}
    />
  );
}

export interface ToolNodeProps {
  readonly id: string;
  readonly data?: { readonly isPerforming?: boolean } | undefined;
  readonly selected?: boolean | undefined;
  /** See module doc comment, deviation 1. */
  readonly versionTools?: readonly PipelineToolEntry[] | undefined;
}

export const ToolNode = memo(function ToolNode(props: ToolNodeProps): ReactNode {
  const { id, data = {}, selected = false, versionTools = [] } = props;

  const edges = useEdges<FlowEdge>();
  const { yamlJsonObject, setYamlJsonObject, isRunningPipeline, disabled } = useFlowEditorNodeContext();
  const runningOrDisabled = Boolean(isRunningPipeline) || Boolean(disabled);
  const isPerforming = Boolean(data.isPerforming);

  const projectId = useSelectedProjectId();
  // `isError` has a reader now (#440): the type schemas supply the tool list
  // of a statically-declared toolkit type, so a lost read empties this
  // node's picker just as a lost catalogue read does.
  const { toolkitTypeSchemas, isError: typeSchemasReadFailed, refetch: retryTypeSchemasRead } = useToolkitTypeSchemas(projectId);
  const { getToolkitNameFromSchema, getSelectedTools } = useGetToolkitNameFromSchema(toolkitTypeSchemas);

  const { toolkit, selectedToolkit, taskValue, toolValue } = useToolNodeState(id, yamlJsonObject, versionTools, getToolkitNameFromSchema);

  // The catalogue tier: a toolkit that declares no `selected_tools` of its
  // own publishes its tools at run time — the same gate `LoopToolSelect` uses.
  const dynamicTools = toolkitTools.useToolkitTools({
    projectId,
    toolkitId: selectedToolkit?.id,
    toolkitType: selectedToolkit?.type,
    enabled: selectedToolkit !== undefined && (selectedToolkit.settings?.selected_tools ?? []).length === 0,
  });

  const { onSelectToolkit, handleSetTask, handleSetTool, functionOptions } = useToolNodeEditing({
    id,
    selectedToolkit,
    getToolkitNameFromSchema,
    getSelectedTools,
    yamlJsonObject,
    setYamlJsonObject,
    dynamicToolNames: dynamicTools.toolNames,
  });

  // Both reads feed the same picker, so one retry control must run both.
  const onRetryToolList = useCallback(() => {
    dynamicTools.refetch();
    retryTypeSchemasRead();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `dynamicTools` is a fresh object every render; its `refetch` is the stable identity this depends on
  }, [dynamicTools.refetch, retryTypeSchemasRead]);

  return (
    <NodeCard
      name={id}
      isEntrypoint={yamlJsonObject?.entry_point === id}
      selected={selected}
      type={PipelineNodeTypes.Tool}
      isPerforming={isPerforming}
      id={id}
      handles={() => (
        <SimpleNodeHandles
          id={id}
          edges={edges}
          isRunningPipeline={Boolean(isRunningPipeline)}
          disabled={Boolean(disabled)}
          isPerforming={isPerforming}
        />
      )}
    >
      <ToolSelect
        onSelectTool={onSelectToolkit}
        selectedToolkit={toolkit}
        disabled={runningOrDisabled}
        filterTypes={isNotApplicationTool}
        versionTools={versionTools}
      />
      <ToolNodeFunctionSelect
        options={functionOptions}
        value={toolValue}
        onChange={handleSetTool}
        disabled={runningOrDisabled}
        readFailed={dynamicTools.isError || typeSchemasReadFailed}
        onRetry={onRetryToolList}
      />
      <StyledInputEnhancer
        disabled={runningOrDisabled}
        autoComplete="off"
        fullWidth
        name="task"
        label={t('pipelines.flowEditor.deprecated.toolNode.task', 'Task')}
        value={taskValue}
        onChange={handleSetTask}
        expand={{ minRows: 1, maxRows: 3 }}
        actions={{ showCopy: false, showExpand: false }}
      />
      <InputSelect
        id={id}
        label={t('pipelines.flowEditor.deprecated.toolNode.input', 'Input')}
        inputFieldName="input"
        disabled={runningOrDisabled}
      />
      <OutputSelect
        id={id}
        label={t('pipelines.flowEditor.deprecated.toolNode.output', 'Output')}
        outputFieldName="output"
        disabled={runningOrDisabled}
      />
      <CommonInterruptSettings
        id={id}
        type={PipelineNodeTypes.Tool}
        disabled={runningOrDisabled}
      />
    </NodeCard>
  );
});
