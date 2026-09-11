import type { ComponentType, ReactNode } from 'react';
import { useCallback, useMemo } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { SingleSelect, type SingleSelectOption } from '@/shared/ui/SingleSelect';
import { ToolListError } from '@/shared/ui/ToolListError';

import { toolkitTools } from '@/entities/toolkit';

import { ToolArgumentPanel } from './ToolArgumentPanel';

import { IndexesToolsEnum } from '../../indexes/lib/constants/indexDetails.constants';
import type { JsonSchemaLike } from '../../indexes/lib/helpers/indexChat.helpers';
import type { UseIndexNameValidationResult } from '../../indexes/lib/hooks/useIndexNameValidation.hooks';
import type { ToolkitConversationValues } from '../../lib/helpers/toolkitConversation.helpers';
import { useGetCurrentToolkitSchemas } from '../../lib/hooks/useGetCurrentToolkitSchemas.hooks';
import { useSelectedProjectId } from '../../lib/hooks/useSelectedProjectId';
import type { LLMModelSelectorProps } from '../../indexes/ui/IndexDetails/IndexChat';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/toolkits/ui/test-tools/
 * TestToolSettings.jsx` (215 lines, Wave-2 unit A4f) — the right-hand
 * "pick a tool, fill its arguments, Run Tool" panel `TestTools.tsx` embeds.
 *
 * DISCLOSED DEVIATIONS, each forced by a real, verified constraint:
 *
 *  1. **No ambient Formik context.** `useFormikContext().values` becomes
 *     an explicit `values: ToolkitConversationValues` prop — this app has
 *     no Formik dependency (established convention, e.g. `DeleteApplicationButton.tsx`'s
 *     own doc comment).
 *
 *  2. **The dynamic tool catalogue is READ (#440).** This item used to claim
 *     there was no `toolkit_available_tools` endpoint. That was wrong:
 *     `internal/api/router.go` registers it (:1912) and
 *     `toolkit_discover_tools` (:1914); only the OpenAPI spec, and so the
 *     generated client, lacked them. The baseline's third tier is restored
 *     through `entities/toolkit`'s `toolkitTools.useToolkitTools`, so
 *     `allToolsOptions` resolves in the baseline's own order: an explicit
 *     `values.settings.selected_tools`, else the static `selected_tools`
 *     schema, else discovery for the saved toolkit instance. A FAILED READ
 *     IS ITS OWN STATE — #381 made both routes answer a failed read with an
 *     error rather than `200 {"tools":[]}`, and `ToolPicker` below renders
 *     that as an error with a retry, so an empty picker keeps its one
 *     meaning. The editor supplies its project and toolkit identity.
 *     Unsaved toolkit forms retain type discovery as their fallback.
 *
 *  3. **`SHARED_TOUR_TARGET_IDS.testSettings` (`features/interactive-tours`)
 *     is dropped.** That domain does not exist in this worktree and is out
 *     of this Wave-2 batch's scope entirely (agents/pipelines/toolkits
 *     only) — same precedent `features/agents/ui/ApplicationTools.tsx`'s
 *     own doc comment already establishes for the identical class of gap:
 *     `data-tour` attributes are omitted rather than pointed at a feature
 *     that will never legally be importable here (`no-sideways-features`,
 *     no carve-out).
 *
 *  4. **`LLMModelSelector` is injected**, not rendered from a real import —
 *     `widgets/llm-model-selector` does not exist anywhere in this worktree
 *     (grepped) and `features/` cannot import `widgets/` even if it did
 *     (R-L1, upward-forbidden). Same DI treaty this slice's own
 *     `IndexChat.tsx` (unit A4a) already established for the identical
 *     baseline dependency — `LLMModelSelectorProps` is reused directly from
 *     that file (intra-slice, R-L3-legal) rather than re-declared, since
 *     the shape this component needs (`selectedModel`/`onSelectModel`/
 *     `models`/`llmSettings`/`onSetLLMSettings`) is byte-for-byte identical.
 *     The five LLM props are grouped into one `llm: LLMModelSelectorProps`
 *     prop (spread directly onto the injected component) rather than five
 *     separate top-level props.
 *
 *  5. **`ToolkitForm.ToolFormContainer` (baseline: `features/toolkits/ui/
 *     form/ToolkitForm/ToolkitForm.jsx`'s barrel) is NOT the component used
 *     here.** Per the mission brief, that is a DIFFERENT, unrelated
 *     component from `./ToolFormContainer.tsx` (A4d, ported from
 *     `pages/Applications/Components/Tools/ToolConfigurationForm.jsx`'s
 *     sibling `ui/form/ToolFormContainer.jsx` file — the old app's own
 *     `ui/index.js` barrel aliasing makes the two genuinely easy to
 *     confuse, per the mission preamble's own warning). `ToolFormContainer.tsx`'s
 *     own doc comment names `TestToolSettings.jsx` (this file) as one of
 *     its two real, intra-slice consumers. Its `onChangeInputVariables`
 *     is `(fieldKey, value) => void` (single-field), not the baseline's
 *     whole-object updater — `onFieldChange` below does the same
 *     caller-owns-the-merge adaptation `ToolFormContainer.tsx`'s own doc
 *     comment documents as the established convention.
 *
 *  6. **`SingleSelect` (unit S1-D) is a substantially trimmed port** of the
 *     baseline's `Select.SingleSelect` — no `withSearch`/`showEmptyPlaceholder`/
 *     `emptyPlaceholder`/`showBorder` (see `shared/ui/SingleSelect/
 *     SingleSelect.tsx`'s own doc comment for the full trim rationale).
 *     Same substitution `features/pipelines/ui/select/ToolSelect.tsx`
 *     (unit A2h) already made for an analogous "pick one tool from a list"
 *     picker. `SingleSelectOption.value` is `string`-only (unlike the
 *     baseline, which could hand a whole tool object through as `value`)
 *     — `allToolsOptions` below always resolves `value` to the same
 *     string used for `label` (the tool's name), never the raw object.
 *
 *  7. **`ContentContainer`** (`apps/elitea-ui/src/pages/Common/Components/
 *     StyledComponents.jsx`) is inlined as `contentContainerSx` rather than
 *     imported — its own base CSS (`boxSizing:'border-box'` plus, on `lg`+,
 *     a scrollable-but-hidden-native-scrollbar `overflowY`) is carried over
 *     alongside the baseline's `sx` overrides, not just the overrides
 *     themselves.
 *
 * Everything else — the `selectedToolsSchema`/`schemaToolNames` derivation,
 * the explicit-selection-first `allToolsOptions` priority, the
 * `onChangeInputVariablesWrapper`'s index-name-error side effect, the
 * disabled-run-tool condition, and the scrollable-fields-plus-sticky-
 * run-button layout — is a faithful, byte-for-byte port of this specific
 * file's own logic.
 */
export interface TestToolSettingsProps {
  readonly projectId?: string | number | undefined;
  readonly toolkitId?: string | number | undefined;
  readonly selectedTool: string | null;
  readonly onChangeTool: (value: string | null) => void;
  readonly toolInputVariables: Readonly<Record<string, unknown>>;
  readonly onChangeInputVariables: (value: Readonly<Record<string, unknown>>) => void;
  readonly onRunTool: () => void;
  readonly isRunning: boolean;
  readonly isValidForm: boolean;
  readonly selectedToolSchema: JsonSchemaLike | null | undefined;
  readonly values: ToolkitConversationValues;
  /**
   * Grouped — see the module doc comment's DI-treaty item 4.
   *
   * OPTIONAL since the synchronous test pane (`./TestToolPane.tsx`) reuses this
   * panel: that pane runs ONE tool through
   * `POST /elitea_core/test_tool/prompt_lib/{projectId}/{toolId}` and never
   * asks a model anything, so there is no model for a selector to pick. Both
   * fields move together — a selector with no props, or props with no
   * selector, is a half-wired control.
   */
  readonly llm?: LLMModelSelectorProps | undefined;
  readonly LLMModelSelector?: ComponentType<LLMModelSelectorProps> | undefined;
  /** Grouped — the four `useIndexNameValidation()` fields the baseline reads individually. */
  readonly indexNameValidation: UseIndexNameValidationResult;
  /**
   * The state of the read that produced `selectedToolSchema` (#440). Grouped
   * because the two fields are only ever read together. Absent means the
   * caller resolves the schema without a read.
   */
  readonly toolSchemaRead?: { readonly isError: boolean; readonly onRetry: () => void };
}

interface SelectedToolsSchemaShape {
  readonly args_schemas?: Readonly<Record<string, unknown>> | undefined;
  readonly items?: { readonly enum?: readonly string[] | undefined } | undefined;
}

type ExplicitSelectedTool = string | { readonly name?: string | undefined };

function resolveToolName(tool: ExplicitSelectedTool): string {
  return typeof tool === 'string' ? tool : (tool.name ?? '');
}

function toToolOption(tool: ExplicitSelectedTool): SingleSelectOption {
  const name = resolveToolName(tool);
  const label = typeof tool === 'string' ? `${tool.charAt(0).toUpperCase()}${tool.slice(1)}`.replaceAll('_', ' ') : name;
  return { label, value: name };
}

/**
 * Whether the dynamic catalogue is the tier that feeds the picker.
 *
 * It is not, while the schema read is still in flight: an empty schema map
 * mid-flight is not yet "the static tier gave nothing". Split out of
 * `TestToolSettings` to keep that function under the §3.5 complexity budget.
 */
function usesDynamicToolTier(isFetchingSchemas: boolean, explicitCount: number, schemaCount: number): boolean {
  if (isFetchingSchemas) return false;
  return explicitCount === 0 && schemaCount === 0;
}

interface ToolPickerProps {
  /** Whether the dynamic tier is the one in use — an error from a tier nobody reads is not this picker's error. */
  readonly dynamicTierActive: boolean;
  readonly readFailed: boolean;
  readonly onRetry: () => void;
  readonly value: string;
  readonly options: readonly SingleSelectOption[];
  readonly onSelect: (value: string) => void;
  readonly onClear: () => void;
}

/**
 * The tool picker, or the error that replaces it (#440).
 *
 * A failed read is its own state. An empty picker means the toolkit offers
 * no tools; it must never stand in for a failure. Split into its own
 * component to keep `TestToolSettings` under the §3.5 complexity budget.
 */
function ToolPicker({ dynamicTierActive, readFailed, onRetry, value, options, onSelect, onClear }: ToolPickerProps): ReactNode {
  if (dynamicTierActive && readFailed) return <ToolListError onRetry={onRetry} />;

  return (
    <SingleSelect
      value={value}
      label={t('features.toolkits.testToolSettings.toolLabel', 'Tool')}
      onChange={onSelect}
      onClear={onClear}
      options={[...options]}
    />
  );
}

/** The baseline's three tiers, in its own order: an explicit selection, then the static schema, then the catalogue the backend publishes at runtime. */
function resolveAvailableTools(
  explicitSelectedTools: readonly ExplicitSelectedTool[],
  schemaToolNames: readonly string[],
  dynamicToolNames: readonly string[],
): readonly ExplicitSelectedTool[] {
  if (explicitSelectedTools.length > 0) return explicitSelectedTools;
  if (schemaToolNames.length > 0) return schemaToolNames;
  return dynamicToolNames;
}

function toolkitDiscoveryIdentity(props: TestToolSettingsProps, selectedProjectId: string | undefined) {
  return {
    projectId: props.projectId === undefined ? selectedProjectId : String(props.projectId),
    toolkitId: props.toolkitId === undefined ? undefined : String(props.toolkitId),
  };
}

export function TestToolSettings(props: TestToolSettingsProps): ReactNode {
  const { selectedTool, onChangeTool, toolInputVariables, onChangeInputVariables, onRunTool, isRunning, isValidForm, selectedToolSchema, values, llm, LLMModelSelector, indexNameValidation, toolSchemaRead } = props;
  const { clearIndexNameError, updateIndexNameError, isIndexNameValid, indexNameError } = indexNameValidation;

  const selectedProjectId = useSelectedProjectId();
  const { projectId, toolkitId } = toolkitDiscoveryIdentity(props, selectedProjectId);
  // `isError` has a reader now (#440). A lost schema read empties the static
  // tier, which makes the dynamic tier take over and, on a 200 with no tools,
  // draws an empty picker for a failure. It counts as a failed read here.
  const { toolkitSchemas, isFetching: isFetchingSchemas, isError: schemasReadFailed, refetch: retrySchemasRead } = useGetCurrentToolkitSchemas();
  const selectedToolsSchema = values.type !== undefined ? (toolkitSchemas?.[values.type] as { properties?: { selected_tools?: SelectedToolsSchemaShape } } | undefined)?.properties?.selected_tools : undefined;
  const disabledRunTool = !isValidForm || isRunning || Boolean(indexNameError);

  const schemaToolNames = useMemo<readonly string[]>(() => {
    const argsSchemasKeys = Object.keys(selectedToolsSchema?.args_schemas ?? {});
    if (argsSchemasKeys.length) return argsSchemasKeys;
    return [...(selectedToolsSchema?.items?.enum ?? [])];
  }, [selectedToolsSchema]);

  const explicitSelectedTools = useMemo<readonly ExplicitSelectedTool[]>(
    () => (values.settings?.['selected_tools'] as readonly ExplicitSelectedTool[] | undefined) ?? [],
    [values.settings],
  );

  // The baseline's third tier. It runs only when the first two tiers give
  // nothing, which is the toolkit type that publishes its tools at runtime.
  // It also waits for the schema read to settle: an empty schema map while
  // that read is still in flight is not yet "the static tier gave nothing".
  const usesDynamicTier = usesDynamicToolTier(isFetchingSchemas, explicitSelectedTools.length, schemaToolNames.length);
  const dynamicTools = toolkitTools.useToolkitTools({
    projectId,
    toolkitId,
    toolkitType: values.type,
    enabled: usesDynamicTier,
  });

  const allToolsOptions = useMemo<SingleSelectOption[]>(() => {
    const availableTools = resolveAvailableTools(explicitSelectedTools, schemaToolNames, dynamicTools.toolNames);
    return availableTools.map(toToolOption).sort((first, second) => first.label.toLowerCase().localeCompare(second.label.toLowerCase()));
  }, [dynamicTools.toolNames, explicitSelectedTools, schemaToolNames]);

  const onChangeInputVariablesWrapper = useCallback(
    (value: Readonly<Record<string, unknown>>) => {
      const indexName = value['index_name'];
      const isInvalid = selectedTool === IndexesToolsEnum.indexData && typeof indexName === 'string' && indexName !== '' && !isIndexNameValid(indexName);

      if (isInvalid) updateIndexNameError(indexName);
      else clearIndexNameError();

      onChangeInputVariables(value);
    },
    [clearIndexNameError, isIndexNameValid, onChangeInputVariables, selectedTool, updateIndexNameError],
  );

  const onFieldChange = useCallback(
    (fieldKey: string, value: unknown) => {
      onChangeInputVariablesWrapper({ ...toolInputVariables, [fieldKey]: value });
    },
    [onChangeInputVariablesWrapper, toolInputVariables],
  );

  const onClearTool = useCallback(() => onChangeTool(null), [onChangeTool]);
  const onSelectTool = useCallback((value: string) => onChangeTool(value), [onChangeTool]);

  // Both reads feed the same picker, so one retry control must run both.
  const onRetryToolList = useCallback(() => {
    dynamicTools.refetch();
    retrySchemasRead();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `dynamicTools` is a fresh object every render; its `refetch` is the stable identity this depends on
  }, [dynamicTools.refetch, retrySchemasRead]);

  return (
    <Box sx={rootSx}>
      <Box sx={contentContainerSx}>
        <Box>
          <Typography variant="subtitle">{t('features.toolkits.testToolSettings.title', 'Test Settings')}</Typography>
        </Box>
        {LLMModelSelector !== undefined && llm !== undefined && (
          <Box sx={llmModelContainerSx}>
            <LLMModelSelector {...llm} />
          </Box>
        )}
        <Box sx={toolSelectContainerSx}>
          <ToolPicker
            dynamicTierActive={usesDynamicTier}
            readFailed={dynamicTools.isError || schemasReadFailed}
            onRetry={onRetryToolList}
            value={selectedTool ?? ''}
            options={allToolsOptions}
            onSelect={onSelectTool}
            onClear={onClearTool}
          />
        </Box>

        {selectedTool && (
          <ToolArgumentPanel
            schema={selectedToolSchema}
            schemaRead={toolSchemaRead}
            toolInputVariables={toolInputVariables}
            onFieldChange={onFieldChange}
            onRunTool={onRunTool}
            runDisabled={disabledRunTool}
          />
        )}
      </Box>
    </Box>
  );
}

const rootSx: SxProps<Theme> = { width: '100%', height: '100%' };

// Baseline: `<ContentContainer sx={styles.contentContainer}>`
// (`apps/elitea-ui/src/pages/Common/Components/StyledComponents.jsx`) — the
// styled component's own base CSS (`boxSizing:'border-box'` plus, on `lg`+,
// a scrollable-but-scrollbar-hidden `overflowY`) is restored here alongside
// the baseline's `sx` overrides (`height`/`maxHeight`/`display`/
// `flexDirection`/`justifyContent`) rather than left dropped.
export const contentContainerSx: SxProps<Theme> = ({ breakpoints }) => ({
  height: '100%',
  maxHeight: '100%',
  display: 'flex',
  flexDirection: 'column',
  justifyContent: 'flex-start',
  boxSizing: 'border-box',
  [breakpoints.up('lg')]: {
    overflowY: 'scroll',
    msOverflowStyle: 'none',
    scrollbarWidth: 'none',
    '&::-webkit-scrollbar': { display: 'none' },
  },
});

const llmModelContainerSx: SxProps<Theme> = {
  flexShrink: 0,
  width: '100%',
  overflow: 'hidden',
  marginTop: '.875rem',
};

const toolSelectContainerSx: SxProps<Theme> = {
  marginTop: '0.5rem',
  paddingRight: '0.5rem',
};



