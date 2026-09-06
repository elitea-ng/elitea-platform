import type { Dispatch, SetStateAction } from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import type { ToolkitTypeSchemaMap } from '@/entities/toolkit';
import { toolkitTools } from '@/entities/toolkit';
import { ToolkitViewOptions } from '@/shared/lib/enums';

import { getToolComponent } from '../../../lib/helpers/toolComponent.helpers';
import type { ToolFormComponent } from '../../../lib/helpers/toolComponent.helpers';
import { convertToolkitSchema } from '../../../lib/helpers/toolkitSchema.helpers';
import type { RawToolkitTypeSchema } from '../../../lib/helpers/toolkitSchema.helpers';
import { useGetCurrentToolkitSchemas } from '../../../lib/hooks/useGetCurrentToolkitSchemas.hooks';
import { useToolkitNameProp } from '../../../lib/hooks/useToolkitNameProp.hooks';

import { applyAutoSelectFormReset, resolveOutOfBandFieldSync, updateDetailByPath } from './ToolkitForm.helpers';
import type { ResolvedToolkitFormProps, ToolkitConfigurationState } from './ToolkitForm.types';

/**
 * State declarations, schema resolution, and `editField` — the first half
 * of `useToolkitFormState`'s baseline logic (`ToolkitForm.jsx`, roughly
 * lines 1-350). Split out of `ToolkitForm.hooks.ts` (which composes this
 * with `ToolkitForm.configuration.hooks.ts`'s `useToolkitFormConfiguration`)
 * purely to stay under the §3.5 400-line-per-file / complexity-12 budgets.
 */
/**
 * `options.section` carries the schema section of the field being edited, and
 * exists so `applyAutoSelectFormReset` can carve `credentials` out of its
 * dirty-suppression (EL-6180). A caller that renders a section-bearing field
 * passes it through; the baseline threads it the same way, at
 * `ToolBaseProperty.jsx:501` (`{ ...options, section }`).
 */
export type EditFieldFn = (
  field: string,
  value: unknown,
  replace?: boolean,
  options?: { readonly isAutoSelect?: boolean; readonly section?: string },
) => Promise<void>;

/**
 * The tool names a toolkit type declares in its own settings schema — the
 * exact pair `ToolBase.render.tsx`'s `resolveAvailableTools` reads to draw
 * the "Tools" chip picker. An empty result means the type declares no tools
 * and publishes them at run time instead (openapi, mcp, mcp_config).
 */
function readStaticToolNames(schema: RawToolkitTypeSchema | undefined): readonly string[] {
  const selectedTools = (schema?.properties as Record<string, SelectedToolsSchemaShape> | undefined)?.['selected_tools'];
  const argsSchemaNames = Object.keys(selectedTools?.args_schemas ?? {});
  if (argsSchemaNames.length > 0) return argsSchemaNames;
  return selectedTools?.items?.enum ?? [];
}

interface SelectedToolsSchemaShape {
  readonly args_schemas?: Readonly<Record<string, unknown>> | undefined;
  readonly items?: { readonly enum?: readonly string[] | undefined } | undefined;
}

/**
 * The baseline's `toolSchemaWithDynamicTools` (#440), names half.
 *
 * A toolkit type that declares no tools of its own used to leave the "Tools"
 * chip picker empty for ever, because nothing read the catalogue the backend
 * publishes for it. The names go into `selected_tools.items.enum`, which is
 * where `resolveAvailableTools` already looks. Only the two nodes on that
 * path are rebuilt; every sibling property is carried over untouched.
 *
 * The ARGUMENT schemas stay out of this: neither tool route carries one (see
 * `ui/test-tools/useGetSelectedToolSchema.ts`'s header), so there is nothing
 * to enrich `args_schemas` with.
 */
function withDynamicToolNames(schema: RawToolkitTypeSchema | undefined, dynamicToolNames: readonly string[]): RawToolkitTypeSchema | undefined {
  if (schema === undefined || dynamicToolNames.length === 0) return schema;
  const properties: Record<string, unknown> = schema.properties ?? {};
  const selectedTools = (properties['selected_tools'] as Record<string, unknown> | undefined) ?? { type: 'array' };
  const items = (selectedTools['items'] as Record<string, unknown> | undefined) ?? {};
  return {
    ...schema,
    properties: { ...properties, selected_tools: { ...selectedTools, items: { ...items, enum: [...dynamicToolNames] } } },
  };
}

interface ToolCatalogueRead {
  readonly toolNames: readonly string[];
  /** The read failed AND left the "Tools" section with nothing to show. A failed read that still produced a list keeps the working picker. */
  readonly readFailed: boolean;
  readonly retry: () => void;
}

interface ToolCatalogueReadArgs {
  readonly projectId: string | undefined;
  readonly toolkitId: string | number | undefined;
  readonly toolkitType: string;
  readonly staticToolNames: readonly string[];
  readonly schemasAreFetching: boolean;
  readonly schemasReadFailed: boolean;
  readonly retrySchemasRead: () => void;
}

