import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';

import { toolkitTools } from '@/entities/toolkit';

import { useSelectedProjectId } from '../../lib/hooks/useSelectedProjectId';

import { useGetCurrentToolkitSchemas } from '../../lib/hooks/useGetCurrentToolkitSchemas.hooks';

import type { JsonSchemaLike } from '../../indexes/lib/helpers/indexChat.helpers';

/** Resolve static, saved MCP, then instance-discovered argument schemas. */
export interface McpToolOption {
  readonly value?: string | undefined;
  readonly args_schema?: JsonSchemaLike | undefined;
}

export interface UseGetSelectedToolSchemaParams {
  readonly projectId?: string | number | undefined;
  readonly toolkitId?: string | number | undefined;
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
  /** The resolved argument schema, or `null` while no schema is available. */
  readonly toolSchema: JsonSchemaLike | null;
  /** The schema read failed (#440). Show it. A `null` schema beside it means nothing. */
  readonly isError: boolean;
  /** Reads the schemas again. Connect it to the retry control of the error state. */
  readonly refetch: () => void;
}

function canDiscoverSchema(params: UseGetSelectedToolSchemaParams, typeSchema: unknown, localSchema: JsonSchemaLike | null, projectId: string | number | undefined, isError: boolean): boolean {
  return !!params.toolOptionType && typeSchema !== undefined && !localSchema && !!projectId && !!params.toolkitId && !isError;
}

export function useGetSelectedToolSchema(params: UseGetSelectedToolSchemaParams): UseGetSelectedToolSchemaResult {
  const { toolkitType, toolOptionType, availableMcpTools, toolkitId } = params;
  const selectedProjectId = useSelectedProjectId();
  const projectId = params.projectId ?? selectedProjectId;
  const { toolkitSchemas, isError, refetch } = useGetCurrentToolkitSchemas();
  const toolkitTypeSchema = toolkitType !== undefined ? toolkitSchemas?.[toolkitType] : undefined;

  const localSchema = useMemo(
    () => resolveToolSchema({ toolkitType, toolOptionType, availableMcpTools, toolkitTypeSchema }),
    [toolkitType, toolOptionType, availableMcpTools, toolkitTypeSchema],
  );

  const dynamic = useQuery({
    queryKey: ['toolkits', 'tools', String(projectId ?? ''), `id:${toolkitId ?? ''}`],
    queryFn: ({ signal }) => toolkitTools.fetchAvailableTools({ projectId: String(projectId), toolkitId: String(toolkitId) }, signal),
    enabled: canDiscoverSchema(params, toolkitTypeSchema, localSchema, projectId, isError),
    retry: false,
  });
  const dynamicSchema = toolOptionType ? dynamic.data?.args_schemas?.[toolOptionType] : undefined;
  const toolSchema = useMemo(() => localSchema ?? (dynamicSchema ? normalizeMcpInputSchema(dynamicSchema as JsonSchemaLike) : null), [localSchema, dynamicSchema]);
  return {
    toolSchema,
    isError: isError || (!localSchema && dynamic.isError),
    refetch: () => { if (isError) refetch(); else void dynamic.refetch(); },
  };
}
