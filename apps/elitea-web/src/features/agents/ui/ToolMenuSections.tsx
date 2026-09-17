import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useState } from 'react';

import { useInfiniteQuery } from '@tanstack/react-query';
import { useNavigate, useRouterState } from '@tanstack/react-router';

import { isMcpToolkit } from '@/entities/toolkit';
import type { Toolkit } from '@/entities/toolkit';
import { getListToolkitInstancesQueryKey, listToolkitInstances } from '@/shared/api/generated/toolkits/toolkits';
import { unwrapListPage } from '@/shared/api/unwrap';
import type { Application } from '@/shared/api/generated/model';
import { SearchParams } from '@/shared/lib/params';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { PlusIcon } from '@/shared/ui/icons/plus-icon';

import type { AssociationCandidate } from '../api/useAgentPipelineAssociation';

import { EntityIcon } from './EntityIcon';
import { ToolMenuDropdown } from './ToolMenuDropdown';
import type { ToolMenuDropdownItem } from './ToolMenuDropdown';

/**
 * `ToolMenu.tsx`'s section subcomponents and their pure/data-fetching
 * helpers — split into this sibling file purely to keep `ToolMenu.tsx`
 * itself under the §3.5 400-line-per-file budget (a single-file shape was
 * 488 lines). See `ToolMenu.tsx`'s own module doc comment for the full
 * porting/gap disclosure this file is part of.
 *
 * **"Create new toolkit" round trip — outbound half.** `InstanceAddSection`
 * (below) is the baseline's `handleCreateNewToolkit`
 * (`ToolMenu.jsx:302-337`): navigating to the toolkit/MCP creation route
 * with `SearchParams.SourceApplicationId`/`SearchParams.ReturnUrl` (this
 * screen's own href, so the creation page can send the user back) wired on.
 * `SearchParams.IsMCP` (`mcp`) is also set for the MCP variant, matching the
 * baseline's `currentParams.set(SearchParams.IsMCP, 'true')`. Both param
 * keys are real, already-registered `paramSchemas` entries
 * (`src/routes/-search/params.ts`) that `/agents/$tab`/`/agents/$tab/
 * $agentId` already declare in their `validateSearch` — this file spends
 * them, it does not invent them. The RETURN half (the not-yet-built
 * toolkit-creation page reading these two params and appending
 * `?newToolkitId=`/`?mcp=` on its way back) is out of this unit's ownership
 * fence; `ToolMenu.tsx`'s own module doc comment covers the inbound half
 * this file's sibling owns.
 */

function toToolkitInstance(row: unknown): Toolkit {
  return row as Toolkit;
}

/** `Application` row -> the `{data: {id}}` shape `useFilterAddedItems`'s `filterAgents`/`filterPipelines` match against. */
function toEntityMenuItem(app: Application): { readonly data: { readonly id: string }; readonly app: Application } {
  return { data: { id: app.id }, app };
}

/* ── toolkit instances (shared source for the Toolkit and MCP dropdowns) ──── */

/**
 * One page of `listToolkitInstances` is 20 rows — the same page the baseline's
 * `instanceLimit` started at. Kept SMALL, not raised to the server's 100-row
 * ceiling, on purpose: paging is OFFSET-based here (`offset = page *
 * INSTANCE_PAGE_SIZE`), so `limit` stays a constant 20 and never trips the
 * handler's `limit > 100 → reset to 20` clamp
 * (`internal/api/v2/toolkits/handler.go:787-789`) the way a growing single
 * `limit` would past 100 rows. Offset itself has no such ceiling, so this
 * pages the whole `elitea_tools` listing however large it grows.
 */
const INSTANCE_PAGE_SIZE = 20;

/**
 * A cursor over the project's toolkit-instance listing that BOTH the Toolkit
 * and the MCP dropdown draw from.
 *
 * The listing endpoint has no server-side type or name filter — only
 * `limit`/`offset`, ordered by name (`handler.go:781-803`, `ListToolkits`
 * `:1196-1246`) — so a section whose rows sort past the first page (e.g. the
 * MCP section on a project whose first 20 toolkits are all non-MCP) cannot be
 * reached by filtering one already-fetched page. The fix is to keep PAGING
 * until the section that needs a row has one; `hasMore`/`fetchMore` expose that
 * cursor and each `InstanceAddSection` drives it independently against its OWN
 * filtered emptiness (see the auto-page effect there). A single shared
 * infinite query — rather than one query per section — because with no
 * server-side type filter both sections would otherwise fetch the identical
 * rows twice; here a page fetched to surface an MCP is immediately available to
 * the Toolkit section too, and react-query dedupes the fetches.
 */
export interface ToolkitInstancePager {
  readonly rows: readonly Toolkit[];
  readonly isFetching: boolean;
  readonly hasMore: boolean;
  readonly fetchMore: () => void;
}

