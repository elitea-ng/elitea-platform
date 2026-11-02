import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Skeleton from '@mui/material/Skeleton';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { useLocationSearch } from '@/shared/lib/hooks/useLocationSearch';

import { EntityCard } from './EntityCard';
import { EntityListPagination } from './EntityListPagination';
import { EntityListTable } from './EntityListTable';
import { ENTITY_LIST_VIEW, type EntityListItem, type EntityListTag, type EntityListView } from './model';

/**
 * The list surface every entity page shares: the card grid (or, on
 * `?view=table`, the table), its loading skeletons, its empty/error state,
 * and its pagination footer. The port of
 * `apps/elitea-ui/src/components/CardList.jsx` + `DataCards.jsx`.
 *
 * Geometry: a `1rem` gap, and the page toolbar's own `1.5rem` side gutter
 * (`widgets/page-header`), so the first column lines up under the tabs and
 * the last one ends where the toolbar's primary action does. The grid takes
 * the page's full width: the filters that used to float over its right edge
 * (the baseline reserved `calc(100% - 328px)` for them) are in the shell's
 * side panel now, beside the page rather than on top of it.
 *
 * The column count is CSS rather than the baseline's measured-width
 * arithmetic (`hooks/useCardLayout.js` + a `ResizeObserver`):
 * `repeat(auto-fill, minmax(300px, 1fr))` over the same `MIN_CARD_WIDTH`
 * lands within 4px of it — measured against live production
 * (next.elitea.ai at a 2000px viewport: a 1408px content box, four 336px
 * columns; this grid gives four 340px ones) — with no measurement pass and
 * no first-paint reflow.
 */
const SKELETON_COUNT = 6;

export interface EntityCardListProps {
  readonly items: readonly EntityListItem[];
  readonly isLoading?: boolean;
  readonly isError?: boolean;
  readonly errorMessage?: string;
  /** Shown in place of the grid when there is nothing to list — usually an `EntityEmptyState`. */
  readonly emptyState?: ReactNode;
  /** Forces a view; by default the `?view=` search param decides (`ViewToggle` writes it). */
  readonly view?: EntityListView;
  readonly onTagClick?: (tag: EntityListTag) => void;
  readonly pagination?: {
    readonly page: number;
    readonly pageSize: number;
    readonly total: number;
    readonly onPageChange: (page: number) => void;
    readonly onPageSizeChange: (pageSize: number) => void;
  };
  /** Per-card top-right slot (publish/pin/status affordances). */
  readonly renderCardActions?: (item: EntityListItem) => ReactNode;
}

/**
 * `hooks/useIsTableView.js` — `?view=table` is the only value that switches
 * away from cards.
 *
 * Read off `window.location` rather than the router's `useSearch`: this grid
 * is `shared/ui` and is rendered by page components whose own unit tests
 * mount them WITHOUT a `RouterProvider` (`useSearch` throws
 * "Cannot read properties of null (reading 'stores')" there, and a hook that
 * throws mid-render cannot be recovered from with try/catch without
 * corrupting React's hook order).
 *
 * SUBSCRIBED, not read at render time. The earlier version of this function
 * read `window.location.search` directly and assumed "every render that could
 * observe a new `view` re-reads it" — which was wrong, and inert in exactly
 * the case the control exists for. A search-only navigation re-renders the
 * components that subscribed to the router; the list body is not one of them,
 * so the toggle in the page HEADER took `aria-pressed="true"` while the panel
 * below kept drawing cards. `useLocationSearch` subscribes to the same value
 * instead (`shared/lib/hooks/useLocationSearch.ts`), so the read happens again
 * when the URL moves.
 */
function useSearchView(): EntityListView {
  const search = useLocationSearch();
  const value = new URLSearchParams(search).get('view');
  return value === ENTITY_LIST_VIEW.table ? ENTITY_LIST_VIEW.table : ENTITY_LIST_VIEW.cards;
}

export function EntityCardList(props: EntityCardListProps): ReactNode {
  return (
    <Box sx={containerSx}>
      <ListStates {...props} />
    </Box>
  );
}

/** The four mutually-exclusive states, as early returns — kept out of {@link EntityCardList} for the §3.5 complexity budget. */
function ListStates({ items, isLoading = false, isError = false, errorMessage, emptyState, ...rest }: EntityCardListProps): ReactNode {
  if (isLoading) return <ListSkeletons />;
  if (isError) {
    return (
      <Box sx={centeredSx}>
        <Typography
          variant="bodyMedium"
          role="alert"
          sx={errorSx}
        >
          {errorMessage ?? t('shared.entityList.loadFailed', 'Oops! Something went wrong. Please try again later!')}
        </Typography>
      </Box>
    );
  }
  if (items.length === 0) return emptyState ?? null;
  return (
    <ListRows
      items={items}
      {...rest}
    />
  );
}

type ListRowsProps = Omit<EntityCardListProps, 'isLoading' | 'isError' | 'errorMessage' | 'emptyState'>;

/** The populated list itself: the `?view=`-selected rendering plus the optional pagination footer. */
function ListRows({ items, view, onTagClick, pagination, renderCardActions }: ListRowsProps): ReactNode {
  const searchView = useSearchView();
  const isTable = (view ?? searchView) === ENTITY_LIST_VIEW.table;
  return (
    <>
      {isTable ? (
        <Box sx={tableScrollSx}>
          <EntityListTable items={items} />
        </Box>
      ) : (
        <Box sx={gridSx}>
          {items.map((item) => (
            <EntityCard
              key={item.id}
              item={item}
              {...(renderCardActions === undefined ? {} : { actions: renderCardActions(item) })}
              {...(onTagClick === undefined ? {} : { onTagClick })}
            />
          ))}
        </Box>
      )}
      {pagination !== undefined && (
        <EntityListPagination
          page={pagination.page}
          pageSize={pagination.pageSize}
          total={pagination.total}
          onPageChange={pagination.onPageChange}
          onPageSizeChange={pagination.onPageSizeChange}
        />
      )}
    </>
  );
}

/** `DataCards.jsx`'s `renderSkeletons` — one card-shaped placeholder per grid slot while the first page loads. */
function ListSkeletons(): ReactNode {
  return (
    <Box sx={gridSx}>
      {Array.from({ length: SKELETON_COUNT }, (_, index) => (
        <Skeleton
          key={index}
          data-testid="entity-card-skeleton"
          animation="wave"
          variant="rectangular"
          sx={skeletonSx}
        />
      ))}
    </Box>
  );
}

/** `MIN_CARD_WIDTH` (`common/constants.js:352`) — the track floor `auto-fill` counts columns against. */
const MIN_CARD_WIDTH = '300px';

const containerSx: SxProps<Theme> = {
  boxSizing: 'border-box',
  display: 'flex',
  flexDirection: 'column',
  minWidth: 0,
  flex: 1,
  width: '100%',
  padding: '1.25rem 1.5rem 1.5rem',
};

const gridSx: SxProps<Theme> = {
  display: 'grid',
  gridTemplateColumns: `repeat(auto-fill, minmax(${MIN_CARD_WIDTH}, 1fr))`,
  gap: '1rem',
  width: '100%',
  minWidth: 0,
};

const skeletonSx: SxProps<Theme> = (theme: Theme) => ({ width: '100%', height: '7rem', borderRadius: theme.vars.shape.radiusLg });

const centeredSx: SxProps<Theme> = { display: 'flex', justifyContent: 'center', padding: '2rem' };

const errorSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.primary, textAlign: 'center' });

const tableScrollSx: SxProps<Theme> = { width: '100%', minWidth: 0, overflowX: 'auto' };
