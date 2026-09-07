/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/lib/
 * hooks/useFunctionInputMapping.hooks.js` (256 lines, unit A2d) — computes
 * a tool/toolkit node's `input_mapping` defaults from its JSON-Schema
 * settings, keeping it in sync as the selected tool/toolkit changes.
 *
 * **DISCLOSED REDESIGNs (all following this batch's own established
 * conventions, cited per-item):**
 *
 * 1. No Formik (`useFormikContext()` -> `values.version_details.tools`). No
 *    Formik dependency exists (`react-hook-form`, `package.json`) — the
 *    caller passes `versionTools` directly, matching
 *    `features/mcps/ui/McpAuthStatusBadge.tsx`'s "DEVIATION FROM BASELINE".
 * 2. No Redux (`useGetToolkitNameFromSchema`'s `schemaOfTools` selector) —
 *    `../hooks/useGetToolkitNameFromSchema.ts`'s own header covers this;
 *    `toolkitTypes` (below) is threaded straight into it.
 * 3. `useGetCurrentToolkitSchemas` -> `./useToolkitTypeSchemas.ts` (local
 *    duplicate of `features/apps/api/useToolkitTypeSchemas.ts`, itself
 *    already the established replacement for this exact baseline hook —
 *    see that file's header).
 * 4. `useSelectedProjectId` -> `./useSelectedProjectId.ts` (local
 *    duplicate of `features/apps/api/useSelectedProjectId.ts`).
 * 5. **CORRECTED (#440): the endpoint exists; the ARGUMENT SCHEMA does
 *    not.** This item used to say that no `toolkit_available_tools`
 *    endpoint existed. It does: `internal/api/router.go:1912` registers it,
 *    `toolkit_discover_tools` sits at :1914, and `entities/toolkit`'s
 *    `toolkitTools.useToolkitTools` reads both.
 *
 *    That does NOT unblock this hook. Both routes return tool
 *    `id`/`name`/`type`/`description` only — their repository query selects
 *    exactly those four columns (`internal/api/v2/toolkits/handler.go:1086`)
 *    — and this hook needs `args_schemas`, which no column carries. So
 *    `dynamicArgsSchemas` stays empty, and a dynamic (non-statically-
 *    schema'd, non-MCP) tool still cannot get real input-mapping defaults
 *    here. Static schemas (`toolkitTypes[...].properties.selected_tools.
 *    args_schemas`) and the MCP `available_mcp_tools` path both still work;
 *    `isSchemaResolved` (below) keeps this from silently wiping an
 *    already-saved `input_mapping` to `{}` while unresolved.
 *
 *    **The names half is wired now.** `dynamicToolNames` no longer returns
 *    a frozen empty array. It reads the catalogue through
 *    `toolkitTools.useToolkitTools`, the same client `ui/select/
 *    LoopToolSelect.tsx` uses, and it reports a failed read through
 *    `dynamicToolsReadFailed` so the node UI can show an error instead of
 *    an empty tool picker. `dynamicArgsSchemas` stays empty until the
 *    backend change above lands.
 *

 * `isMcpToolkit` is inlined rather than imported from `entities/toolkit`:
 * that export's parameter type (`Toolkit`) requires `id`/`name` fields this
 * hook's `selectedToolkit` (a `version_details.tools[]` entry, not a full
 * catalogue `Toolkit`) does not carry — same 3-line check,
 * `shared/lib/helpers/mcp.helpers.js:7-14`, `entities/toolkit/model/
 * selectors.ts`'s own citation for its version.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';

import { toolkitTools } from '@/entities/toolkit';

import * as FlowEditorHelpers from '../helpers/flowEditor.helpers';
import type { YamlInputMappingEntry, YamlPipelineDocument, YamlPipelineNode } from '../helpers/pipelineFlow.types';
import { useGetToolkitNameFromSchema } from './useGetToolkitNameFromSchema';
import { useSelectedProjectId } from './useSelectedProjectId';
import { useToolkitTypeSchemas } from './useToolkitTypeSchemas';

export interface VersionTool {
  readonly id?: string;
  readonly type?: string;
  readonly name?: string;
  readonly toolkit_name?: string;
  readonly meta?: { readonly mcp?: boolean };
  readonly settings?: Readonly<Record<string, unknown>> & {
    readonly available_mcp_tools?: readonly { readonly value?: string; readonly label?: string; readonly args_schema?: unknown }[];
  };
}

/**
 * `shared/lib/helpers/mcp.helpers.js:7-14` / `entities/toolkit/model/
 * selectors.ts`'s `isMcpToolkit` — the flag lives on the toolkit's own
 * top-level `meta.mcp`, NOT nested under `settings`. Must stay aligned with
 * this exact same predicate re-declared (for its own, differently-shaped
 * `ToolkitLike` parameter) as `isMcpToolkit` in the sibling
 * `../helpers/flowEditorInputMapping.helpers.ts` this hook calls into.
 */
function isMcpToolkitLike(toolkit: VersionTool | undefined): boolean {
  if (!toolkit?.type) return false;
  if (toolkit.type === 'mcp' || toolkit.type.startsWith('mcp_')) return true;
  return toolkit.meta?.mcp === true;
}

export interface UseFunctionInputMappingArgs {
  readonly id: string;
  readonly yamlJsonObject: YamlPipelineDocument;
  readonly setYamlJsonObject: (next: YamlPipelineDocument) => void;
  readonly versionTools: readonly VersionTool[] | undefined;
}

export interface UseFunctionInputMappingResult {
  readonly toolkitTypes: unknown;
  readonly onChangeTool: (newValue: string | undefined) => void;
  readonly onChangeMapping: (variable: string, value: { type?: string; value: unknown }, dataType?: string) => void;
  readonly requiredInputs: readonly string[];
  readonly mappingInfo: Record<string, unknown>;
  readonly inputMappings: Record<string, unknown>;
  readonly defaultValues: Record<string, unknown>;
  readonly selectedToolkit: VersionTool | undefined;
  /** The tool names the backend publishes for the selected toolkit. Empty while no toolkit is selected, or when the toolkit carries its own `selected_tools` list. */
  readonly dynamicToolNames: readonly string[];
  /**
   * The catalogue read failed (#440). A caller must render this as its own
   * state. An empty `dynamicToolNames` means the toolkit offers no tools; it
   * must never stand in for a failed read.
   */
  readonly dynamicToolsReadFailed: boolean;
  /** Reads the catalogue again. Connect it to the retry control of the error state. */
  readonly retryDynamicToolsRead: () => void;
  readonly dynamicArgsSchemas: Readonly<Record<string, unknown>>;
  readonly selectedTool: string;
  readonly toolkit: string | undefined;
  /**
   * Whether a real tool schema was found for `selectedTool` (an MCP
   * toolkit's `available_mcp_tools`) — `false` for a static-schema'd
   * toolkit type does NOT mean "no inputs needed", it means this hook
   * could not resolve one; see file header item 5's disclosed backend gap.
   * Exposed so a future pass on the consuming node/select UI (out of this
   * file's scope) can warn instead of rendering an empty input list.
   */
  readonly isSchemaResolved: boolean;
}

const TOOLKIT_NODE_TYPES = new Set(['agent', 'toolkit', 'mcp']);

/**
 * `useFunctionInputMapping.hooks.js:24-33`. For a node without `toolkit_name`/
 * `tool` that also isn't one of the toolkit-bearing node types, the node id
 * IS the toolkit identifier (backward compatibility with pre-migration YAML).
 */
function resolveToolkitIdentifier(id: string, yamlNode: YamlPipelineNode | undefined): string | undefined {
  const hasExplicitToolkitRef = Boolean(yamlNode?.toolkit_name) || Boolean(yamlNode?.tool) || TOOLKIT_NODE_TYPES.has(yamlNode?.type ?? '');
  if (!hasExplicitToolkitRef) return id;
  return yamlNode?.toolkit_name ?? yamlNode?.tool;
}

interface ShouldWriteArgs {
  readonly existingInputMapping: readonly string[];
  readonly requiredInputs: readonly string[];
  readonly selectedTool: string;
  readonly initialTool: string;
  readonly initialToolkit: string | undefined;
  readonly toolkit: string | undefined;
}

/**
 * Only update if there is no existing mapping, or the tool/toolkit has
 * changed, or the required-inputs set has changed — split out purely to
 * keep the effect body under §3.5's cyclomatic-complexity budget
 * (baseline: `useFunctionInputMapping.hooks.js:140-146`).
 */
function shouldWriteDefaultMapping({ existingInputMapping, requiredInputs, selectedTool, initialTool, initialToolkit, toolkit }: ShouldWriteArgs): boolean {
  return (
    !existingInputMapping.length ||
    requiredInputs.some(input => !existingInputMapping.includes(input)) ||
    Boolean(selectedTool && initialTool !== selectedTool) ||
    initialToolkit !== toolkit
  );
}

/**
 * Filter mapping to only include required fields or fields with a non-empty
 * value — prevents optional empty parameters from being added to YAML
 * initially (baseline: `useFunctionInputMapping.hooks.js:147-155`).
 */
function filterRequiredOrNonEmpty(mapping: Record<string, unknown>, requiredInputs: readonly string[]): Record<string, unknown> {
  return Object.entries(mapping).reduce<Record<string, unknown>>((result, [key, value]) => {
    const isRequired = requiredInputs.includes(key);
    const entry = value as { value?: unknown } | undefined;
    const hasValue = entry?.value !== '' && entry?.value !== undefined;
    if (isRequired || hasValue) result[key] = value;
    return result;
  }, {});
}

/**
 * Real, disclosed gap (see file header, item 5): the two tool-catalogue
 * routes carry no argument schema, so these stay empty — a module-level
 * constant (not a per-render `useMemo`) to say so plainly: this is a
 * structural absence of data, not a reactive value that could change once
 * dependencies settle. The tool NAMES beside it are read from the backend;
 * see `useToolkitTools` below.
 */
const EMPTY_DYNAMIC_ARGS_SCHEMAS: Readonly<Record<string, unknown>> = {};

/**
 * Whether the catalogue read is the tier that feeds this node's tool picker.
 *
 * It is, for a selected toolkit that declares no `selected_tools` list of its
 * own — the toolkit type that publishes its tools at run time, and the case
 * the baseline fetched for. `ui/select/LoopToolSelect.tsx` uses the same gate.
 * Split out of the hook body to keep it under the §3.5 complexity budget.
 */
function shouldReadToolCatalogue(toolkit: VersionTool | undefined): boolean {
  if (toolkit === undefined) return false;
  const selected = toolkit.settings?.['selected_tools'];
  return !Array.isArray(selected) || selected.length === 0;
}

export function useFunctionInputMapping({ id, yamlJsonObject, setYamlJsonObject, versionTools }: UseFunctionInputMappingArgs): UseFunctionInputMappingResult {
  const projectId = useSelectedProjectId();
  const { toolkitTypeSchemas: toolkitTypes } = useToolkitTypeSchemas(projectId);
  const yamlNode = useMemo(() => yamlJsonObject.nodes?.find(node => node.id === id), [id, yamlJsonObject.nodes]);

  const toolkit = useMemo(() => resolveToolkitIdentifier(id, yamlNode), [id, yamlNode]);

  const selectedTool = yamlNode?.tool ?? '';
  const [initialToolkit] = useState(toolkit);
  const [initialTool] = useState(yamlNode?.tool ?? '');
  const { getToolkitNameFromSchema } = useGetToolkitNameFromSchema(toolkitTypes);

  const selectedToolkit = useMemo(
    () =>
      (versionTools ?? [])
        .map(tool => (tool.toolkit_name ? tool : { ...tool, toolkit_name: getToolkitNameFromSchema(tool) }))
        .find(tool => tool.toolkit_name === toolkit || tool.name === toolkit),
    [getToolkitNameFromSchema, toolkit, versionTools],
  );

  const dynamicArgsSchemas: Readonly<Record<string, unknown>> = EMPTY_DYNAMIC_ARGS_SCHEMAS;

  // The dynamic catalogue (#440) — see `shouldReadToolCatalogue` for the gate.
  const dynamicTools = toolkitTools.useToolkitTools({
    projectId,
    toolkitId: selectedToolkit?.id,
    toolkitType: selectedToolkit?.type,
    enabled: shouldReadToolCatalogue(selectedToolkit),
  });
  const dynamicToolNames = dynamicTools.toolNames;

  const mcpArgsSchemas = useMemo(() => {
    const acc: Record<string, unknown> = {};
    for (const tool of selectedToolkit?.settings?.available_mcp_tools ?? []) {
      if (tool.args_schema && tool.value) acc[tool.value] = tool.args_schema;
    }
    return acc;
  }, [selectedToolkit?.settings?.available_mcp_tools]);

  // Only an MCP toolkit's schema is ever actually resolved here (`mcpArgsSchemas`,
  // above) — for anything else the "dynamic schema fetch" gap (file header, item 5)
  // means no real schema was found. `Boolean(selectedToolkit)` used to be OR'd in
  // here, which made this always `true` and made `getDefaultInputMappingOfTool`'s
  // "preserve the existing saved input_mapping while unresolved" branch
  // (`flowEditorInputMapping.helpers.ts`'s own `!isSchemaResolved` check)
  // unreachable — silently collapsing an already-saved input_mapping to `{}` for
  // any non-MCP toolkit needing that missing fetch.
  const isSchemaResolved = isMcpToolkitLike(selectedToolkit);

  const [requiredInputs, setRequiredInputs] = useState<readonly string[]>([]);
  const [inputMappings, setInputMappings] = useState<Record<string, unknown>>({});
  const [defaultValues, setDefaultValues] = useState<Record<string, unknown>>({});
  const [mappingInfo, setMappingInfo] = useState<Record<string, unknown>>({});

  useEffect(() => {
    const argsSchemas = isMcpToolkitLike(selectedToolkit) ? mcpArgsSchemas : dynamicArgsSchemas;
    const { mapping, defaultValues: initialValues, mappingInfo: initialMappingInfo } = FlowEditorHelpers.getDefaultInputMappingOfTool(
      toolkitTypes,
      selectedTool,
      yamlNode?.input_mapping,
      selectedToolkit as never,
      argsSchemas as never,
      isSchemaResolved,
    );

    setInputMappings(mapping);
    setDefaultValues(initialValues);
    setMappingInfo(initialMappingInfo ?? {});
    const existingInputMapping = Object.keys(yamlNode?.input_mapping ?? {});

    if (shouldWriteDefaultMapping({ existingInputMapping, requiredInputs, selectedTool, initialTool, initialToolkit, toolkit })) {
      const filteredMapping = filterRequiredOrNonEmpty(mapping, requiredInputs);
      FlowEditorHelpers.updateYamlNode(id, 'input_mapping', filteredMapping, yamlJsonObject, setYamlJsonObject);
    }
    // baseline's own deps array (useFunctionInputMapping.hooks.js:158-166).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [dynamicArgsSchemas, mcpArgsSchemas, toolkitTypes, selectedTool, selectedToolkit, requiredInputs, isSchemaResolved]);

  useEffect(() => {
    const { required } = FlowEditorHelpers.getRequiredInputsAndTooltips(toolkitTypes, selectedTool, selectedToolkit as never, dynamicArgsSchemas as never);
    setRequiredInputs(required);
  }, [dynamicArgsSchemas, selectedTool, selectedToolkit, toolkitTypes]);

  const onChangeTool = useCallback(
    (newValue: string | undefined) => {
      if (newValue) {
        const argsSchemas = isMcpToolkitLike(selectedToolkit) ? mcpArgsSchemas : dynamicArgsSchemas;
        // Re-derived from `yamlJsonObject` (already a dependency below) rather than the
        // memoized `yamlNode`, to keep this callback's own dependency array under §3.5's budget.
        const currentInputMapping = yamlJsonObject.nodes?.find(node => node.id === id)?.input_mapping;
        const { mapping } = FlowEditorHelpers.getDefaultInputMappingOfTool(
          toolkitTypes,
          newValue,
          currentInputMapping,
          selectedToolkit as never,
          argsSchemas as never,
          isSchemaResolved,
        );
        FlowEditorHelpers.batchUpdateYamlNode(id, { tool: newValue, input_mapping: { ...mapping } }, yamlJsonObject, setYamlJsonObject);
      } else {
        FlowEditorHelpers.batchUpdateYamlNode(id, { tool: undefined, input_mapping: undefined }, yamlJsonObject, setYamlJsonObject);
      }
    },
    [dynamicArgsSchemas, mcpArgsSchemas, id, selectedToolkit, setYamlJsonObject, toolkitTypes, yamlJsonObject, isSchemaResolved],
  );

  const onChangeMapping = useCallback(
    (variable: string, value: { type?: string; value: unknown }, dataType?: string) => {
      const isOptional = !requiredInputs.includes(variable);
      const isEmpty = value.value === '' || value.value === undefined;
      setMappingInfo(prev => ({
        ...prev,
        [variable]: { ...(prev[variable] as object | undefined), type: value.type ?? (prev[variable] as { type?: string } | undefined)?.type ?? 'fixed', value: value.value },
      }));

      if (isOptional && isEmpty) {
        const node = yamlJsonObject.nodes?.find(n => n.id === id);
        const nodeMapping = node?.input_mapping;
        if (nodeMapping && variable in nodeMapping) {
          const updatedMapping = { ...nodeMapping };
          delete updatedMapping[variable];
          FlowEditorHelpers.updateYamlNode(id, 'input_mapping', updatedMapping, yamlJsonObject, setYamlJsonObject);
          return;
        }
      }

      FlowEditorHelpers.updateYamlNodeInputMappingVariable(id, variable, value as YamlInputMappingEntry, yamlJsonObject, setYamlJsonObject, dataType);
    },
    [id, requiredInputs, setYamlJsonObject, yamlJsonObject],
  );

  return {
    toolkitTypes,
    onChangeTool,
    onChangeMapping,
    requiredInputs,
    mappingInfo,
    inputMappings,
    defaultValues,
    selectedToolkit,
    dynamicToolNames,
    dynamicToolsReadFailed: dynamicTools.isError,
    retryDynamicToolsRead: dynamicTools.refetch,
    dynamicArgsSchemas,
    selectedTool,
    toolkit,
    isSchemaResolved,
  };
}