/**
 * The catalogue tier of the "Tools" section (#440). It runs only for a
 * toolkit type that declares no tools of its own, which is the type that
 * publishes them at run time. Split out of `useToolkitFormCore` to keep that
 * function under the §3.5 complexity budget.
 */
function useToolCatalogueRead(args: ToolCatalogueReadArgs): ToolCatalogueRead {
  const { projectId, toolkitId, toolkitType, staticToolNames, schemasAreFetching, schemasReadFailed, retrySchemasRead } = args;

  const dynamicTools = toolkitTools.useToolkitTools({
    projectId,
    // `ToolkitFormEditDetail.id` is `string | number`; the route takes a path segment.
    toolkitId: toolkitId !== undefined ? String(toolkitId) : undefined,
    toolkitType,
    enabled: !schemasAreFetching && staticToolNames.length === 0,
  });

  // Both reads feed the "Tools" section, so one retry control must run both.
  const { refetch: retryDynamicToolsRead } = dynamicTools;
  const retry = useCallback(() => {
    retryDynamicToolsRead();
    retrySchemasRead();
  }, [retryDynamicToolsRead, retrySchemasRead]);

  const nothingToShow = staticToolNames.length === 0 && dynamicTools.toolNames.length === 0;
  return { toolNames: dynamicTools.toolNames, readFailed: (dynamicTools.isError || schemasReadFailed) && nothingToShow, retry };
}

export interface CoreState {
  readonly view: string;
  readonly setView: Dispatch<SetStateAction<string>>;
  readonly onManualViewChange: (view: string) => void;
  readonly showValidation: boolean;
  readonly setShowValidation: Dispatch<SetStateAction<boolean>>;
  readonly toolErrors: Record<string, boolean>;
  readonly setToolErrors: Dispatch<SetStateAction<Record<string, boolean>>>;
  readonly serverToolErrors: Record<string, string | undefined>;
  readonly setServerToolErrors: Dispatch<SetStateAction<Record<string, string | undefined>>>;
  readonly configuration: ToolkitConfigurationState;
  readonly setConfiguration: Dispatch<SetStateAction<ToolkitConfigurationState>>;
  readonly configurationErrors: Record<string, boolean>;
  readonly setConfigurationErrors: Dispatch<SetStateAction<Record<string, boolean>>>;
  readonly configurationName: string;
  readonly setConfigurationName: Dispatch<SetStateAction<string>>;
  readonly toolkitSchemas: ToolkitTypeSchemaMap | undefined;
  readonly isFetching: boolean;
  readonly toolType: string;
  readonly effectiveToolSchema: RawToolkitTypeSchema | undefined;
  /** A read that feeds the "Tools" section failed — the type schemas, or the tool catalogue (#440). */
  readonly toolListReadFailed: boolean;
  /** Runs both reads again. */
  readonly retryToolListRead: () => void;
  readonly ToolComponent: ToolFormComponent | undefined;
  readonly isValidSchema: boolean;
  readonly nameIsRequired: boolean;
  readonly hasErrors: boolean;
  readonly mergedToolErrors: Record<string, boolean>;
  readonly editField: EditFieldFn;
}

