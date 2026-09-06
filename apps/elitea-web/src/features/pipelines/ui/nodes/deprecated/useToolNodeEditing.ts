/**
 * `ToolNode.tsx`'s editing logic (baseline: `ToolNode.jsx:34-138`), split
 * into its own hook purely to keep `ToolNode.tsx` under the §3.5
 * `complexity` budget (12) — see `useConditionNodeEditing.ts`'s own doc
 * comment for the identical rationale (moving branching logic to a
 * separate function/file keeps its complexity from summing into the
 * component's own count). No behaviour change from the extraction.
 *
 * CORRECTED (#440): this comment used to say that no endpoint for dynamic
 * tool-name discovery existed. It does exist. `internal/api/router.go`
 * registers `toolkit_available_tools` (:1912) and `toolkit_discover_tools`
 * (:1914), and `entities/toolkit`'s `toolkitTools.useToolkitTools` is the
 * client for both — `ui/select/LoopToolSelect.tsx` already reads it.
 *
 * The wiring gap is closed. `functionOptions` falls back to the catalogue
 * names the caller reads with that hook, and `ToolNode.tsx` renders a failed
 * read as its own state with a retry, the way `LoopToolSelect.tsx` does. The
 * read itself stays in the component, not here, because this hook is pure
 * derivation over values the component already holds — the same split
 * `useToolNodeState` above already follows.
 */
import { useCallback, useMemo } from 'react';
import type { ChangeEvent } from 'react';

import { batchUpdateYamlNode, getToolName, updateYamlNode } from '../../../lib/flow-editor/helpers/flowEditor.helpers';
import type { YamlPipelineDocument, YamlPipelineNode } from '../../../lib/flow-editor/helpers/pipelineFlow.types';
import type { PipelineToolEntry } from '../../select/pipelineToolEntry.types';

export interface ToolNodeState {
  readonly yamlNode: YamlPipelineNode | undefined;
  readonly toolkit: string;
  readonly selectedToolkit: PipelineToolEntry | undefined;
  readonly taskValue: string;
  readonly toolValue: string;
}

// `||`, not `??` (baseline: `yamlNode?.toolkit_name || yamlNode?.tool || id`,
// `ToolNode.jsx:32`) -- an empty-string `toolkit_name`/`tool` must fall
// through to the next candidate the same way baseline's `||` chain does;
// `??` would keep `''` as the resolved toolkit identity instead.
function resolveToolkit(toolkitName: string | undefined, tool: string | undefined, id: string): string {
  return toolkitName || tool || id;
}

/**
 * `tool.toolkit_name` if present, else `tool` with `toolkit_name` filled in
 * via `getToolkitNameFromSchema` (baseline: `ToolNode.jsx:100-112`'s
 * `.map()` step). Needed so a `versionTools` entry whose canonical
 * `toolkit_name` only exists via schema (no literal `toolkit_name` field)
 * still carries a comparable `toolkit_name` into {@link findSelectedToolkit}'s
 * `.find()` below.
 */
function withDerivedToolkitName(tool: PipelineToolEntry, getToolkitNameFromSchema: (toolkit: PipelineToolEntry) => string): PipelineToolEntry {
  return tool.toolkit_name ? tool : { ...tool, toolkit_name: getToolkitNameFromSchema(tool) };
}

/**
 * `ToolNode.jsx:100-112`'s `selectedToolkit` derivation, exactly:
 * schema-derive every entry's `toolkit_name` first (see
 * {@link withDerivedToolkitName}), THEN find by `toolkit_name === toolkit ||
 * name === toolkit` -- the `|| name === toolkit` fallback applies
 * regardless of which branch derived `toolkit_name`, so it is kept
 * unconditional here too (unlike the superficially similar
 * `useLoopNodeEditing.ts`/`useLoopToolNodeEditing.ts` `getSelectedToolkit`,
 * which only falls back to `name`/schema when `toolkit_name` is itself
 * absent -- that is *their* baseline's own, different rule, not this one's).
 */
function findSelectedToolkit(
  versionTools: readonly PipelineToolEntry[],
  toolkit: string,
  getToolkitNameFromSchema: (toolkit: PipelineToolEntry) => string,
): PipelineToolEntry | undefined {
  return versionTools.map(tool => withDerivedToolkitName(tool, getToolkitNameFromSchema)).find(tool => tool.toolkit_name === toolkit || tool.name === toolkit);
}

/**
 * `yamlNode`/`toolkit`/`selectedToolkit`/`taskValue`/`toolValue` derivation
 * (baseline: `ToolNode.jsx:22-33,138-140`), split out for the same
 * `complexity`-budget reason as `useToolNodeEditing` above.
 */
export function useToolNodeState(
  id: string,
  yamlJsonObject: YamlPipelineDocument | undefined,
  versionTools: readonly PipelineToolEntry[],
  getToolkitNameFromSchema: (toolkit: PipelineToolEntry) => string,
): ToolNodeState {
  const yamlNode = useMemo(() => yamlJsonObject?.nodes?.find(node => node.id === id), [id, yamlJsonObject?.nodes]);
  const toolkit = useMemo(() => resolveToolkit(yamlNode?.toolkit_name, yamlNode?.tool, id), [id, yamlNode?.tool, yamlNode?.toolkit_name]);
  const selectedToolkit = useMemo(
    () => findSelectedToolkit(versionTools, toolkit, getToolkitNameFromSchema),
    [getToolkitNameFromSchema, toolkit, versionTools],
  );

  return { yamlNode, toolkit, selectedToolkit, taskValue: yamlNode?.task ?? '', toolValue: yamlNode?.tool ?? '' };
}

