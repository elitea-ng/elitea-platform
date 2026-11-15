/**
 * A titled grid of listing cards: the Featured row, the Popular row and the
 * full list are all this, so they share one set of columns.
 */
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { ListingCardData } from '../types';

import { MarketplaceCard } from './MarketplaceCard';

export interface ListingSectionProps {
  readonly id: string;
  readonly title: string;
  readonly cards: readonly ListingCardData[];
}

export function ListingSection({ id, title, cards }: ListingSectionProps): React.JSX.Element | null {
  if (cards.length === 0) return null;
  const headingId = `marketplace-section-${id}`;
  return (
    <Box component="section" aria-labelledby={headingId} data-testid={`marketplace-section-${id}`} sx={sectionSx}>
      <Typography id={headingId} component="h2" variant="headingMedium" color="text.secondary">
        {title}
      </Typography>
      <Box sx={gridSx}>
        {cards.map((card) => (
          <MarketplaceCard key={`${card.kind}-${card.id}`} card={card} />
        ))}
      </Box>
    </Box>
  );
}

const sectionSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1rem', width: '100%' };

// Columns by the room the page has, not the window: the side panel takes a share of it.
const gridSx: SxProps<Theme> = {
  display: 'grid',
  width: '100%',
  gap: '1rem',
  gridTemplateColumns: 'repeat(auto-fill, minmax(16rem, 1fr))',
};
