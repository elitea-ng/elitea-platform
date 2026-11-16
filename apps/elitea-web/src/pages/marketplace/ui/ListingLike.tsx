/**
 * The heart of a Marketplace listing, agent or skill (ADR-0051 amendment 1,
 * A1.5): the like count and the caller's own like, on the card and on the
 * listing page.
 *
 * A like belongs to the listing's public twin (`entity_id`), so it is the same
 * like wherever the listing is shown. An agent is liked as the `application`
 * entity and a skill as the `skill` entity, in the public project.
 *
 * On a card it sits BESIDE the card's link, not inside it: a button inside an
 * anchor is invalid, and a like must not open the listing (issue 2028).
 */
import { memo } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { useCardLike } from '@/entities/application';
import { t } from '@/shared/i18n';
import { HeartActiveIcon } from '@/shared/ui/icons/heart-active-icon';
import { HeartIcon } from '@/shared/ui/icons/heart-icon';

import { resolvePublicProjectId } from '../listing';
import type { MarketplaceKind } from '../types';

export interface ListingLikeProps {
  readonly kind: MarketplaceKind;
  /** The listing's public twin id. */
  readonly entityId: string;
  readonly likes: number;
  readonly isLiked: boolean;
  /** The listing's name, for the accessible name. */
  readonly name: string;
}

export const ListingLike = memo(function ListingLike({ kind, entityId, likes, isLiked, name }: ListingLikeProps) {
  const { isLiked: liked, likeCount, isToggling, toggleLike } = useCardLike({
    applicationId: entityId,
    entityType: kind === 'skills' ? 'skill' : 'application',
    projectId: resolvePublicProjectId(),
    initialLiked: isLiked,
    initialCount: likes,
  });
  return (
    <Box
      component="button"
      type="button"
      aria-pressed={liked}
      aria-label={t('marketplace.like.label', 'Like {{name}}', { name })}
      disabled={isToggling}
      data-testid={`marketplace-like-${kind}-${entityId}`}
      onClick={(event) => {
        event.stopPropagation();
        void toggleLike();
      }}
      sx={styles.button}
    >
      {liked ? <HeartActiveIcon /> : <HeartIcon />}
      <Typography variant="bodySmall" data-testid="marketplace-like-count">
        {likeCount}
      </Typography>
    </Box>
  );
});

const styles = {
  button: {
    cursor: 'pointer',
    display: 'flex',
    alignItems: 'center',
    gap: 0.5,
    background: 'none',
    border: 0,
    padding: 0,
    color: 'inherit',
    font: 'inherit',
  },
} as const;
