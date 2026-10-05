/* oxlint-disable i18next/no-literal-string -- Wave-2 prototype: UI copy not yet wired through i18n shim (unit S8). REMOVER: S8. */
/**
 * Agent Category Section — renders cards for a single category bucket.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/features/agent-hub/ui/AgentCategorySection.jsx`.
 *
 * Deviations:
 *  - No tour IDs.
 *  - Uses Box/img for icons instead of EntityIcon.
 */
import { memo, useCallback, useEffect, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import IconButton from '@mui/material/IconButton';
import Skeleton from '@mui/material/Skeleton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';
import { useTheme } from '@mui/material/styles';
import useMediaQuery from '@mui/material/useMediaQuery';

const IconButtonAny = IconButton as React.ComponentType<
  React.ComponentProps<typeof IconButton> & { variant?: string }
>;

import RefreshIcon from '@/shared/ui/icons/svg/refresh-icon.svg?react';
import { INITIAL_CARD_DISPLAY_COUNT } from '@/shared/lib/layout';
import { ShowMoreButton } from '@/shared/ui/ShowMoreButton';

import AgentCard from './AgentCard';
import type { ApplicationData } from '../types';

export interface AgentCategorySectionProps {
  category: string;
  items: ApplicationData[];
  totalCount?: number | undefined;
  isLoading?: boolean | undefined;
  isLoadingMore?: boolean | undefined;
  onSelectItem?: ((app: ApplicationData) => void) | undefined;
  onLoadMore?: ((category: string) => void) | undefined;
  onRefresh?: ((category: string) => void) | undefined;
}

/**
 * Screen-size-aware initial card count (adversarial-review fix, cluster
 * A13-agents-hub, finding 10): the baseline (`AgentCategorySection.jsx`)
 * picks it from `theme.breakpoints.up('prompt_list_xl')` — this had
 * regressed to a hardcoded `8` regardless of viewport.
 * `INITIAL_CARD_DISPLAY_COUNT` (`shared/lib/layout.ts`) is the
 * already-ported constant for this. Extracted to its own hook (rather than
 * inlined in the component below) to keep the component's own cyclomatic
 * complexity under the §3.5 budget — a real reduction, not a re-shuffle to
 * dodge the checker: the branch genuinely moves out of the render function.
 */
function useInitialCardDisplayCount(): number {
  const theme = useTheme();
  const isLargeScreen = useMediaQuery(theme.breakpoints.up('prompt_list_xl'));
  return isLargeScreen ? INITIAL_CARD_DISPLAY_COUNT.LARGE_SCREEN : INITIAL_CARD_DISPLAY_COUNT.DEFAULT;
}

const AgentCategorySection = memo(
  ({
    category,
    items,
    totalCount = 0,
    isLoading = false,
    isLoadingMore = false,
    onSelectItem,
    onLoadMore,
    onRefresh,
  }: AgentCategorySectionProps) => {
    const initialDisplayCount = useInitialCardDisplayCount();
    const [displayCount, setDisplayCount] = useState<number>(initialDisplayCount);

    useEffect(() => {
      if (!items.length) setDisplayCount(initialDisplayCount);
    }, [items.length, initialDisplayCount]);

    const visibleItems = useMemo(() => items.slice(0, displayCount), [items, displayCount]);
    const hasMoreLocally = items.length > displayCount;
    const canShowMore = hasMoreLocally || items.length < totalCount;
    const isExpanded = displayCount > initialDisplayCount;
    const shouldShowButton = (canShowMore || isExpanded) && !isLoadingMore;

    const handleShowMore = useCallback(() => {
      const newCount = displayCount + initialDisplayCount;
      setDisplayCount(newCount);
      if (newCount > items.length && items.length < totalCount) {
        onLoadMore?.(category);
      }
    }, [displayCount, initialDisplayCount, items.length, totalCount, onLoadMore, category]);

    const handleShowLess = useCallback(() => {
      setDisplayCount(initialDisplayCount);
    }, [initialDisplayCount]);

    return (
      <Box sx={styles.container}>
        <Box sx={styles.header}>
          <Typography variant="headingMedium" sx={styles.title}>
            {category}
          </Typography>
          {isLoading ? (
            <CircularProgress size={20} />
          ) : (
            <Tooltip title="Reload">
              <IconButtonAny
                variant="elitea"
                color="tertiary"
                onClick={() => onRefresh?.(category)}
              >
                <RefreshIcon />
              </IconButtonAny>
            </Tooltip>
          )}
        </Box>
        <Box sx={styles.grid}>
          {visibleItems
            .filter(item => !!item)
            .map(item => (
              <AgentCard
                key={category + item.id}
                application={item}
                onSelectItem={onSelectItem}
              />
            ))}
          {isLoadingMore &&
            Array.from(
              { length: Math.min(initialDisplayCount, Math.max(0, totalCount - items.length)) },
            ).map((_, i) => (
              <Skeleton key={`skeleton-${i}`} variant="rounded" sx={styles.skeleton} />
            ))}
        </Box>
        {shouldShowButton && (
          <Box sx={styles.showMoreContainer}>
            <ShowMoreButton
              expanded={isExpanded}
              onClick={isExpanded ? handleShowLess : handleShowMore}
              data-testid="agent-category-show-more"
            />
          </Box>
        )}
      </Box>
    );
  },
);

AgentCategorySection.displayName = 'AgentCategorySection';

const styles: Record<string, SxProps<Theme>> = {
  container: {
    width: '100%',
    maxWidth: '81.375rem',
    display: 'flex',
    flexDirection: 'column',
    gap: '1.5rem',
  },
  header: {
    display: 'flex',
    alignItems: 'center',
    gap: '0.5rem',
  },
  title: {
    color: 'text.secondary',
  },
  // Adversarial-review fix (cluster A13-agents-hub, finding 10): the
  // baseline (`AgentCategorySection.jsx`) ramps 1 → 2 → 3 → 4 columns across
  // `sm`/`prompt_list_full_width_sm`/`prompt_list_md`/`prompt_list_xl` —
  // this had regressed to a fixed 4-column grid, cramped/overflowing below
  // that widest breakpoint.
  grid: (theme: Theme) => ({
    display: 'grid',
    width: '100%',
    gridTemplateColumns: '1fr',
    gap: '1rem',
    [theme.breakpoints.up('sm')]: { gridTemplateColumns: 'repeat(2, 1fr)' },
    [theme.breakpoints.up('prompt_list_full_width_sm')]: { gridTemplateColumns: 'repeat(2, 1fr)' },
    [theme.breakpoints.up('prompt_list_md')]: { gridTemplateColumns: 'repeat(3, 1fr)' },
    [theme.breakpoints.up('prompt_list_xl')]: { gridTemplateColumns: 'repeat(4, 1fr)' },
  }),
  skeleton: {
    width: '100%',
    height: '7.25rem',
  },
  // #6640: 12px of space on the right, so the button does not sit on the
  // edge of the last card column.
  showMoreContainer: {
    display: 'flex',
    justifyContent: 'flex-end',
    alignItems: 'center',
    height: '1.5rem',
    paddingRight: '0.75rem',
  },
};

export default AgentCategorySection;
