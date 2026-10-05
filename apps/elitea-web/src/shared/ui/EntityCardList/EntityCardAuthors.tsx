import type { ReactNode } from 'react';

import PersonOffOutlinedIcon from '@mui/icons-material/PersonOffOutlined';
import Avatar from '@mui/material/Avatar';
import Box from '@mui/material/Box';
import Tooltip from '@mui/material/Tooltip';
import type { SxProps, Theme } from '@mui/material/styles';

import { avatarInitialsType, typeScale } from '@/shared/brand/typeScale';
import { getInitials, stringToColor } from '@/shared/lib/string';

import type { EntityListAuthor } from './model';

/**
 * The overlapping author avatars on a card's bottom-left, ported from
 * `apps/elitea-ui/src/components/AuthorContainer.jsx` (`showName={false}`
 * branch, which is the one `Card.jsx` uses) plus its `UserAvatar.jsx`:
 * at most three 20px avatars, each shifted 5px left of the previous one,
 * then a `+N` counter. The whole cluster carries the comma-joined author
 * names as a tooltip, exactly as `Card.jsx` wraps it.
 */
const MAX_AVATARS = 3;
/** `UserAvatar.jsx`'s default `size` — 20px. */
const AVATAR_SIZE = '1.25rem';
/** The circle's size in px — what `avatarInitialsType` snaps the initials by. */
const AVATAR_SIZE_PX = 20;

export interface EntityCardAuthorsProps {
  readonly authors: readonly EntityListAuthor[];
}

export function EntityCardAuthors({ authors }: EntityCardAuthorsProps): ReactNode {
  if (authors.length === 0) return null;
  const shown = authors.slice(0, MAX_AVATARS);
  const extra = authors.length - shown.length;
  return (
    <Tooltip
      title={authors.map((author) => author.name).join(', ')}
      placement="top"
    >
      <Box sx={containerSx}>
        {shown.map((author, index) => (
          <Avatar
            key={author.id ?? author.name}
            {...(author.avatar === undefined || author.deleted === true ? {} : { src: author.avatar })}
            alt={author.name}
            data-testid="entity-card-author-avatar"
            sx={avatarSx(index, shown.length - index, author.avatar === undefined && author.deleted !== true && author.name.trim() !== '' ? author.name : undefined)}
          >
            {author.deleted === true || author.name.trim() === ''
              // A deleted account has no name to take initials from (#6702).
              ? <PersonOffOutlinedIcon sx={{ fontSize: 'inherit' }} data-testid="entity-card-deleted-author-avatar" />
              : getInitials(author.name)}
          </Avatar>
        ))}
        {extra > 0 && <Box sx={extraSx}>{`+${String(extra)}`}</Box>}
      </Box>
    </Tooltip>
  );
}

const containerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(0.5),
  flexShrink: 0,
  minWidth: AVATAR_SIZE,
});

/**
 * `UserAvatar.jsx`: `transform: translateX(-{index * 5}px)`, `zIndex`
 * descending so the first avatar sits on top, `fontSize: ceil(size / 2)`
 * (10px at the default 20px size), and — for an author with no picture —
 * a name-derived background instead of the flat icon colour.
 */
function avatarSx(index: number, zIndex: number, initialsFor: string | undefined): SxProps<Theme> {
  return (theme: Theme) => ({
    width: AVATAR_SIZE,
    height: AVATAR_SIZE,
    // Initials snap to the ladder by the circle's size (≤24px → labelSmall,
    // weight 500): the whole variant, so MuiAvatar's 40px default
    // (headingMedium, 600) does not leak its weight and leading in.
    ...typeScale(theme.typography[avatarInitialsType(AVATAR_SIZE_PX)]),
    padding: 0,
    transform: `translateX(-${String(index * 5)}px)`,
    zIndex,
    color: theme.vars.palette.text.secondary,
    backgroundColor: initialsFor === undefined ? theme.vars.palette.background.icon.default : stringToColor(initialsFor),
  });
}

const extraSx: SxProps<Theme> = (theme: Theme) => ({
  ...theme.typography.bodySmall,
  color: theme.vars.palette.text.primary,
  whiteSpace: 'nowrap',
});
