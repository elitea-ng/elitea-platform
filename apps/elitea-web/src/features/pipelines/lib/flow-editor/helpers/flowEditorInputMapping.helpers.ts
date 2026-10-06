/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/lib/
 * helpers/flowEditor.helpers.js` (unit A2c), the input-mapping-default
 * computation half of that file. Split into its own module purely to keep
 * `flowEditor.helpers.ts` under the §3.5 400-line budget (the baseline file
 * alone is 512 lines) — `flowEditor.helpers.ts` re-exports everything here,
 * so callers keep the baseline's single `flowEditor.helpers` import surface.
 *
 * `ToolTypes` is `entities/toolkit`'s promoted export (Wave-2 promotion
 * pass), NOT a local re-port of `pages/Applications/Components/Tools/
 * consts.js` — see this unit's mission preamble: importing the entities
 * copy is the mandated path (`no-sideways-features` forbids reaching into
 * `features/toolkits` directly, and re-declaring the catalogue locally would
 * fork it from the one `entities/toolkit` already ports faithfully).
 *
 * `getDefaultInputMappingOfTool`/`getRequiredInputsAndTooltips` share a
 * "resolve the JSON-schema for the selected tool" step the baseline repeats
 * inline in both functions (`flowEditor.helpers.js:254-270,347-365`) —
 * factored here into `resolveDirectToolSchema`/`isMcpToolkit`/
 * `findMcpToolSchema` (this app's `complexity` budget forces the
 * extraction; behaviour is unchanged, verified by this file's own tests
 * against both call sites).
 */
import { ToolTypes } from '@/entities/toolkit';

import type { YamlInputMappingEntry } from './pipelineFlow.types';

interface ToolkitLike {
  readonly type?: string;
  readonly meta?: { readonly mcp?: boolean };
  readonly settings?: {
    readonly variables?: readonly { readonly name: string; readonly value?: unknown }[];
    readonly available_mcp_tools?: readonly {
      readonly value?: string;
      readonly label?: string;
      readonly args_schema?: JsonSchemaLike;
    }[];
  };
  readonly variables?: readonly { readonly name: string; readonly value?: unknown }[];
}

interface JsonSchemaProperty {
  readonly type?: string;
  readonly enum?: readonly unknown[];
  readonly items?: { readonly enum?: readonly unknown[] };
  readonly anyOf?: readonly { readonly type?: string; readonly enum?: readonly unknown[] }[];
  readonly default?: unknown;
  readonly description?: string;
  readonly multiline?: boolean;
}

interface JsonSchemaLike {
  readonly properties?: Readonly<Record<string, JsonSchemaProperty>>;
  readonly inputSchema?: {
    readonly properties?: Readonly<Record<string, JsonSchemaProperty>>;
    readonly required?: readonly string[];
  };
  readonly required?: readonly string[];
}

interface ToolkitTypeSchemas {
  readonly [toolkitType: string]: {
    readonly properties?: {
      readonly selected_tools?: {
        readonly args_schemas?: Readonly<Record<string, JsonSchemaLike>>;
      };
    };
  };
}

/** Creates default mapping for an application (agent-as-tool) toolkit. */
const createApplicationMapping = (
  existingMapping: Readonly<Record<string, YamlInputMappingEntry>> | undefined,
  selectedToolkit: ToolkitLike | undefined,
) => {
  return {
    mapping: {
      task: { ...(existingMapping?.['task'] ?? { type: 'fstring', value: '' }) },
      ...selectedToolkit?.variables?.reduce<Record<string, unknown>>((result, variable) => {
        return {
          [variable.name]: { ...(existingMapping?.[variable.name] ?? { type: 'fixed', value: variable.value }) },
          ...result,
        };
      }, {}),
    },
    mappingInfo: {
      task: { tooltip: 'Task for agent.', type: 'fstring', value: '', data_type: 'string' },
      ...selectedToolkit?.variables?.reduce<Record<string, unknown>>((result, variable) => {
        return {
          [variable.name]: {
            tooltip: 'This is a variable from the agent',
            ...(existingMapping?.[variable.name] ?? { type: 'fixed', value: variable.value }),
          },
          ...result,
        };
      }, {}),
    },
    defaultValues: { task: '' as unknown },
  };
};

export const getInputMappingDefaultValue = (
  enumList: readonly unknown[] | undefined,
  dataType: string | undefined,
  defaultValues: Readonly<Record<string, unknown>>,
  key: string,
): unknown => {
  if (enumList && enumList.length > 0) {
    return dataType !== 'array' ? enumList[0] : [];
  }
  return defaultValues[key] !== undefined ? defaultValues[key] : '';
};

export const getEnumList = (
  type: string | undefined,
  schemaEnum: readonly unknown[] | undefined,
  inputOptions: readonly { readonly value: unknown }[],
): readonly unknown[] | undefined => {
  switch (type) {
    case 'fixed':
      return schemaEnum;
    case 'variable':
      return inputOptions.map(item => item.value);
    case undefined:
    default:
      return [];
  }
};

/** JSON-schema `type` -> zero value, for every type that isn't `''` (the catch-all for string/integer/number/undefined/anything else). */
const ZERO_VALUE_BY_SCHEMA_TYPE: Readonly<Record<string, unknown>> = {
  boolean: false,
  array: [] as const,
  object: {} as const,
};

/** Gets appropriate default value based on property type. */
const getDefaultValueForType = (property: JsonSchemaProperty): unknown => {
  const { type, enum: enumProp, items } = property;
  const enumValues = enumProp ?? items?.enum;

  // If enum values exist, use the first one as default
  if (enumValues && enumValues.length > 0 && type !== 'array') {
    return enumValues[0];
  }

  return type !== undefined && type in ZERO_VALUE_BY_SCHEMA_TYPE ? ZERO_VALUE_BY_SCHEMA_TYPE[type] : '';
};

const getDataTypeOfMapping = (value: JsonSchemaProperty): string => {
  if (value.type) {
    return value.type;
  }
  const foundType = value.anyOf?.find(v => v.type && v.type !== 'null')?.type;
  return foundType ?? 'string';
};

const getEnumOfMapping = (value: JsonSchemaProperty): readonly unknown[] | undefined => {
  if (value.enum) {
    return value.enum;
  }
  if (value.items?.enum) {
    return value.items.enum;
  }
  const foundEnum = value.anyOf?.find(v => v.enum)?.enum;
  return foundEnum && foundEnum.length > 0 ? foundEnum : undefined;
};

const getMappingValue = (
  foundMapping: YamlInputMappingEntry | undefined,
  value: JsonSchemaProperty,
  defaultValueForType: unknown,
): unknown => {
  if (foundMapping) {
    return foundMapping.value;
  }
  return value.default !== undefined ? value.default : defaultValueForType;
};

/** Extracts a Remote-MCP tool's `args_schema` from a toolkit's `available_mcp_tools` list, by value or label. */
const findMcpToolSchema = (selectedToolkit: ToolkitLike | undefined, selectedTool: string | undefined) =>
  selectedToolkit?.settings?.available_mcp_tools?.find(
    tool => tool.value === selectedTool || tool.label === selectedTool,
  )?.args_schema;

const isMcpToolkit = (selectedToolkit: ToolkitLike | undefined): boolean =>
  Boolean(selectedToolkit?.meta?.mcp || selectedToolkit?.type === 'mcp');

/** The direct (non-MCP-fallback) schema lookup shared by both public functions below. */
const resolveDirectToolSchema = (
  toolkitTypeSchemas: ToolkitTypeSchemas | undefined,
  selectedTool: string | undefined,
  selectedToolkit: ToolkitLike | undefined,
  dynamicArgsSchemas: Readonly<Record<string, JsonSchemaLike>>,
): JsonSchemaLike | undefined => {
  if (selectedTool && dynamicArgsSchemas[selectedTool]) {
    return dynamicArgsSchemas[selectedTool];
  }
  if (!selectedToolkit?.type) return undefined;
  return toolkitTypeSchemas?.[selectedToolkit.type]?.properties?.selected_tools?.args_schemas?.[selectedTool ?? ''];
};

interface MappingEntryResult {
  readonly mapping: unknown;
  readonly info: unknown;
}

const buildMappingEntry = (
  value: JsonSchemaProperty,
  foundMapping: YamlInputMappingEntry | undefined,
): MappingEntryResult => {
  const defaultValueForType = getDefaultValueForType(value);
  const enumValues = getEnumOfMapping(value);
  const info = {
    enum: enumValues,
    data_type: getDataTypeOfMapping(value),
    tooltip: value.description ?? '',
    type: 'fixed',
    value: getMappingValue(foundMapping, value, defaultValueForType),
    multiline: value.multiline === true,
  };
  const mapping = foundMapping
    ? { ...foundMapping, enum: enumValues }
    : { type: 'fixed', value: value.default !== undefined ? value.default : defaultValueForType, enum: enumValues };
  return { mapping, info };
};

const buildMappingFromProperties = (
  properties: Readonly<Record<string, JsonSchemaProperty>>,
  existingMapping: Readonly<Record<string, YamlInputMappingEntry>> | undefined,
): { mapping: Record<string, unknown>; defaultValues: Record<string, unknown>; mappingInfo: Record<string, unknown> } => {
  const mappingInfo: Record<string, unknown> = {};
  const mapping = Object.entries(properties).reduce<Record<string, unknown>>((result, [key, value]) => {
    const entry = buildMappingEntry(value, existingMapping ? existingMapping[key] : undefined);
    mappingInfo[key] = entry.info;
    return { [key]: entry.mapping, ...result };
  }, {});
  const defaultValues = Object.entries(properties).reduce<Record<string, unknown>>((result, [key, value]) => {
    const defaultValueForType = getDefaultValueForType(value);
    return { [key]: value.default !== undefined ? value.default : defaultValueForType, ...result };
  }, {});
  return { mapping, defaultValues, mappingInfo };
};

/** `resolveDirectToolSchema`, then the MCP `available_mcp_tools` fallback if nothing was found directly. */
const resolveSchemaForTool = (
  toolkitTypeSchemas: ToolkitTypeSchemas | undefined,
  selectedTool: string | undefined,
  selectedToolkit: ToolkitLike | undefined,
  dynamicArgsSchemas: Readonly<Record<string, JsonSchemaLike>>,
): JsonSchemaLike | undefined => {
  const direct = resolveDirectToolSchema(toolkitTypeSchemas, selectedTool, selectedToolkit, dynamicArgsSchemas);
  if (direct) return direct;
  if (isMcpToolkit(selectedToolkit) && selectedToolkit?.settings?.available_mcp_tools) {
    return findMcpToolSchema(selectedToolkit, selectedTool);
  }
  return undefined;
};

/** For MCP tools, checks both `properties` and `inputSchema.properties`; for everything else, just `properties`. */
const resolvePropertiesForMapping = (
  schemaForTool: JsonSchemaLike | undefined,
  selectedToolkit: ToolkitLike | undefined,
): Readonly<Record<string, JsonSchemaProperty>> => {
  if (isMcpToolkit(selectedToolkit)) {
    return schemaForTool?.properties ?? schemaForTool?.inputSchema?.properties ?? {};
  }
  return schemaForTool?.properties ?? {};
};

export const getDefaultInputMappingOfTool = (
  toolkitSchemas: ToolkitTypeSchemas | undefined,
  selectedTool: string | undefined,
  existingMapping: Readonly<Record<string, YamlInputMappingEntry>> | undefined,
  selectedToolkit: ToolkitLike | undefined,
  dynamicArgsSchemas: Readonly<Record<string, JsonSchemaLike>> = {},
  isSchemaResolved = false,
): {
  mapping: Record<string, unknown>;
  defaultValues: Record<string, unknown>;
  mappingInfo?: Record<string, unknown>;
} => {
  if (selectedToolkit?.type === ToolTypes.application.value) {
    return createApplicationMapping(existingMapping, selectedToolkit);
  }

  const schemaForTool = resolveSchemaForTool(toolkitSchemas, selectedTool, selectedToolkit, dynamicArgsSchemas);
  const properties = resolvePropertiesForMapping(schemaForTool, selectedToolkit);

  if (Object.entries(properties).length === 0 && selectedTool && !isSchemaResolved) {
    return { mapping: { ...existingMapping }, defaultValues: {} };
  }

  return buildMappingFromProperties(properties, existingMapping);
};

/** Creates tooltips for an application (agent-as-tool) toolkit. */
const createApplicationTooltips = (selectedToolkit: ToolkitLike | undefined): Record<string, string> => {
  const tooltips: Record<string, string> = { task: 'Provides the main instruction or task for the agent being called.' };
  selectedToolkit?.settings?.variables?.forEach(variable => {
    tooltips[variable.name] = 'This is a variable from the agent';
  });
  return tooltips;
};

/** Whether `getRequiredInputsAndTooltips` should still try the MCP `available_mcp_tools` fallback: the directly-resolved schema found neither `properties` nor `inputSchema` at all. */
const shouldTryMcpRequiredFallback = (schemaForTool: JsonSchemaLike, selectedToolkit: ToolkitLike | undefined): boolean =>
  !schemaForTool.properties &&
  !schemaForTool.inputSchema &&
  isMcpToolkit(selectedToolkit) &&
  Boolean(selectedToolkit?.settings?.available_mcp_tools);

export const getRequiredInputsAndTooltips = (
  toolkitTypes: ToolkitTypeSchemas | undefined,
  selectedTool: string | undefined,
  selectedToolkit: ToolkitLike | undefined,
  dynamicArgsSchemas: Readonly<Record<string, JsonSchemaLike>> = {},
): { required: readonly string[]; tooltips?: Record<string, string>; enums?: Record<string, never> } => {
  if (selectedToolkit?.type === ToolTypes.application.value) {
    return { required: ['task'], tooltips: createApplicationTooltips(selectedToolkit), enums: {} };
  }

  const directSchema: JsonSchemaLike =
    resolveDirectToolSchema(toolkitTypes, selectedTool, selectedToolkit, dynamicArgsSchemas) ?? {};
  const mcpSchema = shouldTryMcpRequiredFallback(directSchema, selectedToolkit)
    ? findMcpToolSchema(selectedToolkit, selectedTool)
    : undefined;
  const schemaForTool = mcpSchema ?? directSchema;

  const { required, inputSchema } = schemaForTool;
  return { required: required ?? inputSchema?.required ?? [] };
};
