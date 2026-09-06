import type { ReactNode } from 'react';
import { useCallback, useMemo } from 'react';

import type { SxProps, Theme } from '@mui/material/styles';

import { toolkitTools } from '@/entities/toolkit';
import { t } from '@/shared/i18n';
import { SingleSelect, type SingleSelectOption } from '@/shared/ui/SingleSelect';
import { ToolListError } from '@/shared/ui/ToolListError';

import { getToolName } from '../../lib/flow-editor/helpers/flowEditor.helpers';
import type { YamlPipelineNode } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import { useGetToolkitNameFromSchema } from '../../lib/flow-editor/hooks/useGetToolkitNameFromSchema';
import { useSelectedProjectId } from '../../lib/flow-editor/hooks/useSelectedProjectId';
import { useToolkitTypeSchemas } from '../../lib/flow-editor/hooks/useToolkitTypeSchemas';
import { EntityOptionIcon, resolvePipelineToolEntityType } from './EntityOptionIcon';
import type { PipelineToolEntry } from './pipelineToolEntry.types';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * select/LoopToolSelect.jsx` (unit A2h) -- deprecated-node picker (a
 * toolkit select plus a tool/function select), consumed by the
 * `Loop`/`LoopFromTool` node UI (`../../lib/flow-editor/constants/
 * deprecated.constants.ts`).
 *
 * DEVIATIONS FROM BASELINE, both following this batch's own precedent
 * (`../../lib/flow-editor/hooks/useFunctionInputMapping.ts`'s header):
 *
 *  1. `useFormikContext()` -> explicit `versionTools` prop (no Formik).
 *  2. `useGetToolkitNameFromSchema()`/`useGetToolkitIconMeta()` -> locally
 *     resolved `toolkitTypeSchemas` + `EntityOptionIcon`'s entity-type
 *     fallback (see `ToolSelect.tsx`'s identical deviation #2).
 *
 * **CORRECTED (#440): the dynamic tool discovery is REAL and this
 * component now reads it.** This header used to say that no endpoint for
 * it existed. That was wrong. `internal/api/router.go` registers
 * `toolkit_available_tools` (:1912) and `toolkit_discover_tools` (:1914);
 * only the OpenAPI spec, and so the generated client, lacked them.
 * `entities/toolkit`'s `toolkitTools.useToolkitTools` is the hand-written
 * client (the `usePipelineTrigger.ts` shape this header asked for), and
 * `shared/api/endpoints.manifest.json` carries both routes.
 *
 * `functionOptions` therefore resolves in the baseline's own order: the
 * toolkit's explicit `selected_tools`, else the catalogue read from the
 * backend for the selected toolkit.
 *
 * **A FAILED READ IS ITS OWN STATE.** #381 made both routes answer a failed
 * database read with an error rather than `200 {"tools":[]}`. This
 * component renders an error with a retry action for that case, so an empty
 * picker keeps its one meaning: the toolkit offers no tools.
 *
 * **Kept from the earlier mitigation:** `functionOptions` still synthesises
 * a single-entry fallback option for the node's CURRENT `tool` value when
 * it is not otherwise in the list, the same "value present but absent from
 * the known options list" pattern `PipelineMultiSelect.tsx`'s own
 * `canDelete` synthesis and `InputSelect.tsx`'s `realInputOptions` use. It
 * keeps an already-configured value visible and clearable while the
 * catalogue is still loading, or when the toolkit no longer offers it.
 *
 * `useLoopToolSelection` bundles every derived-options computation into
 * one hook purely to keep this component's own cyclomatic complexity under
 * the §3.5 budget (12); no other behaviour change.
 */
export interface LoopToolSelectProps {
  readonly yamlNode?: YamlPipelineNode | undefined;
  readonly disabled?: boolean | undefined;
  readonly label?: string;
  readonly toolkitField?: string;
  readonly toolField?: string;
  readonly onChangeToolkit?: (value: string | null) => void;
  readonly onChangeTool?: (value: string) => void;
  readonly versionTools?: readonly PipelineToolEntry[];
}

interface ToolkitOption extends SingleSelectOption {
  readonly originalTool: PipelineToolEntry;
}

const noop = (): void => {};
/** A stable empty list — see `useLoopToolSelection`'s own note on why the fallback may not be a fresh literal. */
const NO_TOOLS: readonly string[] = [];
const toolkitSelectSx: SxProps<Theme> = { marginBottom: '0rem' };
const toolSelectSx: SxProps<Theme> = { marginBottom: '0rem' };

interface LoopToolSelection {
  readonly toolkits: readonly ToolkitOption[];
  readonly toolkit: string | undefined;
  readonly functionOptions: readonly SingleSelectOption[];
  /** The tool-catalogue read failed. Render it as its own state, never as an empty picker (#440). */
  readonly toolsReadFailed: boolean;
  readonly retryToolsRead: () => void;
}

function useLoopToolSelection(
  yamlNode: YamlPipelineNode | undefined,
  toolkitField: string,
  toolField: string,
  versionTools: readonly PipelineToolEntry[],
  selectedTool: string,
): LoopToolSelection {
  const projectId = useSelectedProjectId();
  // `isError` has a reader now (#440). The type schemas name the toolkit a
  // node points at, so a lost read can leave this picker with nothing to
  // show — an empty picker must not stand for that.
  const { toolkitTypeSchemas, isError: typeSchemasReadFailed, refetch: retryTypeSchemasRead } = useToolkitTypeSchemas(projectId);
  const { getToolkitNameFromSchema } = useGetToolkitNameFromSchema(toolkitTypeSchemas);

  const toolkits = useMemo<ToolkitOption[]>(
    () =>
      versionTools.map((tool): ToolkitOption => {
        const name = tool.type === 'application' ? (tool.name ?? '') : (tool.toolkit_name || getToolkitNameFromSchema(tool));
        return { label: name, value: name, icon: <EntityOptionIcon entityType={resolvePipelineToolEntityType(tool)} />, originalTool: tool };
      }),
    [getToolkitNameFromSchema, versionTools],
  );

  const toolkit = useMemo(() => {
    if (!yamlNode) return undefined;
    return (yamlNode[toolkitField] as string | undefined) ?? (yamlNode[toolField] as string | undefined) ?? yamlNode.id;
  }, [yamlNode, toolField, toolkitField]);

  const selectedToolkit = useMemo(
    () => versionTools.find(tool => tool.toolkit_name === toolkit || tool.name === toolkit || toolkit === getToolkitNameFromSchema(tool)),
    [getToolkitNameFromSchema, toolkit, versionTools],
  );

  // `NO_TOOLS`, not a fresh `[]`: this value is a `useMemo` dependency below,
  // and a new array literal on every render would rebuild the option list
  // every time for a toolkit that has no explicit selection at all.
  const enabledTools = selectedToolkit?.settings?.selected_tools ?? NO_TOOLS;

  // The dynamic catalogue (#440). It runs only for a toolkit with no
  // explicit `selected_tools`, which is the case the baseline fetched for.
  const dynamicTools = toolkitTools.useToolkitTools({
    projectId,
    toolkitId: selectedToolkit?.id,
    toolkitType: selectedToolkit?.type,
    enabled: selectedToolkit !== undefined && enabledTools.length === 0,
  });

  // The node's OWN currently-configured `selectedTool` is added as a
  // single-entry fallback when it is not otherwise in the list, so an
  // already-configured value stays visible and clearable while the
  // catalogue loads.
  const functionOptions = useMemo<SingleSelectOption[]>(() => {
    const names = enabledTools.length > 0 ? enabledTools.map(getToolName) : dynamicTools.toolNames;
    const options = names.map(name => ({ label: name, value: name }));
    if (selectedTool && !options.some(option => option.value === selectedTool)) {
      options.push({ label: selectedTool, value: selectedTool });
    }
    return options;
  }, [dynamicTools.toolNames, enabledTools, selectedTool]);

  // Both reads feed the same picker, so one retry control must run both.
  const { refetch: retryDynamicToolsRead } = dynamicTools;
  const retryToolsRead = useCallback(() => {
    retryDynamicToolsRead();
    retryTypeSchemasRead();
  }, [retryDynamicToolsRead, retryTypeSchemasRead]);

  // A lost type-schema read that still left options on screen keeps the
  // working picker: the list the user sees is real.
  const toolsReadFailed = dynamicTools.isError || (typeSchemasReadFailed && functionOptions.length === 0);

  return { toolkits, toolkit, functionOptions, toolsReadFailed, retryToolsRead };
}

interface ToolPickerProps {
  readonly readFailed: boolean;
  readonly onRetry: () => void;
  readonly label: string;
  readonly value: string;
  readonly options: readonly SingleSelectOption[];
  readonly onSelect: (value: string) => void;
  readonly disabled: boolean;
}

/**
 * The tool picker, or the error that replaces it (#440).
 *
 * A failed catalogue read is its own state. A missing picker means the
 * toolkit offers no tools; it must never stand in for a failure. Split out
 * to keep `LoopToolSelect` under the §3.5 complexity budget.
 */
function ToolPicker({ readFailed, onRetry, label, value, options, onSelect, disabled }: ToolPickerProps): ReactNode {
  if (readFailed) {
    return (
      <ToolListError
        onRetry={onRetry}
        testId="loop-tool-list-error"
      />
    );
  }
  if (options.length === 0) return null;
  return (
    <SingleSelect
      sx={toolSelectSx}
      label={label}
      value={value}
      onChange={onSelect}
      options={[...options]}
      disabled={disabled}
    />
  );
}

export function LoopToolSelect(props: LoopToolSelectProps): ReactNode {
  const {
    yamlNode,
    disabled = false,
    label = 'Toolkit',
    toolkitField = 'toolkit_name',
    toolField = 'tool',
    onChangeToolkit = noop,
    onChangeTool = noop,
    versionTools = [],
  } = props;

  const selectedTool = useMemo(() => (yamlNode ? ((yamlNode[toolField] as string | undefined) ?? '') : ''), [toolField, yamlNode]);
  const { toolkits, toolkit, functionOptions, toolsReadFailed, retryToolsRead } = useLoopToolSelection(yamlNode, toolkitField, toolField, versionTools, selectedTool);

  const handleToolkitChange = useCallback((newValue: string) => onChangeToolkit(newValue), [onChangeToolkit]);
  const onClear = useCallback(() => onChangeToolkit(null), [onChangeToolkit]);

  const toolLabel = toolField === 'tool' ? t('pipelines.loopToolSelect.tool', 'Tool') : t('pipelines.loopToolSelect.loopTool', 'Loop tool');

  return (
    <>
      <SingleSelect
        sx={toolkitSelectSx}
        label={label}
        value={toolkit ?? ''}
        onChange={handleToolkitChange}
        onClear={onClear}
        options={[...toolkits]}
        disabled={disabled || toolkits.length === 0}
      />
      <ToolPicker
        readFailed={toolsReadFailed}
        onRetry={retryToolsRead}
        label={toolLabel}
        value={selectedTool}
        options={functionOptions}
        onSelect={onChangeTool}
        disabled={disabled}
      />
    </>
  );
}
