import { useMemo } from 'react';

import { useGetCurrentToolkitSchemas } from '../../lib/hooks/useGetCurrentToolkitSchemas.hooks';

import type { JsonSchemaLike } from '../../indexes/lib/helpers/indexChat.helpers';

/**
 * Ported from `apps/elitea-ui/src/hooks/toolkit/useGetSelectedToolSchema.js`
 * (44 lines, NOT under `[fsd]/`, not one of the 4 confirmed-not-promoted
 * hooks, not owned by any other A4 sub-unit — `TestTools.jsx` is its only
 * baseline call site, so this sub-unit (A4f) is its rightful owner). Kept
 * local to `ui/test-tools/` (not `lib/hooks/`) for the same reason
 * `features/toolkits/api/useIsMcpVisible.ts` gives for staying feature-local
 * rather than being promoted: single consumer.
 *
 * **CORRECTION (#440).** This comment used to say that no
 * `toolkit_available_tools` endpoint existed. That was wrong.
 * `services/elitea-main/internal/api/router.go:1912` registers
 * `GET /elitea_core/toolkit_available_tools/prompt_lib/{projectID}/{toolkitID}`,
 * and `entities/toolkit`'s `toolkitTools.useToolkitTools` now reads it. Only
 * the OpenAPI spec, and therefore the generated client, lacked it.
 *
 * The baseline's third schema-resolution tier still does not run here, for a
 * different and narrower reason: the route carries NO argument schema. Its
 * repository query selects `id, name, type, description` only
 * (`internal/api/v2/toolkits/handler.go:1086-1100`), and the `Tool` struct
 * it marshals has no schema field. So the route answers "which tools exist",
 * which is what `TestToolSettings.tsx` reads it for, and it cannot answer
 * "what arguments does this tool take". `toolSchema` therefore resolves from
 * the static and pre-loaded-MCP tiers only. The static tier is real data
 * from the server: `ListTypeSchemas` merges the SDK argument schemas into
 * `properties.selected_tools.args_schemas` of each type
 * (`internal/api/v2/toolkits/handler.go`'s `toolkitTypeCatalogue`).
 *
 * A FAILED READ IS NOT AN EMPTY FORM (#440). Because the static tier IS that
 * server read, a lost read used to resolve to `null` and draw an argument
 * form with no fields — the same screen a tool that takes no arguments
 * draws. This hook returns `isError` and `refetch` beside the schema now, so
 * the caller can show the failure and offer a retry. It returns a RESULT
 * OBJECT rather than the bare schema for that reason; every caller reads
 * `.toolSchema` where it used to read the return value.
 *
 * `toolkitId`/`projectId` stay off this hook's own signature (the baseline
 * threads both through purely to drive that dynamic query) rather than being
 * kept as accepted-but-unused parameters — an honest narrowing of the port's
 * surface, not a silent behaviour change of what remains.
 */
export interface McpToolOption {
  readonly value?: string | undefined;
  readonly args_schema?: JsonSchemaLike | undefined;
}

export interface UseGetSelectedToolSchemaParams {
  readonly toolkitType: string | undefined;
  readonly toolOptionType: string | null;
  readonly availableMcpTools: readonly McpToolOption[] | undefined;
}

interface SelectedToolsSchemaEntry {
  readonly args_schemas?: Readonly<Record<string, JsonSchemaLike>> | undefined;
}

interface ToolInputSchemaShape {
  readonly inputSchema?: JsonSchemaLike | undefined;
  readonly title?: string | undefined;
  readonly name?: string | undefined;
  readonly description?: string | undefined;
}

function normalizeMcpInputSchema(toolSchema: JsonSchemaLike): JsonSchemaLike {
  const shaped = toolSchema as ToolInputSchemaShape;
  const inputSchema = shaped.inputSchema;
  if (!inputSchema) return toolSchema;

  return {
    properties: inputSchema.properties ?? {},
    required: inputSchema.required ?? [],
    title: shaped.title ?? shaped.name,
    description: shaped.description,
    type: 'object',
  };
}

/** The static-schema-lookup tier, split out of `useGetSelectedToolSchema`'s memo body to stay under the §3.5 complexity budget. */
function resolveStaticToolSchema(toolkitTypeSchema: unknown, toolOptionType: string): JsonSchemaLike | undefined {
  const selectedToolsEntry = (toolkitTypeSchema as { properties?: { selected_tools?: SelectedToolsSchemaEntry } } | undefined)?.properties?.selected_tools;
  return selectedToolsEntry?.args_schemas?.[toolOptionType];
}

/** The whole tool-schema resolution (static tier, then pre-loaded-MCP tier), split out of the hook body to stay under the §3.5 complexity budget. */
function resolveToolSchema(params: UseGetSelectedToolSchemaParams & { readonly toolkitTypeSchema: unknown }): JsonSchemaLike | null {
  const { toolOptionType, availableMcpTools, toolkitTypeSchema } = params;
  if (!toolOptionType || toolkitTypeSchema === undefined) return null;

  const staticToolSchema = resolveStaticToolSchema(toolkitTypeSchema, toolOptionType);
  // MCP tools have schemas pre-loaded in settings (baseline: `availableMcpTools`).
  const mcpToolSchema = availableMcpTools?.find((it) => it.value === toolOptionType)?.args_schema;

  const toolSchema = staticToolSchema ?? mcpToolSchema ?? null;
  return toolSchema ? normalizeMcpInputSchema(toolSchema) : null;
}

export interface UseGetSelectedToolSchemaResult {
  /** The resolved argument schema, or `null` when the tool takes no arguments. */
  readonly toolSchema: JsonSchemaLike | null;
  /** The schema read failed (#440). Show it. A `null` schema beside it means nothing. */
  readonly isError: boolean;
  /** Reads the schemas again. Connect it to the retry control of the error state. */
  readonly refetch: () => void;
}

export function useGetSelectedToolSchema(params: UseGetSelectedToolSchemaParams): UseGetSelectedToolSchemaResult {
  const { toolkitType, toolOptionType, availableMcpTools } = params;
  const { toolkitSchemas, isError, refetch } = useGetCurrentToolkitSchemas();
  const toolkitTypeSchema = toolkitType !== undefined ? toolkitSchemas?.[toolkitType] : undefined;

  const toolSchema = useMemo(
    () => resolveToolSchema({ toolkitType, toolOptionType, availableMcpTools, toolkitTypeSchema }),
    [toolkitType, toolOptionType, availableMcpTools, toolkitTypeSchema],
  );

  return { toolSchema, isError, refetch };
}