export function useToolkitFormCore(props: ResolvedToolkitFormProps): CoreState {
  const { editToolDetail, onChangeToolDetail, isMCP, onValidationStateChange, formValues, onSetFormField, onMcpScopesChanged, forceCustomView, onResetForm, projectId } = props;

  const hasSetViewManually = useRef(false);
  const [view, setView] = useState<string>(ToolkitViewOptions.Form);
  const [showValidation, setShowValidation] = useState(false);
  const [toolErrors, setToolErrors] = useState<Record<string, boolean>>({});
  const [serverToolErrors, setServerToolErrors] = useState<Record<string, string | undefined>>({});
  const [configurationErrors, setConfigurationErrors] = useState<Record<string, boolean>>({});
  const [configurationName, setConfigurationName] = useState('');
  const [configuration, setConfiguration] = useState<ToolkitConfigurationState>({
    elitea_title: (editToolDetail.settings?.elitea_title as string | undefined) ?? '',
    private: editToolDetail.settings?.private as boolean | undefined,
  });

  const isMcpType = editToolDetail.type === 'mcp';
  // `isError` has a reader now (#440): a lost schema read leaves the "Tools"
  // section with nothing to offer, which is what a toolkit with no tools
  // looks like.
  const { toolkitSchemas, isFetching, isError: schemasReadFailed, refetch: retrySchemasRead } = useGetCurrentToolkitSchemas({ isMCP: Boolean(isMCP) && !isMcpType });

  const toolType = editToolDetail.type ?? '';

  const toolSchema = useMemo<RawToolkitTypeSchema | undefined>(
    // `convertToolkitSchema`'s return type (`ConvertedToolkitSchema` — `properties` required, not optional)
    // and this file's own `RawToolkitTypeSchema` (`properties` optional) describe the same
    // JSON-Schema-shaped object from two different, independently-typed modules; the cast
    // documents that, rather than a real behavioural difference.
    () => (editToolDetail.schema ?? convertToolkitSchema(toolkitSchemas?.[toolType])) as RawToolkitTypeSchema,
    [editToolDetail.schema, toolkitSchemas, toolType],
  );
  const staticToolNames = useMemo(() => readStaticToolNames(toolSchema), [toolSchema]);
  const toolCatalogue = useToolCatalogueRead({
    projectId,
    toolkitId: editToolDetail.id,
    toolkitType: toolType,
    staticToolNames,
    schemasAreFetching: isFetching,
    schemasReadFailed,
    retrySchemasRead,
  });

  const effectiveToolSchema = useMemo(() => withDynamicToolNames(toolSchema, toolCatalogue.toolNames), [toolSchema, toolCatalogue.toolNames]);

  const ToolComponent = useMemo(() => {
    const useJsonView = forceCustomView || view === ToolkitViewOptions.Json;
    if (useJsonView) return undefined;
    // `RawToolkitTypeSchema`'s only NAMED properties are `properties`/`required`/`$defs`
    // (everything else lives on its index signature) — none named `type`, so
    // it fails TS's "weak type" common-property check against
    // `getToolComponent`'s all-optional `ToolComponentSchema` (`{type?:
    // unknown}`) even though the index signature covers a real `.type` key
    // at runtime. An explicit two-step cast documents that mismatch instead
    // of silently loosening `getToolComponent`'s own parameter type.
    return getToolComponent(toolType, effectiveToolSchema as unknown as { readonly type?: unknown } | undefined);
  }, [effectiveToolSchema, forceCustomView, view, toolType]);

  const { nameIsRequired } = useToolkitNameProp(toolType, toolkitSchemas);
  const nameIsBlank = !editToolDetail.name?.trim();
  const computedNameError = nameIsRequired && nameIsBlank;
  const mergedToolErrors = useMemo(() => ({ ...toolErrors, ...serverToolErrors, name: computedNameError }), [toolErrors, serverToolErrors, computedNameError]);
  const hasErrors = useMemo(() => Object.values(mergedToolErrors).some(Boolean), [mergedToolErrors]);
  const triggerValidation = useCallback(() => setShowValidation(true), []);

  useEffect(() => {
    onValidationStateChange?.({ hasErrors, triggerValidation });
  }, [hasErrors, triggerValidation, onValidationStateChange]);

  const editField: EditFieldFn = useCallback(
    async (field, value, replace, options) => {
      const isNameOrDescription = field === 'name' || field === 'description';
      if (isNameOrDescription || toolType === 'custom') {
        await onSetFormField?.(field, value);
      }
      if (isMcpType && field === 'settings.scopes') {
        const settings = formValues.settings as { readonly url?: string } | undefined;
        onMcpScopesChanged?.(settings?.url);
      }
      const fieldKey = field.includes('.') ? (field.split('.').pop() ?? field) : field;
      setToolErrors((prev) => (fieldKey in prev ? Object.fromEntries(Object.entries(prev).filter(([key]) => key !== fieldKey)) : prev));
      setServerToolErrors((prev) => (fieldKey in prev ? Object.fromEntries(Object.entries(prev).filter(([key]) => key !== fieldKey)) : prev));
      onChangeToolDetail((prevState) => updateDetailByPath(prevState ?? {}, field, value, replace), options);
      applyAutoSelectFormReset(options, formValues, field, value, onResetForm);
    },
    [onChangeToolDetail, onSetFormField, toolType, isMcpType, onMcpScopesChanged, formValues, onResetForm],
  );

  const isValidSchema = useMemo(() => Object.keys(effectiveToolSchema ?? {}).length > 0, [effectiveToolSchema]);

  useEffect(() => {
    if (!isValidSchema) {
      setView((prev) => (prev !== ToolkitViewOptions.Json ? ToolkitViewOptions.Json : prev));
    } else if (!hasSetViewManually.current) {
      setView(ToolkitViewOptions.Form);
    }
  }, [isValidSchema, toolType]);

  useEffect(() => {
    for (const sync of resolveOutOfBandFieldSync(editToolDetail, formValues)) {
      void onSetFormField?.(sync.field, sync.value);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- mirrors the baseline's own dep array (`[editToolDetail]` only)
  }, [editToolDetail]);

  const onManualViewChange = useCallback((newView: string) => {
    setView(newView);
    hasSetViewManually.current = true;
  }, []);

  return {
    view,
    setView,
    onManualViewChange,
    showValidation,
    setShowValidation,
    toolErrors,
    setToolErrors,
    serverToolErrors,
    setServerToolErrors,
    configuration,
    setConfiguration,
    configurationErrors,
    setConfigurationErrors,
    configurationName,
    setConfigurationName,
    toolkitSchemas,
    isFetching,
    toolType,
    effectiveToolSchema,
    toolListReadFailed: toolCatalogue.readFailed,
    retryToolListRead: toolCatalogue.retry,
    ToolComponent,
    isValidSchema,
    nameIsRequired,
    hasErrors,
    mergedToolErrors,
    editField,
  };
}
