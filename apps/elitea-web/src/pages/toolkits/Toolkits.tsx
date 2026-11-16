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
import { PERMISSIONS } from '@/shared/lib/permissions';
import { t } from '@/shared/i18n';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';
import { EntityCard, EntitySelectionProvider, entityTypeIcon } from '@/shared/ui/EntityCardList';
import { EntityFolderDnd, EntityFolderScope, useFolderFilter } from '@/widgets/entity-folders';
import { pageBodySx, pageSx, useListSearchQuery } from '@/widgets/page-header';
import { usePermissionSet } from '@/widgets/sidebar';

import { matchesQuery, toListItem, toTagList } from './lib/toolkitListRows';
import { useFolderRows } from './lib/useFolderRows';
import { useToolkitSelection } from './lib/useToolkitSelection';
import { toolkitsCopy } from './lib/toolkitsCopy';
import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { EmptyFolderNote } from './ui/EmptyFolderNote';
import { ToolkitsSidebar } from './ui/ToolkitsSidebar';

const PAGE_SIZE = 20;

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
  /** Whether the server holds rows past the ones loaded, regardless of a fetch in flight. The redirect gate reads THIS, not `hasMore` (#2000). */
  readonly hasMoreRawPages: boolean;
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
    // The server filters by kind, so a page holds only this list's rows. Without it
    // the first 20 rows of a project with many toolkits held no MCP at all (#2092).
    { limit: PAGE_SIZE, offset: resolvedPage * PAGE_SIZE, mcp: isMCP },
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

  // Starts `undefined`, NOT `wire`: a remount that finds the page already in
  // the query cache (a second visit, or one right after a toolkit was created)
  // has `wire` on its first render, and seeding the sentinel with it meant the
  // rows were never accumulated: the list read empty and showed the "new
  // toolkit" view until a refetch happened to hand back a new object.
  const [prevWire, setPrevWire] = useState<typeof wire>(undefined);
  if (wire !== prevWire) {
    setPrevWire(wire);
    if (wire !== undefined) {
      resolvedRows = nextRowsAfterFetch(resolvedPage, resolvedRows, wire.rows);
      setAccumulatedRows(resolvedRows);
    }
  }

  const totalCount = wire?.total ?? 0;
  // `hasMore` hides "load more" while a fetch is in flight; the redirect gate must not inherit that,
  // or a refetch on re-entry reads as an empty project and sends the person to Create (#2000).
  // While a later page is in flight `wire` is undefined and `totalCount` reads 0: that is "not known yet",
  // never "no more rows", or the redirect gate sends the person to Create mid load-more (#2092).
  const nextPageUnknown = resolvedPage > 0 && wire === undefined;
  const hasMoreRawPages = nextPageUnknown || (resolvedPage + 1) * PAGE_SIZE < totalCount;
  const hasMore = !query.isFetching && hasMoreRawPages;
  const onLoadMore = useCallback(() => setPage((prev) => prev + 1), []);

  return {
    rows: resolvedRows,
    isLoading: derivePagedIsLoading(query.isLoading, resolvedPage, projectId),
    isFetchingMore: query.isFetching && resolvedPage > 0,
    isError: query.isError,
    hasMore,
    hasMoreRawPages,
    totalCount,
    onLoadMore,
  };
}

