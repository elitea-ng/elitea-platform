import { type ReactNode, useCallback, useEffect, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate } from '@tanstack/react-router';

import { isMcpToolkit } from '@/entities/toolkit';
import { isPublicProject } from '@/entities/project';
import { ToolkitsList, type ToolkitListItem } from '@/features/toolkits';
import { useListToolkitInstances } from '@/shared/api/generated/toolkits/toolkits';
import type { ToolkitInstance } from '@/shared/api/generated/model';
import { unwrapListPage } from '@/shared/api/unwrap';
import { getConfig } from '@/shared/config';
import { t } from '@/shared/i18n';
import { BaseTab } from '@/shared/ui/BaseTab';
import { BaseTabs } from '@/shared/ui/BaseTabs';
import { EntityCard, entityTypeIcon } from '@/shared/ui/EntityCardList';

import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { ToolkitsAuthorCard } from './ui/ToolkitsAuthorCard';

const PAGE_SIZE = 20;

const pageSx: SxProps<Theme> = { height: '100%', display: 'flex', flexDirection: 'column' };
const tabBarSx: SxProps<Theme> = { flexShrink: 0, borderBottom: 1, borderColor: 'divider', padding: '0 1.5rem' };
const tabPanelSx: SxProps<Theme> = { flex: 1, minHeight: 0, overflowY: 'auto' };
const listSx: SxProps<Theme> = { height: '100%' };

export interface ToolkitsProps {
  readonly isMCP?: boolean;
}

/** Same `entities/project`'s `isPublicProject` + `shared/config`'s `getConfig()` two-line reproduction `pages/agents/lib/isPublicAgentsProject.ts`'s own doc comment already establishes for that page's identical need — `pages/toolkits`'s own copy, not a new concept. */
function isPublicToolkitsProject(projectId: string | undefined): boolean {
  if (projectId === undefined) return false;
  const config = getConfig();
  if (config.status !== 'ok') return false;
  return isPublicProject(projectId, config.config.vite_public_project_id);
}

/** Baseline navigate-to-create-page condition (`ToolkitsList.jsx:97-131`) — mirrored, not imported (`features/toolkits`' public `index.ts` doesn't export its own same-named copy either). Exported so this page's own test file can exercise the gate directly, no network mock needed. */
export function shouldRedirectToCreatePage(input: {
  readonly isPublicProject: boolean;
  readonly isLoading: boolean;
  readonly isError: boolean;
  readonly hasMoreRawPages: boolean;
  readonly scopedItemCount: number;
  readonly selectedTypesCount: number;
}): boolean {
  const { isPublicProject: isPublic, isLoading, isError, hasMoreRawPages, scopedItemCount, selectedTypesCount } = input;
  return !isPublic && !isLoading && !isError && !hasMoreRawPages && scopedItemCount === 0 && selectedTypesCount === 0;
}

/** `settings` narrowed to the two string fields `ToolkitListItem`/`toolkitDisplayName`'s fallback chain actually reads — `ToolkitInstance.settings` is an untyped `Record<string, unknown>` (arbitrary per-type JSON), not directly assignable to `ToolkitListItem`'s narrower shape. */
function toListItem(row: ToolkitInstance): ToolkitListItem {
  const settings = row.settings;
  const eliteaTitle = typeof settings['elitea_title'] === 'string' ? settings['elitea_title'] : undefined;
  const configurationTitle = typeof settings['configuration_title'] === 'string' ? settings['configuration_title'] : undefined;
  return {
    id: row.id,
    name: row.name,
    type: row.type,
    ...((eliteaTitle !== undefined || configurationTitle !== undefined) && {
      settings: {
        ...(eliteaTitle !== undefined && { elitea_title: eliteaTitle }),
        ...(configurationTitle !== undefined && { configuration_title: configurationTitle }),
      },
    }),
  };
}

/** `tagList` — every distinct toolkit TYPE among the currently-loaded rows, name-sorted. Simpler than `useLoadToolkits.ts`'s own `buildProjectWideTagList` (the full schema-catalogue-derived list): that hook's `toolkitSchemas`/label-lookup machinery (`useGetCurrentToolkitSchemas`) is not exported from `features/toolkits`' public `index.ts` either (same §3.5 budget ceiling — see that barrel's own doc comment), so this reads the raw `type` string directly off whatever rows have actually loaded, same "presentation-layer, not a full re-implementation" scope every other disclosed gap in this unit takes. */
function toTagList(rows: readonly ToolkitInstance[]): { readonly id: number; readonly name: string }[] {
  const seen = new Set<string>();
  const tags: { id: number; name: string }[] = [];
  for (const row of rows) {
    if (seen.has(row.type)) continue;
    seen.add(row.type);
    tags.push({ id: tags.length + 1, name: row.type });
  }
  return tags.sort((a, b) => a.name.localeCompare(b.name));
}

