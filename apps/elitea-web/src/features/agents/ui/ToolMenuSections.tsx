import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useState } from 'react';

import { useNavigate, useRouterState } from '@tanstack/react-router';

import { isMcpToolkit } from '@/entities/toolkit';
import type { Toolkit } from '@/entities/toolkit';
import type { Application } from '@/shared/api/generated/model';
import { SearchParams } from '@/shared/lib/params';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { PlusIcon } from '@/shared/ui/icons/plus-icon';

import type { AssociationCandidate } from '../api/useAgentPipelineAssociation';
import { INSTANCE_PAGE_SIZE, useToolkitInstancePager } from '../api/useToolkitInstancePager';

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

/** `Application` row -> the `{data: {id}}` shape `useFilterAddedItems`'s `filterAgents`/`filterPipelines` match against. */
function toEntityMenuItem(app: Application): { readonly data: { readonly id: string }; readonly app: Application } {
  return { data: { id: app.id }, app };
}

/* ── toolkit instances (one server-filtered cursor per Toolkit/MCP dropdown) ── */

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
  // The server already filters by `mcp`, drops `application` rows and matches the
  // text; these guards stay as defence. The text match (name OR description, like
  // the server's) keeps the `keepPreviousData` rows shown while a new search
  // loads consistent with the new text.
  return rows
    .filter(
      (toolkit) =>
        toolkit.type !== 'application' &&
        isMcpToolkit(toolkit) === isMcp &&
        !addedToolkitIds.has(toolkit.id) &&
        (toolkit.name.toLowerCase().includes(lowerSearch) || (toolkit.description ?? '').toLowerCase().includes(lowerSearch)),
    )
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
  /** The section pages its own server-filtered cursor (see {@link useToolkitInstancePager}) for this project. */
  readonly projectId: string | undefined;
  readonly addedToolkitIds: ReadonlySet<string | number>;
  readonly onAttach: ((toolkit: Toolkit) => void) | undefined;
  readonly createRoute: '/toolkits/create' | '/mcps/create';
  /** The current agent/pipeline's numeric id — sent as `SearchParams.SourceApplicationId` on "Create new" navigation (see this file's module doc comment). `undefined` while the entity is unsaved, but `onCreateNew` can only fire once the "Create new" menu item is reachable, which requires a saved entity (`isEntityUnsaved` disables the add button itself). */
  readonly sourceApplicationId: number | undefined;
}

export function InstanceAddSection({ copy, testId, isEntityUnsaved, tooltip, isMcp, projectId, addedToolkitIds, onAttach, createRoute, sourceApplicationId }: InstanceSectionProps): ReactNode {
  const navigate = useNavigate();
  const currentHref = useRouterState({ select: (routerState) => routerState.location.href });
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const [search, setSearch] = useState('');
  const closeAnchor = useCallback(() => setAnchor(null), []);
  const close = useCallback(() => {
    setAnchor(null);
    setSearch('');
  }, []);

  // `search` is already debounced: `SimpleSearchBar` (inside the dropdown) only
  // calls `onSearchChange` ~300 ms after the last keystroke.
  const { rows, isFetching, hasMore, fetchMore } = useToolkitInstancePager(projectId, { mcp: isMcp, query: search });
  const items = useMemo(
    () => buildInstanceItems(rows, addedToolkitIds, isMcp, search, onAttach, closeAnchor),
    [rows, addedToolkitIds, isMcp, search, onAttach, closeAnchor],
  );

  // Keep paging while the OPEN dropdown holds less than a page of items. The
  // scroll-near-end trigger (below) cannot fire until the list overflows the
  // 23.3rem paper, and rows are ≥ 36px tall under ~85px of search box and
  // "Create new" row, so about 8 rows already overflow it; a full page (20)
  // always does. Rows the server sent but the client drops (already attached)
  // can leave the list short, hence this check on the VISIBLE count rather than
  // on the fetched count. Terminates: each `fetchMore` advances the offset and
  // `hasMore` goes false at the end of the listing. Gated on `anchor` so a
  // closed section never pages in the background.
  useEffect(() => {
    if (anchor === null || isFetching || !hasMore || items.length >= INSTANCE_PAGE_SIZE) return;
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