interface UseToolkitsListDataResult {
  readonly data: readonly ToolkitListItem[];
  /** The rows of this list's kind loaded so far, before the type filter and the search. */
  readonly scopedCount: number;
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
function useToolkitsListData(projectId: string | undefined, isMCP: boolean, isPublicProjectFlag: boolean, query: string): UseToolkitsListDataResult {
  const [selectedTypes, setSelectedTypes] = useState<readonly string[]>([]);
  const paged = usePagedToolkitRows(projectId, isMCP);

  const scopedRows = useMemo(() => paged.rows.filter((row) => isMcpToolkit(row) === isMCP), [paged.rows, isMCP]);
  const filteredRows = useMemo(
    () => (selectedTypes.length === 0 ? scopedRows : scopedRows.filter((row) => selectedTypes.includes(row.type))),
    [scopedRows, selectedTypes],
  );
  const data = useMemo(() => filteredRows.map(toListItem).filter((item) => matchesQuery(item, query)), [filteredRows, query]);
  const tagList = useMemo(() => toTagList(scopedRows), [scopedRows]);

  const onSelectType = useCallback((typeName: string) => {
    setSelectedTypes((prev) => (prev.includes(typeName) ? prev.filter((type) => type !== typeName) : [...prev, typeName]));
  }, []);
  const onClearTypes = useCallback(() => setSelectedTypes([]), []);

  const isRedirectEligible = shouldRedirectToCreatePage({
    isPublicProject: isPublicProjectFlag,
    isLoading: paged.isLoading,
    isError: paged.isError,
    hasMoreRawPages: paged.hasMoreRawPages,
    scopedItemCount: scopedRows.length,
    selectedTypesCount: selectedTypes.length,
  });

  return {
    data,
    scopedCount: scopedRows.length,
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

/** One toolkit/MCP card: the shared `EntityCard`, with the toolkit's `type` in the chip slot (and the table's "Type" column). */
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
 * `/toolkits/:tab` and `/mcps/:tab` (ROUTE-030 family), ported from
 * `apps/elitea-ui/src/pages/Toolkits/Toolkits.jsx`; `isMCP` picks the surface.
 *
 * The list (ADR-0051 M1) has no toolbar row: search, New, the types filter,
 * the author card and the folders are in the right sidebar's List tab, and a
 * summary bar above the list keeps the search and the active types on screen
 * while that sidebar is closed. Rows are selectable, for Delete.
 *
 * This page talks to `useListToolkitInstances` directly: `no-deep-slice-import`
 * keeps `features/toolkits`' loader out of reach, and the endpoint has only
 * `limit`/`offset`, so the search and the types filter run CLIENT-SIDE over the
 * rows loaded so far. `tagList` is derived from those rows' raw `type` strings,
 * not the baseline's per-type schema-derived label (see `toTagList`).
 */
export function Toolkits({ isMCP = false }: ToolkitsProps): ReactNode {
  const copy = toolkitsCopy(isMCP);

  const navigate = useNavigate();
  const projectId = useSelectedProjectId();
  const isPublic = isPublicToolkitsProject(projectId);
  const permissions = usePermissionSet(projectId);
  const canCreate = permissions.has(PERMISSIONS.toolkits.create) && !isPublic;
  const canDelete = permissions.has(PERMISSIONS.toolkits.delete) && permissions.has(PERMISSIONS.applications.delete);
  const kind = isMCP ? 'mcp' : 'toolkit';
  const folderFilter = useFolderFilter(isPublic ? undefined : projectId, kind);
  const query = useListSearchQuery();

  const list = useToolkitsListData(projectId, isMCP, isPublic, query);
  const rows = useFolderRows(list.data, folderFilter, list);

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

  const createAction = canCreate ? { label: copy.createLabel, testId: copy.createTestId, onClick: handleCreateClick } : undefined;

  const selection = useToolkitSelection(projectId, rows);
  const { selected } = selection;

  return (
    <EntityFolderDnd
      projectId={isPublic ? undefined : projectId}
      kind={kind}
      permissions={permissions}
      selectedIds={selected.ids}
    >
    <Box sx={pageSx}>
      <ToolkitsSidebar
        isMCP={isMCP}
        projectId={projectId}
        isPublic={isPublic}
        canDelete={canDelete}
        permissions={permissions}
        createAction={createAction}
        selection={selection}
        types={{ tagList: list.tagList, selected: list.selectedTypes, onSelect: list.onSelectType, onClear: list.onClearTypes, isLoading: list.isLoading }}
      />
      <Box sx={pageBodySx}>
        <Box data-testid={copy.listTestId}>
          <EntitySelectionProvider value={selected.selection}>
            <EntityFolderScope
            projectId={isPublic ? undefined : projectId}
            kind={kind}
            permissions={permissions}
          >
            <ToolkitsList
              sx={listSx}
              data={rows}
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
                totalCount: folderFilter.active ? rows.length : list.totalCount,
              }}
              query={query}
              isMCP={isMCP}
              {...(folderFilter.active
                ? { emptyListPlaceHolder: <EmptyFolderNote /> }
                : { emptyStateConfig: { title: copy.emptyTitle, description: copy.emptyDescription, onCreateClick: handleCreateClick } })}
            />
          </EntityFolderScope>
          </EntitySelectionProvider>
        </Box>
      </Box>
      <DeleteEntityModal
        open={selection.confirmingDelete}
        name={t('pages.toolkits.toolkits.deleteSelection', '{{count}} selected', { count: selected.ids.length })}
        confirming={selection.deleting}
        onClose={() => selection.setConfirmingDelete(false)}
        onConfirm={() => void selection.deleteSelected()}
      />
    </Box>
    </EntityFolderDnd>
  );
}
