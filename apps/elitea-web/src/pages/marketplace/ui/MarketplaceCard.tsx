/**
 * One listing in the Marketplace grid (ADR-0051 decision 8): avatar, name,
 * one-line description, the like count and the current version.
 *
 * The whole card is ONE link to the listing page — a real `<a>`, so it opens
 * in a new tab, copies as an address and is reached by the keyboard with the
 * browser's own focus ring. The name is the link's text, which is what a
 * screen reader announces; the like count is read as text, not as an icon.
 * It also shows the publisher, the heart (a button beside the link, for agents
 * and skills alike), the rating as text ("No ratings yet" until
 * reviews exist), the hire count, the tier, what the agent requires and a
 * "Hired v2.3" badge when this project hired it.
 */
import { memo, type CSSProperties } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Card from '@mui/material/Card';
import Chip from '@mui/material/Chip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { Link } from '@tanstack/react-router';

import { t } from '@/shared/i18n';
import { cardGradientSx } from '@/shared/lib/cardGradient';

import { listingParams, ratingText } from '../listing';
import type { ListingCardData } from '../types';
import { useTrySkill } from '../useTrySkill';

import { ListingLike, hasLikeTarget } from './ListingLike';

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

/** Try on a skill card (ADR-0051 amendment 1, A1.3): a new chat on the default model with this version pinned by reference. */
function CardTry({ card }: { readonly card: ListingCardData }): React.JSX.Element | null {
  const trySkill = useTrySkill();
  const listingId = Number(card.id);
  const version = Number(card.versionId);
  const valid = Number.isInteger(listingId) && listingId > 0 && Number.isInteger(version) && version > 0;
  if (!valid) return null;
  return (
    <Button
      size="small"
      variant="outlined"
      disabled={trySkill.start === undefined || trySkill.isStarting || card.delisted}
      onClick={() => trySkill.start?.({ name: card.name, pin: { listing_id: listingId, listing_version_id: version } })}
      aria-label={t('skills.try.cardLabel', 'Try {{name}}', { name: card.name })}
      data-testid="marketplace-card-try"
    >
      {t('skills.try.button', 'Try it')}
    </Button>
  );
}

export const MarketplaceCard = memo(function MarketplaceCard({ card }: MarketplaceCardProps) {
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
          <Box sx={styles.titles}>
            <Typography variant="headingSmall" sx={styles.title}>
              {card.name || t('marketplace.untitled', 'Untitled')}
            </Typography>
            {card.publisher !== undefined && (
              <Typography variant="bodySmall" color="text.secondary" sx={styles.title}>
                {t('marketplace.card.publisher', 'by {{name}}', { name: card.publisher })}
              </Typography>
            )}
          </Box>
          {card.hiredVersion !== undefined && (
            <Chip
              size="small"
              color="primary"
              label={t('marketplace.card.hired', 'Hired {{version}}', { version: card.hiredVersion })}
              data-testid="marketplace-card-hired"
            />
          )}
        </Box>
        <Typography variant="bodySmall" color="text.secondary" sx={styles.description}>
          {card.description}
        </Typography>
        <Box sx={styles.chips}>
          {card.requiredTypes.slice(0, 2).map((type) => (
            <Chip
              key={type}
              size="small"
              variant="outlined"
              label={t('marketplace.card.requires', 'Requires {{type}}', { type })}
            />
          ))}
          <Chip
            size="small"
            variant="outlined"
            label={t('marketplace.card.tier', 'Tier {{tier}}', { tier: card.tier })}
          />
        </Box>
        <Box sx={card.kind === 'skills' ? styles.footerWithTry : styles.footer}>
          <Typography variant="bodySmall" color="text.secondary">
            {card.version ?? ''}
          </Typography>
          <Typography variant="bodySmall" color="text.secondary">
            {ratingText(card)}
          </Typography>
          <Typography variant="bodySmall" color="text.secondary">
            {t('marketplace.card.hires', '{{count}} hires', { count: card.hires })}
          </Typography>
        </Box>
      </Link>
      {/* Beside the link, not inside it: a button in an anchor is invalid, and a like must not open the listing. */}
      <Box sx={styles.like}>
        {card.kind === 'skills' && <CardTry card={card} />}
        {hasLikeTarget(card.entityId) && (
          <ListingLike kind={card.kind} entityId={card.entityId} likes={card.likes} isLiked={card.isLiked} name={card.name} />
        )}
      </Box>
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
    height: '12rem',
    boxSizing: 'border-box',
    position: 'relative',
  }),
  like: { position: 'absolute', right: '1rem', bottom: '0.5rem', display: 'flex', alignItems: 'center', gap: '0.5rem' },
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
  titles: { flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column' },
  chips: { display: 'flex', flexWrap: 'wrap', gap: '0.25rem' },
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
  footer: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    flexWrap: 'wrap',
    gap: '0.5rem',
    minHeight: '1.25rem',
    // Room for the heart, which sits beside the link at the card's corner.
    paddingRight: '3.5rem',
  },
  // Room for Try as well, beside the heart.
  footerWithTry: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap', gap: '0.5rem', minHeight: '1.25rem', paddingRight: '7.5rem' },
};
