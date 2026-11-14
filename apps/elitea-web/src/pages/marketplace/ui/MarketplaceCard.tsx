/**
 * One listing in the Marketplace grid (ADR-0051 decision 8): avatar, name,
 * one-line description, the like count and the current version.
 *
 * The whole card is ONE link to the listing page — a real `<a>`, so it opens
 * in a new tab, copies as an address and is reached by the keyboard with the
 * browser's own focus ring. The name is the link's text, which is what a
 * screen reader announces; the like count is read as text, not as an icon.
 * Publisher, rating and hire count come with the feedback phase (M5), and the
 * "Hired" badge with hiring (M3): none is on the wire yet, so none is drawn.
 */
import { memo, type CSSProperties } from 'react';

import Box from '@mui/material/Box';
import Card from '@mui/material/Card';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { Link } from '@tanstack/react-router';

import { t } from '@/shared/i18n';
import { cardGradientSx } from '@/shared/lib/cardGradient';

import { listingParams } from '../listing';
import type { ListingCardData } from '../types';

export interface MarketplaceCardProps {
  readonly card: ListingCardData;
}

function CardAvatar({ card }: { readonly card: ListingCardData }): React.JSX.Element {
  if (card.iconUrl !== undefined) {
    return <Box component="img" src={card.iconUrl} alt="" sx={styles.icon} />;
  }
  // No icon: the first letter, so a catalogue full of icon-less listings is a
  // grid of distinct tiles and not a wall of the same mark.
  return (
    <Box aria-hidden sx={styles.monogram}>
      {card.name.trim().charAt(0).toUpperCase()}
    </Box>
  );
}

export const MarketplaceCard = memo(function MarketplaceCard({ card }: MarketplaceCardProps) {
  const likes = card.likes ?? 0;
  return (
    <Card data-testid="marketplace-card" sx={styles.card}>
      <Link
        style={linkStyle}
        to="/marketplace/$kind/$listingId"
        params={listingParams(card)}
        data-testid={`marketplace-card-link-${card.kind}-${card.id}`}
      >
        <Box sx={styles.header}>
          <CardAvatar card={card} />
          <Typography variant="headingSmall" sx={styles.title}>
            {card.name || t('marketplace.untitled', 'Untitled')}
          </Typography>
        </Box>
        <Typography variant="bodySmall" color="text.secondary" sx={styles.description}>
          {card.description}
        </Typography>
        <Box sx={styles.footer}>
          <Typography variant="bodySmall" color="text.secondary">
            {card.version === undefined ? '' : card.version}
          </Typography>
          {card.likes !== undefined && (
            <Typography variant="bodySmall" color="text.secondary">
              {t('marketplace.card.likes', '{{count}} likes', { count: likes })}
            </Typography>
          )}
        </Box>
      </Link>
    </Card>
  );
});

/** The card's one link: it fills the card, so the whole card is the click and tab target. */
const linkStyle: CSSProperties = {
  height: '100%',
  boxSizing: 'border-box',
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'stretch',
  justifyContent: 'space-between',
  gap: '0.5rem',
  padding: '0.75rem 1rem',
  textAlign: 'left',
  color: 'inherit',
  textDecoration: 'none',
};

const styles: Record<string, SxProps<Theme>> = {
  // The shared gradient card: fill and the 1px gradient border, so the card does not melt into the page in either scheme.
  card: (theme: Theme) => ({
    ...cardGradientSx(theme),
    boxShadow: 'none',
    height: '9rem',
    boxSizing: 'border-box',
  }),
  header: { display: 'flex', alignItems: 'center', gap: '0.75rem', width: '100%' },
  icon: (theme: Theme) => ({ width: '2rem', height: '2rem', flexShrink: 0, borderRadius: theme.vars.shape.radiusSm }),
  monogram: (theme: Theme) => ({
    width: '2rem',
    height: '2rem',
    flexShrink: 0,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    borderRadius: theme.vars.shape.radiusSm,
    background: theme.vars.palette.background.secondary,
    color: theme.vars.palette.text.secondary,
    fontWeight: 600,
  }),
  title: {
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    display: '-webkit-box',
    WebkitLineClamp: 1,
    WebkitBoxOrient: 'vertical',
  },
  description: {
    flex: 1,
    overflow: 'hidden',
    display: '-webkit-box',
    WebkitLineClamp: 2,
    WebkitBoxOrient: 'vertical',
  },
  footer: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '0.5rem', minHeight: '1.25rem' },
};