/** `resolvedPage === 0 ? freshRows : [...priorRows, ...freshRows]`, split out to keep {@link usePagedToolkitRows}'s own complexity under the §3.5 budget. */
function nextRowsAfterFetch(
  resolvedPage: number,
  priorRows: readonly ToolkitInstance[],
  freshRows: readonly ToolkitInstance[],
): readonly ToolkitInstance[] {
  return resolvedPage === 0 ? freshRows : [...priorRows, ...freshRows];
}

/** A DISABLED query (`enabled: projectId !== undefined`) reports `isLoading` `false` (TanStack Query: `isPending && isFetching`, never-started `isFetching` is `false`) — before `projectId` resolves off the router context, that misreads as "loaded, empty" rather than "hasn't started". Folds `projectId === undefined` in so every caller (spinner + the redirect gate below) gets the right signal. */
function derivePagedIsLoading(queryIsLoading: boolean, resolvedPage: number, projectId: string | undefined): boolean {
  if (projectId === undefined) return true;
  return queryIsLoading && resolvedPage === 0;
}

interface UsePagedToolkitRowsResult {
  readonly rows: readonly ToolkitInstance[];
  readonly isLoading: boolean;
  readonly isFetchingMore: boolean;
  readonly isError: boolean;
  readonly hasMore: boolean;
  readonly totalCount: number;
  readonly onLoadMore: () => void;
}

/**
 * Real pagination/accumulation over `useListToolkitInstances` (`@/shared/
 * api/generated/toolkits/toolkits` — the SAME primitive `./lib/
 * useToolkitDetail.ts` already reaches for; `useLoadToolkits`/a `renderCard`
 * are budget-capped out of `features/toolkits`' public `index.ts` — see the
 * `Toolkits` doc comment below).
 *
 * `page`/`accumulatedRows` reset, and each new page's rows append, DURING
 * RENDER (React's "adjusting state when a prop changes" pattern:
 * https://react.dev/learn/you-might-not-need-an-effect#adjusting-some-state-when-a-prop-changes,
 * not a `useEffect`) so every returned value is consistent within ONE
 * render — an effect-based version raced `totalCount` (always current, read
 * off `query.data`) against a not-yet-committed `setAccumulatedRows`,
 * leaving `rows` empty for one extra render: long enough for the
 * private-empty-project auto-redirect below to misfire on a non-empty one.
 */
function usePagedToolkitRows(projectId: string | undefined, isMCP: boolean): UsePagedToolkitRowsResult {
  const safeProjectId = projectId ?? '';
  const [page, setPage] = useState(0);
  const [accumulatedRows, setAccumulatedRows] = useState<readonly ToolkitInstance[]>([]);

  const scopeKey = `${safeProjectId}|${String(isMCP)}`;
  const [prevScopeKey, setPrevScopeKey] = useState(scopeKey);
  let resolvedPage = page;
  let resolvedRows = accumulatedRows;
  if (scopeKey !== prevScopeKey) {
    setPrevScopeKey(scopeKey);
    setPage(0);
    setAccumulatedRows([]);
    resolvedPage = 0;
    resolvedRows = [];
  }

  const query = useListToolkitInstances(
    safeProjectId,
    { limit: PAGE_SIZE, offset: resolvedPage * PAGE_SIZE },
    { query: { enabled: projectId !== undefined } },
  );
  // R-A6 (#132) via the one helper. `undefined` is preserved deliberately:
  // the `wire !== prevWire` sentinel below distinguishes "no response yet"
  // from "a page arrived", and memoising keeps that identity stable per fetch
  // (an unmemoised fresh object would re-trigger it every render).
  const wire = useMemo(
    () => (query.data === undefined ? undefined : unwrapListPage<ToolkitInstance>(query.data, 'listToolkitInstances')),
    [query.data],
  );

  const [prevWire, setPrevWire] = useState(wire);
  if (wire !== prevWire) {
    setPrevWire(wire);
    if (wire !== undefined) {
      resolvedRows = nextRowsAfterFetch(resolvedPage, resolvedRows, wire.rows);
      setAccumulatedRows(resolvedRows);
    }
  }

  const totalCount = wire?.total ?? 0;
  const hasMore = !query.isFetching && (resolvedPage + 1) * PAGE_SIZE < totalCount;
  const onLoadMore = useCallback(() => setPage((prev) => prev + 1), []);

  return {
    rows: resolvedRows,
    isLoading: derivePagedIsLoading(query.isLoading, resolvedPage, projectId),
    isFetchingMore: query.isFetching && resolvedPage > 0,
    isError: query.isError,
    hasMore,
    totalCount,
    onLoadMore,
  };
}

