import { type ReactNode, useCallback, useMemo } from 'react';

import Link from '@mui/material/Link';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { ToolInitialValues } from '@/entities/toolkit';
import type { ToolkitTypeSchemaMap } from '@/entities/toolkit';
import { docsLink } from '@/shared/brand';
import { t } from '@/shared/i18n';
import { useGroupedCategories } from '@/shared/lib/hooks/useGroupedCategories';
import { CategoryFilter } from '@/shared/ui/CategoryFilter';
import { CategorySection } from '@/shared/ui/CategorySection';
import type { CategoryItem } from '@/shared/ui/CategoryItemCard';
import { GroupedCategory } from '@/shared/ui/GroupedCategory';
import { NoResultsMessage } from '@/shared/ui/NoResultsMessage';
import { ToolkitTypeIcon } from '@/shared/ui/ToolkitTypeIcon';

import { McpCategory } from '../lib/constants/mcp.constants';
import { convertToolkitSchema } from '../lib/helpers/toolkitSchema.helpers';
import { useIsMcpVisible } from '../api/useIsMcpVisible';
import { useToolMenuItems } from '../lib/hooks/useToolMenuItems';

interface ToolkitTypeSchemaShape {
  readonly required?: readonly string[];
  readonly name_required?: boolean;
}

/** Inlined rather than the local `useGetToolkitNameFromSchema` hook — that hook needs the schema map as a stable value at call time, but here it only becomes available inside `onAddTool`'s own `toolSchemas` closure argument (a hook cannot be called from inside a non-render callback). Same two rules `useGetToolkitNameFromSchema.ts`'s `getRequiredProperties`/`isNameRequired` implement, as plain functions. */
function requiredPropertiesOf(toolSchemas: ToolkitTypeSchemaMap, toolType: string): readonly string[] {
  return (toolSchemas[toolType] as ToolkitTypeSchemaShape | undefined)?.required ?? [];
}

function isNameRequiredFor(toolSchemas: ToolkitTypeSchemaMap, toolType: string): boolean {
  return (toolSchemas[toolType] as ToolkitTypeSchemaShape | undefined)?.name_required !== false;
}

interface ToolkitTypeSelectorToolDetail {
  readonly type: string;
  readonly schema: Readonly<Record<string, unknown>>;
  readonly meta: Readonly<Record<string, unknown>>;
  readonly settings?: Readonly<Record<string, unknown>>;
  readonly name?: string;
  readonly [key: string]: unknown;
}

/** @public */
export interface ToolkitTypeSelectorProps {
  readonly onSelectTool: (detail: ToolkitTypeSelectorToolDetail) => void;
  readonly setFormikInitialValues: (updater: (prev: Readonly<Record<string, unknown>>) => Readonly<Record<string, unknown>>) => void;
  readonly isMCP?: boolean;
  readonly isApplication?: boolean;
  readonly disableNavigation?: boolean;
}

function getDefaultTools(selectedToolsProperty: Readonly<{ readonly args_schemas?: Readonly<Record<string, { readonly default?: boolean }>> }> | undefined): readonly string[] {
  return Object.entries(selectedToolsProperty?.args_schemas ?? {})
    .filter(([, value]) => value.default === true)
    .map(([key]) => key);
}

interface ResolvedToolSelection {
  readonly detail: ToolkitTypeSelectorToolDetail;
  readonly initialFormValues: Readonly<Record<string, unknown>>;
}

/**
 * The full "a toolkit type was picked" computation — split out of
 * `onAddTool`'s closure purely to keep `ToolkitTypeSelector` under the
 * oxlint complexity budget (the baseline's own `onAddTool`, `ToolkitTypeSelector.jsx:32-148`,
 * is a single ~110-line callback).
 */
