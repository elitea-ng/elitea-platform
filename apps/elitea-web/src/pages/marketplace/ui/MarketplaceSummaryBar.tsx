/**
 * The strip above the grid: what is being shown and which filters are on,
 * with a **Browse** button that opens the right sidebar.
 *
 * It is the answer to a closed sidebar (ADR-0051 risks): every active filter is
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

export interface MarketplaceSummaryBarProps {
  /** "12 agents" — what the list below holds. */
  readonly summary: string;
  readonly query: string;
  readonly categories: readonly string[];
  readonly onClearQuery: () => void;
  readonly onRemoveCategory: (category: string) => void;
  readonly onClearAll: () => void;
}

export function MarketplaceSummaryBar({ summary, query, categories, onClearQuery, onRemoveCategory, onClearAll }: MarketplaceSummaryBarProps): React.JSX.Element {
  const slot = useSidePanelSlot();
  const hasFilters = query.trim() !== '' || categories.length > 0;
  return (
    <Box data-testid="marketplace-summary-bar" sx={barSx}>
      <Typography variant="bodySmall" color="text.metrics" data-testid="marketplace-summary" aria-live="polite">
        {summary}
      </Typography>
      <Box sx={chipsSx}>
        {query.trim() !== '' && (
          <Chip size="small" label={t('marketplace.summary.search', 'Search: {{query}}', { query: query.trim() })} onDelete={onClearQuery} data-testid="marketplace-chip-query" />
        )}
        {categories.map((category) => (
          <Chip
            key={category}
            size="small"
            label={t('marketplace.summary.category', 'Category: {{category}}', { category })}
            onDelete={() => onRemoveCategory(category)}
          />
        ))}
        {hasFilters && (
          <Button size="small" onClick={onClearAll} data-testid="marketplace-clear-all">
            {t('marketplace.summary.clearAll', 'Clear all')}
          </Button>
        )}
      </Box>
      {slot !== null && (
        <Button size="small" variant="outlined" onClick={() => slot.show('filters')} data-testid="marketplace-open-browse">
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

const chipsSx: SxProps<Theme> = { flex: 1, minWidth: 0, display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: '0.5rem' };