interface UseToolkitsListDataResult {
  readonly data: readonly ToolkitListItem[];
  readonly tagList: readonly { readonly id: number; readonly name: string }[];
  readonly selectedTypes: readonly string[];
  readonly onSelectType: (typeName: string) => void;
  readonly onClearTypes: () => void;
  readonly onLoadMore: () => void;
  readonly isLoading: boolean;
  readonly isFetchingMore: boolean;
  readonly isError: boolean;
  readonly hasMore: boolean;
  readonly totalCount: number;
  readonly isRedirectEligible: boolean;
}

/** MCP-scoping, client-side type-filtering, and the redirect gate, layered over {@link usePagedToolkitRows} — split out of the `Toolkits` component body to keep its own cyclomatic complexity under the §3.5 budget. */
function useToolkitsListData(projectId: string | undefined, isMCP: boolean, isPublicProjectFlag: boolean): UseToolkitsListDataResult {
  const [selectedTypes, setSelectedTypes] = useState<readonly string[]>([]);
  const paged = usePagedToolkitRows(projectId, isMCP);

  const scopedRows = useMemo(() => paged.rows.filter((row) => isMcpToolkit(row) === isMCP), [paged.rows, isMCP]);
  const filteredRows = useMemo(
    () => (selectedTypes.length === 0 ? scopedRows : scopedRows.filter((row) => selectedTypes.includes(row.type))),
    [scopedRows, selectedTypes],
  );
  const data = useMemo(() => filteredRows.map(toListItem), [filteredRows]);
  const tagList = useMemo(() => toTagList(scopedRows), [scopedRows]);

  const onSelectType = useCallback((typeName: string) => {
    setSelectedTypes((prev) => (prev.includes(typeName) ? prev.filter((type) => type !== typeName) : [...prev, typeName]));
  }, []);
  const onClearTypes = useCallback(() => setSelectedTypes([]), []);

  const isRedirectEligible = shouldRedirectToCreatePage({
    isPublicProject: isPublicProjectFlag,
    isLoading: paged.isLoading,
    isError: paged.isError,
    hasMoreRawPages: paged.hasMore,
    scopedItemCount: scopedRows.length,
    selectedTypesCount: selectedTypes.length,
  });

  return {
    data,
    tagList,
    selectedTypes,
    onSelectType,
    onClearTypes,
    onLoadMore: paged.onLoadMore,
    isLoading: paged.isLoading,
    isFetchingMore: paged.isFetchingMore,
    isError: paged.isError,
    hasMore: paged.hasMore,
    totalCount: paged.totalCount,
    isRedirectEligible,
  };
}

interface ToolkitCardProps {
  readonly item: ToolkitListItem;
  readonly isMCP: boolean;
  readonly onOpen: () => void;
}

/**
 * One toolkit/MCP card. The bespoke `ButtonBase` tile this used to render
 * is replaced by the shared `EntityCard` every other list page now uses, so
 * a toolkit card carries the same round gradient icon tile, `headingSmall`
 * title and divider-separated bottom row as an agent or pipeline card. The
 * toolkit's `type` rides in the bottom row's chip slot (and the table
 * view's "Type" column), which is where the production reference shows it.
 */
function ToolkitCard({ item, isMCP, onOpen }: ToolkitCardProps): ReactNode {
  return (
    <EntityCard
      data-testid="toolkit-card"
      item={{
        id: item.id,
        name: item.name,
        icon: entityTypeIcon(isMCP ? 'mcp' : 'toolkit'),
        tags: [{ id: item.type, name: item.type }],
        typeLabel: item.type,
        onClick: onOpen,
      }}
    />
  );
}