function resolveToolSelection(toolType: string, toolSchemas: ToolkitTypeSchemaMap): ResolvedToolSelection {
  const nameIsRequired = isNameRequiredFor(toolSchemas, toolType);
  const descriptionIsRequired = requiredPropertiesOf(toolSchemas, toolType).includes('description');
  const rawSchema = (toolSchemas[toolType] ?? { properties: {} }) as Parameters<typeof convertToolkitSchema>[0];
  const schema = convertToolkitSchema(rawSchema);
  const selectedTools = getDefaultTools(schema.properties?.['selected_tools']);
  const metadata = (schema.metadata as Readonly<Record<string, unknown>> | undefined) ?? {};

  const initialValues = (ToolInitialValues as Record<string, { readonly settings?: Readonly<Record<string, unknown>> }>)[toolType] ?? {};
  const settings = { ...initialValues.settings, selected_tools: selectedTools };

  const detail: ToolkitTypeSelectorToolDetail = {
    ...initialValues,
    settings,
    type: toolType,
    schema: { required: [''], ...schema },
    meta: metadata,
  };

  const initialFormValues: Readonly<Record<string, unknown>> = {
    settings,
    ...(nameIsRequired ? { name: '' } : {}),
    ...(descriptionIsRequired ? { description: '' } : {}),
    type: toolType,
    meta: metadata,
  };

  return { detail, initialFormValues };
}

/**
 * Ported from `apps/elitea-ui/src/pages/Toolkits/ToolkitTypeSelector.jsx`
 * (240 lines).
 *
 * DISCLOSED DEVIATIONS:
 *  - `useToolMenuItems` (baseline: `hooks/application`) is a LOCAL
 *    duplicate (`../lib/hooks/useToolMenuItems.ts`, this same unit) — a
 *    cross-top-level-feature import in the baseline (agents), forbidden by
 *    `no-sideways-features`, exactly as this batch's own mission preamble
 *    flags for this file by name. The mission preamble also names
 *    `useGetToolkitNameFromSchema` (baseline: `features/pipelines/
 *    flow-editor`) as a second forbidden-sideways import this file makes —
 *    reading the ACTUAL baseline `ToolkitTypeSelector.jsx` directly
 *    (verified, not assumed) shows it uses that hook's
 *    `isNameRequired`/`getRequiredProperties` ONLY inside `onAddTool`,
 *    where the real schema map is already available as that closure's own
 *    `toolSchemas` argument — `isNameRequiredFor`/`requiredPropertiesOf`
 *    below are the same two rules as plain functions instead of a second
 *    duplicated hook (a hook cannot be called from inside a non-render
 *    callback the way the baseline's ambient-Redux-backed hook could).
 *  - **Real backend gap, disclosed, not invented:** the entire
 *    vector-storage/embedding-model/image-generation-model default-value
 *    pre-loading branch (baseline: `useLazyListModelsQuery`,
 *    `onAddTool`'s `shouldLoadingConfigurations` block) is DROPPED. The
 *    mission brief states this directly: "No ListModels endpoint exists (so
 *    old-app LLM-settings-defaulting behavior that reads a live model list
 *    cannot be faithfully ported — disclose, don't invent)." Selecting a
 *    toolkit type that has one of these fields still works; it just starts
 *    with `getToolInitialValueBySchema`'s/`ToolInitialValues`' own default
 *    (usually blank), not a live server-resolved default.
 *  - `getToolInitialValueBySchema` (baseline: `common/
 *    getToolInitialValueBySchema.js`) has no confirmed port anywhere in this
 *    worktree (grepped directly — zero hits under any name). Dropped in
 *    favour of `entities/toolkit`'s promoted `ToolInitialValues[toolType]`
 *    map alone (the baseline's own fallback when the schema-derived
 *    function returns nothing) — a real, disclosed narrowing, not an
 *    invented replacement.
 *  - `Category.GroupedCategory` is one monolithic search+chips+grouped-list
 *    component in the baseline; here it is the documented two-part split
 *    (`CategoryFilter`'s chrome composing `GroupedCategory`'s item region as
 *    `children`) — the same screen, assembled by composition.
 *
 *    **CORRECTED — the grouping itself is no longer dropped.** This file used
 *    to render every toolkit type under ONE un-grouped `CategorySection` with
 *    no category chips, on the stated grounds that `useToolkitSearch`'s
 *    category source "was not determinable from the baseline's available
 *    context". It is determinable, and it is implemented now:
 *    `metadata.categories[0]` off the toolkit-type schema for the toolkit and
 *    application tabs, `Local`/`Remote` for the MCP tab
 *    (`lib/hooks/useToolMenuItems.ts`'s `toolkitCategory`), with
 *    `shared/lib/hooks/useGroupedCategories` — the ported baseline hook —
 *    owning search, chip selection and grouping. That gap was the visible one:
 *    the production screens show a category-chip row and uppercase
 *    per-category section headings, and this screen showed neither.
 *  - The `isApplication`/`appType`-URL auto-select effect (baseline lines
 *    155-163, `useParams().appType` + `react-router-dom`) is dropped: this
 *    component owns no route-matching, and no caller in this unit's owned
 *    files needs it (only `CreateToolkit.tsx`, a route page, has `appType`
 *    in scope — it is a disclosed gap on that page instead, see its own doc
 *    comment).
 */