interface ToolOption {
  readonly label: string;
  readonly value: string;
}

/** A stable empty list. A fresh `[]` default would rebuild `functionOptions` on every render. */
const NO_DYNAMIC_TOOL_NAMES: readonly string[] = [];

export interface UseToolNodeEditingArgs {
  readonly id: string;
  readonly selectedToolkit: PipelineToolEntry | undefined;
  readonly getToolkitNameFromSchema: (toolkit: PipelineToolEntry) => string;
  readonly getSelectedTools: (type: string) => readonly string[];
  readonly yamlJsonObject: YamlPipelineDocument | undefined;
  readonly setYamlJsonObject: ((next: YamlPipelineDocument) => void) | undefined;
  /** The tool names the backend publishes for the selected toolkit (#440). Used when the toolkit declares no `selected_tools` of its own. */
  readonly dynamicToolNames?: readonly string[];
}

export interface UseToolNodeEditingResult {
  readonly onSelectToolkit: (newToolkit: PipelineToolEntry | null) => void;
  readonly handleSetTask: (event: ChangeEvent<HTMLInputElement>) => void;
  readonly handleSetTool: (value: string) => void;
  readonly functionOptions: readonly ToolOption[];
}

// `||`, not `??` (baseline: `newToolkit.toolkit_name || getToolkitNameFromSchema(newToolkit)`,
// `ToolNode.jsx:56`) -- an empty-string `toolkit_name` on the picked
// toolkit must fall through to the schema-derived name, same rationale as
// `resolveToolkit` above.
function resolveToolkitName(newToolkit: PipelineToolEntry, getToolkitNameFromSchema: (tool: PipelineToolEntry) => string): string | undefined {
  if (newToolkit.type === 'application') return undefined;
  return newToolkit.toolkit_name || getToolkitNameFromSchema(newToolkit);
}

function resolveExplicitTool(newToolkit: PipelineToolEntry): string | undefined {
  return newToolkit.type === 'application' ? newToolkit.name : undefined;
}

function resolveFunctionOptions(
  selectedToolkit: PipelineToolEntry | undefined,
  getSelectedTools: (type: string) => readonly string[],
  dynamicToolNames: readonly string[],
): readonly ToolOption[] {
  const explicitSelected: readonly string[] | undefined = selectedToolkit?.settings?.selected_tools;
  // The catalogue tier (#440): a toolkit that declares no `selected_tools`
  // publishes its tools at run time, and this list used to stay empty.
  if (!Array.isArray(explicitSelected) || explicitSelected.length === 0) {
    return dynamicToolNames.map(name => ({ label: getToolName(name), value: getToolName(name) })).sort((a, b) => a.label.localeCompare(b.label));
  }

  const availableTools = getSelectedTools(selectedToolkit?.type ?? '');
  const enabledTools: readonly string[] =
    availableTools.length > 0 ? explicitSelected.filter((tool: string) => availableTools.includes(tool)) : explicitSelected;

  return enabledTools.map(item => ({ label: getToolName(item), value: getToolName(item) })).sort((a, b) => a.label.localeCompare(b.label));
}

export function useToolNodeEditing(args: UseToolNodeEditingArgs): UseToolNodeEditingResult {
  const { id, selectedToolkit, getToolkitNameFromSchema, getSelectedTools, yamlJsonObject, setYamlJsonObject, dynamicToolNames = NO_DYNAMIC_TOOL_NAMES } = args;

  const onSelectToolkit = useCallback(
    (newToolkit: PipelineToolEntry | null) => {
      if (!yamlJsonObject || !setYamlJsonObject) return;
      if (!newToolkit) {
        batchUpdateYamlNode(id, { toolkit_name: undefined, tool: undefined }, yamlJsonObject, setYamlJsonObject);
        return;
      }
      batchUpdateYamlNode(
        id,
        { toolkit_name: resolveToolkitName(newToolkit, getToolkitNameFromSchema), tool: resolveExplicitTool(newToolkit) },
        yamlJsonObject,
        setYamlJsonObject,
      );
    },
    [getToolkitNameFromSchema, id, setYamlJsonObject, yamlJsonObject],
  );

  const handleSetTask = useCallback(
    (event: ChangeEvent<HTMLInputElement>) => {
      if (!yamlJsonObject || !setYamlJsonObject) return;
      updateYamlNode(id, 'task', event.target.value, yamlJsonObject, setYamlJsonObject);
    },
    [id, setYamlJsonObject, yamlJsonObject],
  );

  const handleSetTool = useCallback(
    (value: string) => {
      if (!yamlJsonObject || !setYamlJsonObject) return;
      batchUpdateYamlNode(id, { tool: value }, yamlJsonObject, setYamlJsonObject);
    },
    [id, setYamlJsonObject, yamlJsonObject],
  );

  const functionOptions = useMemo(
    () => resolveFunctionOptions(selectedToolkit, getSelectedTools, dynamicToolNames),
    [dynamicToolNames, getSelectedTools, selectedToolkit],
  );

  return { onSelectToolkit, handleSetTask, handleSetTool, functionOptions };
}