/**
 * Ported from `apps/elitea-ui/src/pages/Toolkits/Toolkits.jsx` (39 lines) —
 * ROUTE-030-family (`/toolkits/:tab`, `/mcps/:tab`; spec §8.1); `isMCP`
 * mirrors the baseline's own single component reused for both.
 *
 * **Composition gap CLOSED.** The baseline's body is `<StickyTabs>` wrapping
 * ONE tab whose content is `<ToolkitsList isMCP cardContentType={...} />`;
 * this page now wires the real, already-ported `ToolkitsList` (`@/features/
 * toolkits`) to a real data source instead of the empty placeholder `<Box
 * data-testid=.../>` this file used to render. `useLoadToolkits`/a
 * `renderCard` are budget-capped out of `features/toolkits`' public
 * `index.ts`, and `no-deep-slice-import` forbids reaching that slice's
 * internals directly from `pages/` (same rule `./lib/useToolkitDetail.ts`
 * cites) — so this page talks to the SAME real `useListToolkitInstances`
 * endpoint directly (`@/shared/api/generated/toolkits/toolkits`, the
 * `shared/` layer) and owns its own minimal `renderCard`, rather than
 * duplicating `useLoadToolkits.ts`'s full 188-line body.
 *
 * DISCLOSED, NARROWER than the baseline (real backend/budget constraints,
 * same class of gap `useLoadToolkits.ts`'s own doc comment discloses):
 *  - **No server-side search/sort/type filtering** — `ListToolkitInstancesParams`
 *    only has `limit`/`offset`. The "Types" filter is real and interactive,
 *    but filters CLIENT-SIDE over whatever pages have already loaded.
 *  - **No search box** — feeds the same unsupported server param, so not
 *    added (`query` stays `undefined`).
 *  - **`tagList`** is derived from loaded rows' raw `type` strings, not the
 *    baseline's per-type schema-derived label (see `toTagList`'s own doc).
 *  - **`AuthorInformation` gap CLOSED** — `rightPanelExtra` carries the real
 *    author card; see `./ui/ToolkitsAuthorCard.tsx`.
 */
export function Toolkits({ isMCP = false }: ToolkitsProps): ReactNode {
  const title = isMCP ? t('pages.toolkits.toolkits.titleMcp', 'MCPs') : t('pages.toolkits.toolkits.title', 'Toolkits');
  const emptyTitle = isMCP
    ? t('pages.toolkits.toolkits.emptyMcpTitle', 'No MCPs yet')
    : t('pages.toolkits.toolkits.emptyTitle', 'No toolkits yet');
  const emptyDescription = isMCP
    ? t('pages.toolkits.toolkits.emptyMcpDescription', 'Create your first MCP to get started.')
    : t('pages.toolkits.toolkits.emptyDescription', 'Create your first toolkit to get started.');

  const navigate = useNavigate();
  const projectId = useSelectedProjectId();
  const isPublic = isPublicToolkitsProject(projectId);

  const list = useToolkitsListData(projectId, isMCP, isPublic);

  const handleOpen = useCallback(
    (toolkitId: string) => {
      if (isMCP) {
        void navigate({ to: '/mcps/$tab/$mcpId', params: { tab: 'all', mcpId: toolkitId } });
      } else {
        void navigate({ to: '/toolkits/$tab/$toolkitId', params: { tab: 'all', toolkitId } });
      }
    },
    [isMCP, navigate],
  );

  const handleCreateClick = useCallback(() => {
    void navigate({ to: isMCP ? '/mcps/create' : '/toolkits/create' });
  }, [isMCP, navigate]);

  useEffect(() => {
    if (!list.isRedirectEligible) return;
    void navigate({ to: isMCP ? '/mcps/create' : '/toolkits/create', replace: true });
  }, [list.isRedirectEligible, isMCP, navigate]);

  return (
    <Box sx={pageSx}>
      <Box sx={tabBarSx}>
        <BaseTabs
          value={0}
          aria-label={title}
        >
          <BaseTab
            label={title}
            data-testid="toolkits-tab-all"
          />
        </BaseTabs>
      </Box>
      <Box
        sx={tabPanelSx}
        role="tabpanel"
      >
        <Box data-testid={isMCP ? 'mcps-list-panel' : 'toolkits-list-panel'}>
          <ToolkitsList
            sx={listSx}
            data={list.data}
            renderCard={(item) => (
              <ToolkitCard
                key={item.id}
                item={item}
                isMCP={isMCP}
                onOpen={() => handleOpen(item.id)}
              />
            )}
            listState={{
              isLoading: list.isLoading,
              isFetchingMore: list.isFetchingMore,
              isError: list.isError,
              hasMore: list.hasMore,
              onLoadMore: list.onLoadMore,
              totalCount: list.totalCount,
            }}
            typeFilter={{
              tagList: list.tagList,
              selectedTypes: list.selectedTypes,
              onSelectType: list.onSelectType,
              onClearTypes: list.onClearTypes,
            }}
            isMCP={isMCP}
            emptyStateConfig={{ title: emptyTitle, description: emptyDescription, onCreateClick: handleCreateClick }}
            // Single-line element (not this file's usual one-prop-per-line style): it sits 1 line under the §3.5 400-line budget.
            rightPanelExtra={<ToolkitsAuthorCard projectId={projectId} isMCP={isMCP} />}
          />
        </Box>
      </Box>
    </Box>
  );
}