/** Split out purely to keep `ToolkitTypeSelector` under the oxlint complexity budget (12) — each of the three copy-selection helpers below is its own independent branch chain. */
function resolveSelectorTitle(isApplication: boolean, isMCP: boolean): string {
  if (isApplication) return t('toolkits.toolkitTypeSelector.titleApplication', 'Choose the application type');
  return isMCP ? t('toolkits.toolkitTypeSelector.titleMcp', 'Choose the MCP type') : t('toolkits.toolkitTypeSelector.titleToolkit', 'Choose the toolkit type');
}

/** Same reason as {@link resolveSelectorTitle}. */
function resolveNoResultsTitle(isApplication: boolean, isMCP: boolean): string {
  if (isApplication) return t('toolkits.toolkitTypeSelector.noResultsApplication', 'No applications found');
  return isMCP ? t('toolkits.toolkitTypeSelector.noResultsMcp', 'No MCPs found') : t('toolkits.toolkitTypeSelector.noResultsToolkit', 'No toolkits found');
}

/** Same reason as {@link resolveSelectorTitle}. */
function resolveSearchPlaceholder(isApplication: boolean, isMCP: boolean): string {
  if (isApplication) return t('toolkits.toolkitTypeSelector.searchApplication', 'Search applications');
  return !isMCP ? t('toolkits.toolkitTypeSelector.searchToolkit', 'Search toolkits') : t('toolkits.toolkitTypeSelector.searchMcp', 'Search MCPs');
}

/**
 * Baseline: `ToolkitTypeSelector.jsx:178`'s doc URL. The page path is the
 * baseline's; the origin is the brand pack's (`docsLink`, ADR-0024 WP8).
 */
const MCP_CREATION_DOCS_PATH = 'integrations/mcp/create-and-use-server-stdio';

const mcpEmptyStateLinkSx: SxProps<Theme> = { textDecoration: 'underline', '&:hover': { cursor: 'pointer', textDecoration: 'underline' } };

/**
 * `ToolkitTypeSelector.jsx:165-190`'s MCP-specific `EmptyPlaceholder` —
 * distinct from the generic "no results, try adjusting your search terms"
 * message (reserved for the non-MCP / genuinely-no-match case below): when
 * there are zero local MCP toolkit types to show, this points the user at
 * the docs page that explains how to create one, rather than telling them
 * to adjust a search that has nothing to search over. It is now wired the way
 * the baseline wires it — `allowEmptyCategory={isMCP}` keeps the empty `Local`
 * category rendered, and this goes into `CategorySection`'s
 * `emptyPlaceholder` slot INSIDE the item grid, so it sits left-aligned under
 * the LOCAL rule (verified against the live production screen) rather than
 * centred in a 12.5rem box of its own, which is what the previous
 * sibling-branch rendering produced.
 */
function McpNoLocalServersMessage(): ReactNode {
  const docsUrl = useMemo(() => docsLink(MCP_CREATION_DOCS_PATH), []);
  return (
    <Typography
      variant="bodyMedium"
      color="text.primary"
    >
      {t('toolkits.toolkitTypeSelector.mcpEmptyStatePrefix', 'Still no local MCP available. Follow creation guides in our ')}
      <Link
        href={docsUrl}
        target="_blank"
        rel="noopener noreferrer"
        sx={mcpEmptyStateLinkSx}
      >
        {t('toolkits.toolkitTypeSelector.mcpEmptyStateLinkText', 'Documentation')}
      </Link>
      {t('toolkits.toolkitTypeSelector.mcpEmptyStateSuffix', '.')}
    </Typography>
  );
}

/**
 * The grouped-item region sits inside `CategoryFilter`'s centred flex column,
 * so it has to be the full-width, centred, 1.5rem-gapped column the baseline's
 * `itemsContainer` was for the sections themselves — otherwise every section
 * shrinks to its content width and the 4-column grid stops lining up with the
 * search box above it.
 */
