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
import { memo, useState } from 'react';

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

/** A listing whose public twin was deleted (an unpublish) has no id to like. */
export function hasLikeTarget(entityId: string): boolean {
  return /^[1-9][0-9]*$/.test(entityId);
}

export const ListingLike = memo(function ListingLike(props: ListingLikeProps) {
  // The check sits outside the hook's component so a listing without a twin
  // renders no heart at all instead of a control that can only fail.
  return hasLikeTarget(props.entityId) ? <LikeButton {...props} /> : null;
});

function LikeButton({ kind, entityId, likes, isLiked, name }: ListingLikeProps) {
  const [failed, setFailed] = useState(false);
  const { isLiked: liked, likeCount, isToggling, toggleLike } = useCardLike({
    applicationId: entityId,
    entityType: kind === 'skills' ? 'skill' : 'application',
    projectId: resolvePublicProjectId(),
    initialLiked: isLiked,
    initialCount: likes,
    onError: () => setFailed(true),
  });
  return (
    <Box
      component="button"
      type="button"
      aria-pressed={liked}
      aria-label={t('marketplace.like.label', 'Like {{name}}', { name })}
      aria-invalid={failed || undefined}
      disabled={isToggling}
      data-testid={`marketplace-like-${kind}-${entityId}`}
      onClick={(event) => {
        event.stopPropagation();
        setFailed(false);
        void toggleLike();
      }}
      sx={failed ? { ...styles.button, ...styles.failed } : styles.button}
    >
      {liked ? <HeartActiveIcon /> : <HeartIcon />}
      <Typography variant="bodySmall" data-testid="marketplace-like-count">
        {likeCount}
      </Typography>
      {failed && (
        <Box component="span" role="alert" sx={styles.visuallyHidden}>
          {t('marketplace.like.failed', 'Your like could not be saved.')}
        </Box>
      )}
    </Box>
  );
}

const styles = {
  button: {
    cursor: 'pointer',
    display: 'flex',
    alignItems: 'center',
    gap: 0.5,
    background: 'none',
    border: 0,
    // A 24px target at least (WCAG 2.2 target size), with the icon and count unchanged.
    minHeight: '1.5rem',
    minWidth: '1.5rem',
    padding: '0 0.25rem',
    color: 'inherit',
    font: 'inherit',
  },
  // A refused like snaps back; the heart says so beyond the screen reader.
  failed: { color: 'error.main' },
  visuallyHidden: {
    position: 'absolute',
    width: '1px',
    height: '1px',
    overflow: 'hidden',
    clip: 'rect(0 0 0 0)',
    whiteSpace: 'nowrap',
  },
} as const;