export function useToolkitInstancePager(projectId: string | undefined): ToolkitInstancePager {
  const query = useInfiniteQuery({
    queryKey: [...getListToolkitInstancesQueryKey(projectId ?? ''), 'pager'] as const,
    enabled: projectId !== undefined,
    initialPageParam: 0,
    queryFn: async ({ pageParam }) =>
      unwrapListPage<unknown>(
        await listToolkitInstances(projectId ?? '', { limit: INSTANCE_PAGE_SIZE, offset: pageParam * INSTANCE_PAGE_SIZE }),
        'listToolkitInstances',
      ),
    getNextPageParam: (lastPage, allPages) => {
      // A short/empty page means the server has nothing more to give, even if
      // its reported `total` disagrees — stop rather than re-request the same
      // exhausted offset forever.
      if (lastPage.rows.length === 0) return undefined;
      const fetched = allPages.reduce((sum, page) => sum + page.rows.length, 0);
      return fetched < lastPage.total ? allPages.length : undefined;
    },
  });

  // useMemo, not a bare expression: this flattened array is a prop/dep
  // downstream, and `flatMap`/`map` return FRESH arrays each render.
  const rows = useMemo(
    () => (query.data?.pages ?? []).flatMap((page) => page.rows).map(toToolkitInstance),
    [query.data],
  );
  const fetchMore = useCallback(() => {
    void query.fetchNextPage();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `query.fetchNextPage` is a stable TanStack Query identity per query key
  }, [query.fetchNextPage]);

  return { rows, isFetching: query.isFetching, hasMore: query.hasNextPage, fetchMore };
}

// elitea_issues #5296 — exported for its own direct unit test: this is the
// ONE place that builds the Agents-page toolkit-attach dropdown's item
// labels (`label: toolkit.name`), the same raw field the Toolkits page
// itself renders — the two surfaces cannot disagree on spacing/formatting
// because there is no second, slugified label anywhere in this file.
export function buildInstanceItems(
  rows: readonly Toolkit[],
  addedToolkitIds: ReadonlySet<string | number>,
  isMcp: boolean,
  search: string,
  onAttach: ((toolkit: Toolkit) => void) | undefined,
  onClose: () => void,
): readonly ToolMenuDropdownItem[] {
  const lowerSearch = search.toLowerCase();
  return rows
    .filter((toolkit) => isMcpToolkit(toolkit) === isMcp && !addedToolkitIds.has(toolkit.id) && toolkit.name.toLowerCase().includes(lowerSearch))
    .sort((a, b) => a.name.localeCompare(b.name))
    .map((toolkit) => ({
      key: toolkit.id,
      label: toolkit.name,
      description: toolkit.description,
      icon: (
        <EntityIcon
          entityType="toolkit"
          icon={{}}
        />
      ),
      onClick: () => {
        onAttach?.(toolkit);
        onClose();
      },
    }));
}

/* ── agents / pipelines (one parametrized builder instead of two copies) ──── */

function buildEntityIcon(entityType: 'agent' | 'pipeline', app: Application): ReactNode {
  return (
    <EntityIcon
      entityType={entityType}
      icon={app.icon ? { url: app.icon } : {}}
    />
  );
}

export function useEntityAssociationItems(
  rows: readonly Application[],
  excludeId: number | undefined,
  filterItems: <T extends { readonly data?: { readonly id?: string | number | undefined } | undefined }>(items: readonly T[] | undefined) => readonly T[],
  entityType: 'agent' | 'pipeline',
  associate: (candidate: AssociationCandidate) => void,
): readonly ToolMenuDropdownItem[] {
  return useMemo((): readonly ToolMenuDropdownItem[] => {
    const candidates = rows.filter((app) => app.id !== String(excludeId)).map(toEntityMenuItem);
    const available = [...filterItems(candidates)].sort((a, b) => a.app.name.localeCompare(b.app.name));
    return available.map(({ app }) => ({
      key: app.id,
      label: app.name,
      description: app.description,
      icon: buildEntityIcon(entityType, app),
      onClick: () => associate({ id: Number(app.id), name: app.name }),
    }));
  }, [rows, excludeId, filterItems, entityType, associate]);
}

/* ── the repeated "add" button + tooltip pattern ───────────────────────────── */

interface AddMenuButtonProps {
  readonly label: string;
  readonly disabled: boolean;
  readonly tooltip: string;
  readonly onClick: (event: { readonly currentTarget: HTMLElement }) => void;
  readonly testId?: string;
}

function AddMenuButton({ label, disabled, tooltip, onClick, testId }: AddMenuButtonProps): ReactNode {
  return (
    <BaseBtn
      {...(testId !== undefined ? { 'data-testid': testId } : {})}
      variant="iconLabel"
      startIcon={<PlusIcon />}
      disabled={disabled}
      title={disabled ? tooltip : undefined}
      onClick={onClick}
    >
      {label}
    </BaseBtn>
  );
}

/* ── one self-contained section per add-button ─────────────────────────────── */

/** User-visible copy for one `InstanceAddSection`, grouped into its own object to keep the component's own prop count under the §3.5 12-prop budget. */
interface InstanceSectionCopy {
  readonly label: string;
  readonly searchPlaceholder: string;
  readonly emptyMessage: string;
}

