/**
 * The strip above the grid: what is being shown and which filters are on,
 * with a **Browse** button that opens the right sidebar.
 *
 * It shows only while the sidebar is closed (or a drawer not yet opened). It is the answer to a closed sidebar (ADR-0051 risks): every active filter is
 * a removable chip here, so a person who closed the panel still sees why the
 * list is short and can clear it without opening anything.
 */
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { useSidePanelSlot } from '@/shared/ui/side-panel';

import type { MarketplaceFilters } from '../filters';

export interface MarketplaceSummaryBarProps {
  /** "12 agents" — what the list below holds. */
  readonly summary: string;
  readonly filters: MarketplaceFilters;
  readonly onChange: (patch: Partial<MarketplaceFilters>) => void;
  readonly onClearAll: () => void;
}

interface FilterChip {
  readonly key: string;
  readonly label: string;
  readonly testId?: string;
  readonly onRemove: () => void;
}

/** One removable chip for every active filter, the search text included. */
function chipsOf(filters: MarketplaceFilters, onChange: MarketplaceSummaryBarProps['onChange']): FilterChip[] {
  const chips: FilterChip[] = [];
  if (filters.query.trim() !== '') {
    chips.push({
      key: 'query',
      testId: 'marketplace-chip-query',
      label: t('marketplace.summary.search', 'Search: {{query}}', { query: filters.query.trim() }),
      onRemove: () => onChange({ query: '' }),
    });
  }
  for (const category of filters.categories) {
    chips.push({
      key: `category-${category}`,
      label: t('marketplace.summary.category', 'Category: {{category}}', { category }),
      onRemove: () => onChange({ categories: filters.categories.filter((c) => c !== category) }),
    });
  }
  for (const tag of filters.tags) {
    chips.push({
      key: `tag-${tag}`,
      label: t('marketplace.summary.tag', 'Tag: {{tag}}', { tag }),
      onRemove: () => onChange({ tags: filters.tags.filter((c) => c !== tag) }),
    });
  }
  for (const type of filters.requires) {
    chips.push({
      key: `requires-${type}`,
      label: t('marketplace.summary.requires', 'Requires: {{type}}', { type }),
      onRemove: () => onChange({ requires: filters.requires.filter((c) => c !== type) }),
    });
  }
  if (filters.publisherId !== '') {
    chips.push({
      key: 'publisher',
      label: t('marketplace.summary.publisher', 'Publisher selected'),
      onRemove: () => onChange({ publisherId: '' }),
    });
  }
  if (filters.maxTier !== '') {
    chips.push({
      key: 'tier',
      label: t('marketplace.summary.tier', 'Tier up to {{tier}}', { tier: filters.maxTier }),
      onRemove: () => onChange({ maxTier: '' }),
    });
  }
  if (filters.minRating > 0) {
    chips.push({
      key: 'rating',
      label: t('marketplace.summary.rating', 'Rating {{rating}} and up', { rating: filters.minRating }),
      onRemove: () => onChange({ minRating: 0 }),
    });
  }
  if (filters.hiredHere) {
    chips.push({
      key: 'hired',
      label: t('marketplace.summary.hiredHere', 'Hired in this project'),
      onRemove: () => onChange({ hiredHere: false }),
    });
  }
  return chips;
}

export function MarketplaceSummaryBar({
  summary,
  filters,
  onChange,
  onClearAll,
}: MarketplaceSummaryBarProps): React.JSX.Element | null {
  const slot = useSidePanelSlot();
  // While the Browse tab is on screen it already shows every filter, so the bar has nothing to add.
  if (slot?.targets.has('filters') === true) return null;
  const chips = chipsOf(filters, onChange);
  return (
    <Box data-testid="marketplace-summary-bar" sx={barSx}>
      <Typography variant="bodySmall" color="text.metrics" data-testid="marketplace-summary" aria-live="polite">
        {summary}
      </Typography>
      <Box sx={chipsSx}>
        {chips.map((chip) => (
          <Chip key={chip.key} size="small" label={chip.label} onDelete={chip.onRemove} data-testid={chip.testId} />
        ))}
        {chips.length > 0 && (
          <Button size="small" onClick={onClearAll} data-testid="marketplace-clear-all">
            {t('marketplace.summary.clearAll', 'Clear all')}
          </Button>
        )}
      </Box>
      {slot !== null && (
        <Button
          size="small"
          variant="outlined"
          onClick={() => slot.show('filters')}
          data-testid="marketplace-open-browse"
        >
          {t('marketplace.browse.title', 'Browse')}
        </Button>
      )}
    </Box>
  );
}

const barSx: SxProps<Theme> = {
  flexShrink: 0,
  display: 'flex',
  alignItems: 'center',
  flexWrap: 'wrap',
  gap: '0.75rem',
  paddingBlock: '0.5rem',
  borderBottom: 1,
  borderColor: 'divider',
};

const chipsSx: SxProps<Theme> = {
  flex: 1,
  minWidth: 0,
  display: 'flex',
  alignItems: 'center',
  flexWrap: 'wrap',
  gap: '0.5rem',
};
