import type { ReactNode } from 'react';
import { useEffect, useMemo, useRef } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { isMcpToolkit, toolkitDisplayName as entityToolkitDisplayName } from '@/entities/toolkit';
import { t } from '@/shared/i18n';
import { uniqueArrayByProp } from '@/shared/lib/array';

import { useIsMcpVisible } from '../../api/useIsMcpVisible';
import { ToolkitTypesPanel } from './ToolkitTypesPanel';
import type { ToolkitTypeTag } from './ToolkitTypesPanel';
import { ToolkitsEmptyListPlaceHolder } from './ToolkitsEmptyListPlaceHolder';
import { ToolkitsEmptyState } from './ToolkitsEmptyState';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/toolkits/ui/list/
 * ToolkitsList.jsx` (Wave-2 unit A4e).
 *
 * MAJOR DISCLOSED REDESIGN — the baseline's list-rendering SHELL
 * (`components/CardList.jsx`) is not ported. Traced in full: it composes
 * `widgets/data-table` (`DataTable`), `./CardList/{DataCards,RightPanel}`,
 * `components/EmptyListBox`, `hooks/useIsTableView`,
 * `hooks/useShouldCollapseRightToolbar` — a whole grid/table dual-view +
 * virtualization subsystem used by every domain's list page (agents,
 * pipelines, credentials, skills…), not just toolkits. NONE of it exists
 * anywhere in this app yet, none of it is this sub-unit's owned scope (only
 * `ToolkitsList.jsx` itself is), and it has no promoted `entities/`/
 * `shared/` home. This component instead owns a real, self-contained,
 * SIMPLER rendering shell:
 *  - **Cards only, no table view.** `useIsTableView`/`DataTable` are not
 *    ported — this list always renders as a card grid. `@mui/x-data-grid`
 *    IS a dependency of this app (`package.json`), so a future toolkits
 *    sub-unit can add table-view support on top of this shell without
 *    reworking its data/props contract.
 *  - **Caller-supplied `renderCard`.** The baseline's `useCardList(viewMode)`
 *    (also not ported — not this sub-unit's file, and itself a thin
 *    strategy-pattern wrapper with no real logic of its own worth
 *    duplicating) is replaced by a required `renderCard` prop, same
 *    "caller supplies the concrete rendering, this component owns layout"
 *    convention `entities/application-form`'s slot components already use.
 *  - **A real, working infinite-scroll sentinel** (`IntersectionObserver`)
 *    replaces `DataCards`' internal virtualized-scroll implementation —
 *    functionally equivalent (calls `onLoadMore` once the sentinel enters
 *    the viewport, gated the same way the baseline's own `loadMore`
 *    callback is: `hasMore && !isLoadingMore`), not virtualized.
 *  - **No `TeamMates`/`AuthorInformation`.** Neither is ported (no home,
 *    out of scope) — `rightPanelExtra` is an injected `ReactNode` slot a
 *    page-level caller fills with whichever of the two applies.
 *  - **No `useQueryTrendingAuthor`/navigate-away-when-empty effect.**
 *    Route-derived concerns move to the caller per this session's
 *    established "page/route-layer caller reads its own params" convention
 *    (`../../lib/hooks/useLoadToolkits.ts`'s own doc comment, point 3).
 *    `shouldRedirectToCreatePage` below is the pure decision the baseline's
 *    `useEffect` made, exported for a page-layer caller's own
 *    `useEffect`+`navigate()`.
 *  - **No internal `useToast()` error effect.** `isError`/`error` are
 *    surfaced as props for the caller to render/toast — this app has no
 *    real toast implementation yet (grepped, zero call sites anywhere).
 */
export interface ToolkitListItem {
  readonly id: string;
  /** Baseline rows always carry a `name` field (possibly blank/whitespace) — `toolkitDisplayName` below resolves the blank case, matching `LoadedToolkit`'s (`../../lib/hooks/useLoadToolkits.ts`) own required-`name` shape. */
  readonly name: string;
  readonly type: string;
  readonly toolkit_name?: string;
  readonly settings?: {
    readonly elitea_title?: string;
    readonly configuration_title?: string;
  };
  readonly [key: string]: unknown;
}

/**
 * Baseline `getToolkitItemName` (`ToolkitsList.jsx:134-144`) — the
 * display-name fallback chain for a row with a blank `name`. Delegates to
 * `entities/toolkit`'s `toolkitDisplayName` (the single owner of this
 * fallback chain — no local re-implementation) rather than duplicating the
 * logic here.
 */
export function toolkitDisplayName(item: ToolkitListItem): string {
  return entityToolkitDisplayName(item);
}

/** Baseline `uniqueDataList` (`ToolkitsList.jsx:133-157`): drops MCP rows when MCPs are not visible, resolves each row's display name, then dedupes by `id`. */
export function dedupeToolkitsForDisplay(data: readonly ToolkitListItem[] | undefined, isMcpVisible: boolean): ToolkitListItem[] {
  const items = (data ?? [])
    .filter((item) => isMcpVisible || !isMcpToolkit(item))
    .map((item) => ({ ...item, name: toolkitDisplayName(item) }));
  return uniqueArrayByProp(items, 'id');
}

/** Baseline navigate-to-create-page condition (`ToolkitsList.jsx:97-131`) — a PURE decision; the caller performs the actual navigation (see module doc comment). */
export function shouldRedirectToCreatePage(input: {
  readonly isPublicProject: boolean;
  readonly isLoading: boolean;
  readonly isError: boolean;
  readonly disableEmptyRedirect: boolean;
  readonly hasQuery: boolean;
  readonly totalCount: number;
  readonly selectedTypesCount: number;
}): boolean {
  const { isPublicProject, isLoading, isError, disableEmptyRedirect, hasQuery, totalCount, selectedTypesCount } = input;
  return !isPublicProject && !isLoading && !isError && !disableEmptyRedirect && !hasQuery && totalCount === 0 && selectedTypesCount === 0;
}

// Not exported: nothing outside this module names the type (it is reached
// through the two prop interfaces below). An `export` here is dead surface and
// the dead-code gate counts it as such.
interface ToolkitsEmptyStateConfig {
  readonly title: string;
  readonly description: string;
  readonly onCreateClick: () => void;
}

/** Pure derivation, split out of the component body to keep its own cyclomatic complexity under the §3.5 budget (also independently unit-testable). */
export function isListEmpty(isLoading: boolean, isError: boolean, itemCount: number): boolean {
  return !isLoading && !isError && itemCount === 0;
}

/**
 * Baseline fidelity fix (parity-review finding R4): the old app's
 * `CardList.jsx` computes `showCustomEmptyState = showEmptyOrError &&
 * customEmptyState && !isError` — since `ToolkitsList.jsx` (the baseline
 * caller) ALWAYS passes a non-null `customEmptyState`, that condition
 * reduces to just "the list is empty and not erroring", with NO dependency
 * on `query`/type-filter/`totalCount` at all. A previous version of this
 * function additionally gated on `!query && totalCount === 0 &&
 * selectedTypesCount === 0`, which made the "No toolkits yet" CTA lose to
 * `ToolkitsEmptyListPlaceHolder`'s plain "Nothing found" text whenever a
 * search query or an active type filter was present — a real content
 * divergence from `CardList.jsx`'s own always-wins precedence (see
 * `ToolkitsList.test.tsx`'s own regression test for the exact scenario this
 * fixes). `totalCount`/`selectedTypesCount` are no longer read here at all
 * — same "genuinely nothing" signal `isListEmpty`'s own `empty` already is.
 */
export function isZeroStateEligible(empty: boolean): boolean {
  return empty;
}

/** Same reason as {@link isListEmpty}: the cards-grid branch's own gate. */
export function shouldShowCards(isLoading: boolean, isError: boolean, itemCount: number): boolean {
  return !isLoading && !isError && itemCount > 0;
}

interface ToolkitsListEmptyAreaProps {
  readonly zeroStateEligible: boolean;
  readonly emptyStateConfig: ToolkitsEmptyStateConfig | undefined;
  readonly emptyListPlaceHolder: ReactNode;
  readonly query: string | undefined;
  readonly isMCP: boolean;
}

/** The "which empty view" decision, split out of the main component for the same §3.5 reason. */
function ToolkitsListEmptyArea({ zeroStateEligible, emptyStateConfig, emptyListPlaceHolder, query, isMCP }: ToolkitsListEmptyAreaProps): ReactNode {
  if (zeroStateEligible && emptyStateConfig) {
    return <ToolkitsEmptyState {...emptyStateConfig} />;
  }
  if (emptyListPlaceHolder !== undefined) {
    return emptyListPlaceHolder;
  }
  return (
    <ToolkitsEmptyListPlaceHolder
      {...(query !== undefined && { query })}
      isMCP={isMCP}
    />
  );
}

/**
 * Loading/pagination state, grouped into one prop — `ToolkitsListProps`
 * would otherwise declare 18 top-level props, over the §3.5 `component-
 * props` budget (12). Mirrors `useLoadToolkits.ts`'s own
 * `UseLoadToolkitsResult` shape closely enough that a caller can spread
 * most of that hook's return value straight in.
 *
 * NOT exported (module-private): `index.ts`'s own doc comment already
 * covers why — a caller's `listState={{...}}` object literal is checked
 * structurally against `ToolkitsListProps['listState']`, with no import of
 * this interface's NAME required either at this module's boundary or at
 * the public-API one.
 */
interface ToolkitsListState {
  readonly isLoading: boolean;
  readonly isFetchingMore?: boolean;
  readonly isError?: boolean;
  readonly hasMore: boolean;
  readonly onLoadMore: () => void;
  readonly totalCount: number;
}

/** The right-panel type-filter wiring, grouped for the same §3.5 reason as {@link ToolkitsListState} (and NOT exported for the same reason). */
interface ToolkitsListTypeFilter {
  readonly tagList: readonly ToolkitTypeTag[];
  readonly selectedTypes: readonly string[];
  readonly onSelectType: (typeName: string) => void;
  readonly onClearTypes: () => void;
}

export interface ToolkitsListProps {
  readonly data: readonly ToolkitListItem[] | undefined;
  readonly renderCard: (toolkit: ToolkitListItem) => ReactNode;
  readonly listState: ToolkitsListState;
  readonly typeFilter: ToolkitsListTypeFilter;
  readonly query?: string;
  readonly isMCP?: boolean;
  readonly emptyListPlaceHolder?: ReactNode;
  readonly emptyStateConfig?: ToolkitsEmptyStateConfig;
  readonly rightPanelExtra?: ReactNode;
  readonly sx?: SxProps<Theme>;
}

export function ToolkitsList({
  data,
  renderCard,
  listState,
  typeFilter,
  query,
  isMCP = false,
  emptyListPlaceHolder,
  emptyStateConfig,
  rightPanelExtra,
  sx,
}: ToolkitsListProps): ReactNode {
  const { isLoading, isFetchingMore = false, isError = false, hasMore, onLoadMore } = listState;
  const { tagList, selectedTypes, onSelectType, onClearTypes } = typeFilter;

  const isMcpVisible = useIsMcpVisible();
  const items = useMemo(() => dedupeToolkitsForDisplay(data, isMcpVisible), [data, isMcpVisible]);

  const sentinelRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    const node = sentinelRef.current;
    if (!node || !hasMore || isFetchingMore) return undefined;

    const observer = new IntersectionObserver((entries) => {
      if (entries.some((entry) => entry.isIntersecting)) onLoadMore();
    });
    observer.observe(node);
    return () => observer.disconnect();
  }, [hasMore, isFetchingMore, onLoadMore]);

  const isEmpty = isListEmpty(isLoading, isError, items.length);
  const zeroStateEligible = isZeroStateEligible(isEmpty);
  const showCards = shouldShowCards(isLoading, isError, items.length);

  return (
    <Box sx={combineListSx(sx)}>
      <Box sx={gridContainerSx}>
        {isLoading && (
          <Box sx={centeredSx}>
            <CircularProgress size={32} />
          </Box>
        )}
        {isError && !isLoading && (
          <Box sx={centeredSx}>
            <Typography variant="body2">{t('features.toolkits.toolkitsList.loadError', 'Failed to load toolkits.')}</Typography>
          </Box>
        )}
        {isEmpty && (
          <ToolkitsListEmptyArea
            zeroStateEligible={zeroStateEligible}
            emptyStateConfig={emptyStateConfig}
            emptyListPlaceHolder={emptyListPlaceHolder}
            query={query}
            isMCP={isMCP}
          />
        )}
        {showCards && (
          <Box sx={cardsGridSx}>
            {items.map((item) => (
              <Box
                key={item.id}
                sx={cardSlotSx}
              >
                {renderCard(item)}
              </Box>
            ))}
          </Box>
        )}
        {hasMore && (
          <Box
            ref={sentinelRef}
            sx={sentinelSx}
          >
            {isFetchingMore && <CircularProgress size={20} />}
          </Box>
        )}
      </Box>
      <Box sx={rightPanelContainerSx}>
        <ToolkitTypesPanel
          tagList={tagList}
          selectedTypes={selectedTypes}
          onSelectType={onSelectType}
          onClear={onClearTypes}
          sx={rightPanelSx}
        />
        {rightPanelExtra}
      </Box>
    </Box>
  );
}

function combineListSx(sx: SxProps<Theme> | undefined): SxProps<Theme> {
  const base: SxProps<Theme> = { display: 'flex', width: '100%' };
  return sx === undefined ? base : ([base, sx] as SxProps<Theme>);
}

const gridContainerSx: SxProps<Theme> = { flex: 1, minWidth: 0 };

const centeredSx: SxProps<Theme> = { display: 'flex', justifyContent: 'center', padding: '2rem' };

/**
 * `DataCards.jsx`'s grid geometry, matched to `shared/ui/EntityCardList`'s
 * own: `1.25rem 0 0 1.5rem` padding, a `1rem` gap and `MIN_CARD_WIDTH`
 * (300px) tracks, so a toolkit grid lines up with an agent/pipeline/skill
 * one column-for-column at the same viewport.
 */
const cardsGridSx: SxProps<Theme> = {
  display: 'grid',
  gridTemplateColumns: 'repeat(auto-fill, minmax(300px, 1fr))',
  gap: '1rem',
  padding: '1.25rem 0 0 1.5rem',
  marginRight: '-1rem',
  boxSizing: 'border-box',
  width: '100%',
};

const cardSlotSx: SxProps<Theme> = { minWidth: 0 };

const sentinelSx: SxProps<Theme> = { display: 'flex', justifyContent: 'center', padding: '1rem' };

const rightPanelContainerSx: SxProps<Theme> = { height: '100dvh', display: 'flex', flexDirection: 'column' };

const rightPanelSx: SxProps<Theme> = { flex: 1 };