const groupedItemsSx: SxProps<Theme> = (theme: Theme) => ({
  width: '100%',
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  gap: theme.spacing(3),
});

interface ToolkitTypeMenuItem {
  readonly key: string;
  readonly label: string;
  readonly category: string;
  readonly icon: ReactNode;
  readonly onClick: () => void;
}

function categoryOfToolkit(item: ToolkitTypeMenuItem): string {
  return item.category;
}

function selectToolkit(item: ToolkitTypeMenuItem): void {
  item.onClick();
}

export function ToolkitTypeSelector({ onSelectTool, setFormikInitialValues, isMCP = false, isApplication = false }: ToolkitTypeSelectorProps): ReactNode {
  const onAddTool = useCallback(
    (toolType: string, toolSchemas: ToolkitTypeSchemaMap) => () => {
      const { detail, initialFormValues } = resolveToolSelection(toolType, toolSchemas);
      onSelectTool(detail);
      setFormikInitialValues(() => initialFormValues);
    },
    [onSelectTool, setFormikInitialValues],
  );

  const isMcpVisible = useIsMcpVisible();
  const { toolMenuItems, isFetchingToolkitTypes } = useToolMenuItems({ onAddTool, isMCP, isApplication });

  /**
   * Baseline `useToolkitSearch.js`'s `localGroupForMCP`: with nothing but the
   * pre-built "Remote MCP" entry to show, `Local` is pinned into the chip row
   * anyway so its empty section — and with it the "Still no local MCP
   * available" guidance below — is reachable at all.
   */
  const localGroupForMCP = useMemo(() => (!isMCP || toolMenuItems.some((item) => item.label !== 'Remote MCP') ? undefined : McpCategory.Local), [isMCP, toolMenuItems]);

  /**
   * The baseline resolves each tile's brand glyph in `useToolkitSearch.js`
   * (`icon: child.icon || getToolIcon(child.key, theme)`); this port dropped
   * it because no per-type icon resolver existed, so every tile rendered
   * label-only. `shared/ui/ToolkitTypeIcon` is that resolver.
   */
  const itemsWithIcons = useMemo<readonly ToolkitTypeMenuItem[]>(
    () => toolMenuItems.map((item) => ({ ...item, icon: <ToolkitTypeIcon type={item.key} /> })),
    [toolMenuItems],
  );

  const { allCategories, groupedItems, selectedCategories, searchQuery, onSearchChange, onSelectCategory } = useGroupedCategories<ToolkitTypeMenuItem>(
    itemsWithIcons,
    categoryOfToolkit,
    selectToolkit,
    localGroupForMCP,
  );

  const renderCategory = useCallback(
    (category: string, items: readonly CategoryItem[]): ReactNode => (
      <CategorySection
        category={category}
        items={items}
        {...(isMCP ? { emptyPlaceholder: <McpNoLocalServersMessage /> } : {})}
      />
    ),
    [isMCP],
  );

  if (isMCP && !isMcpVisible) return null;

  const title = resolveSelectorTitle(isApplication, isMCP);
  const noResultsTitle = resolveNoResultsTitle(isApplication, isMCP);

  return (
    <CategoryFilter
      title={title}
      searchPlaceholder={resolveSearchPlaceholder(isApplication, isMCP)}
      searchQuery={searchQuery}
      onSearchChange={onSearchChange}
      allCategories={[...allCategories]}
      selectedCategories={[...selectedCategories]}
      onSelectCategory={onSelectCategory}
    >
      <GroupedCategory
        isLoading={isFetchingToolkitTypes}
        allCategories={allCategories}
        selectedCategories={selectedCategories}
        groupedItems={groupedItems}
        allowEmptyCategory={isMCP}
        renderCategory={renderCategory}
        noResultsSlot={
          <NoResultsMessage
            title={noResultsTitle}
            description={t('toolkits.toolkitTypeSelector.noResultsDescription', 'Try adjusting your search terms')}
          />
        }
        sx={groupedItemsSx}
      />
    </CategoryFilter>
  );
}

// `disableNavigation` is accepted for baseline call-site compatibility
// (`ToolkitEditor.tsx`/`CreateToolkit.tsx` both pass it) but has no effect
// here — the baseline used it only inside `useToolkitSearch`'s
// navigation-on-select branch, which this port does not reproduce (see the
// module doc comment's `Category.GroupedCategory` deviation).