export interface InstanceSectionProps {
  readonly copy: InstanceSectionCopy;
  readonly testId?: string;
  readonly isEntityUnsaved: boolean;
  readonly tooltip: string;
  readonly isMcp: boolean;
  /** The shared toolkit-instance cursor (see {@link useToolkitInstancePager}); this section pages it independently against its own filtered emptiness. */
  readonly pager: ToolkitInstancePager;
  readonly addedToolkitIds: ReadonlySet<string | number>;
  readonly onAttach: ((toolkit: Toolkit) => void) | undefined;
  readonly createRoute: '/toolkits/create' | '/mcps/create';
  /** The current agent/pipeline's numeric id — sent as `SearchParams.SourceApplicationId` on "Create new" navigation (see this file's module doc comment). `undefined` while the entity is unsaved, but `onCreateNew` can only fire once the "Create new" menu item is reachable, which requires a saved entity (`isEntityUnsaved` disables the add button itself). */
  readonly sourceApplicationId: number | undefined;
}

export function InstanceAddSection({ copy, testId, isEntityUnsaved, tooltip, isMcp, pager, addedToolkitIds, onAttach, createRoute, sourceApplicationId }: InstanceSectionProps): ReactNode {
  const navigate = useNavigate();
  const currentHref = useRouterState({ select: (routerState) => routerState.location.href });
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const [search, setSearch] = useState('');
  const closeAnchor = useCallback(() => setAnchor(null), []);
  const close = useCallback(() => {
    setAnchor(null);
    setSearch('');
  }, []);

  const { rows, isFetching, hasMore, fetchMore } = pager;
  const items = useMemo(
    () => buildInstanceItems(rows, addedToolkitIds, isMcp, search, onAttach, closeAnchor),
    [rows, addedToolkitIds, isMcp, search, onAttach, closeAnchor],
  );

  // THE FIX. The server cannot filter this listing by type or by name (only
  // `limit`/`offset`, ordered by name), so a section whose rows sort past the
  // first fetched page — the MCP section on a project with 20+ non-MCP toolkits
  // ahead of it, or a name search that matches a row not yet fetched — would
  // otherwise show an empty dropdown forever: `buildInstanceItems` can only
  // filter what is already fetched, and the scroll-to-load-more trigger never
  // fires on a 0–2 row list because the paper does not scroll. So while THIS
  // section's OPEN dropdown has no matching row and the listing still has more
  // pages, page again. It terminates: every `fetchMore` advances the offset and
  // `hasMore` goes false once the listing is exhausted. Gated on `anchor` so a
  // closed section never pages the whole table in the background.
  useEffect(() => {
    if (anchor === null || isFetching || !hasMore || items.length > 0) return;
    fetchMore();
  }, [anchor, isFetching, hasMore, items.length, fetchMore]);

  return (
    <>
      <AddMenuButton
        {...(testId !== undefined ? { testId } : {})}
        label={copy.label}
        disabled={isEntityUnsaved}
        tooltip={tooltip}
        onClick={(event) => setAnchor(event.currentTarget)}
      />
      <ToolMenuDropdown
        anchorEl={anchor}
        onClose={close}
        items={items}
        search={search}
        onSearchChange={setSearch}
        searchPlaceholder={copy.searchPlaceholder}
        isLoading={isFetching}
        emptyMessage={copy.emptyMessage}
        onCreateNew={() => {
          setAnchor(null);
          void navigate({
            to: createRoute,
            search: {
              ...(sourceApplicationId !== undefined ? { [SearchParams.SourceApplicationId]: String(sourceApplicationId) } : {}),
              [SearchParams.ReturnUrl]: currentHref,
              ...(isMcp ? { [SearchParams.IsMCP]: 'true' } : {}),
            },
          });
        }}
        onScrollNearEnd={fetchMore}
      />
    </>
  );
}

/** Same grouping rationale as `InstanceSectionCopy`. */
interface EntitySectionCopy {
  readonly label: string;
  readonly searchPlaceholder: string;
  readonly emptyMessage: string;
}

export interface EntitySectionProps {
  readonly copy: EntitySectionCopy;
  readonly isEntityUnsaved: boolean;
  readonly tooltip: string;
  readonly items: readonly ToolMenuDropdownItem[];
  readonly isFetching: boolean;
  readonly search: string;
  readonly onSearchChange: (value: string) => void;
  readonly onOpen: (anchor: HTMLElement) => void;
  readonly anchor: HTMLElement | null;
  readonly onClose: () => void;
}

export function EntityAddSection({ copy, isEntityUnsaved, tooltip, items, isFetching, search, onSearchChange, onOpen, anchor, onClose }: EntitySectionProps): ReactNode {
  return (
    <>
      <AddMenuButton
        label={copy.label}
        disabled={isEntityUnsaved}
        tooltip={tooltip}
        onClick={(event) => onOpen(event.currentTarget)}
      />
      <ToolMenuDropdown
        anchorEl={anchor}
        onClose={onClose}
        items={items}
        search={search}
        onSearchChange={onSearchChange}
        searchPlaceholder={copy.searchPlaceholder}
        isLoading={isFetching}
        emptyMessage={copy.emptyMessage}
      />
    </>
  );
}
