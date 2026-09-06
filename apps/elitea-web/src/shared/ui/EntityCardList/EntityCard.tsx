import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Divider from '@mui/material/Divider';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { cardGradientSx } from '@/shared/lib/cardGradient';

import { EntityCardAuthors } from './EntityCardAuthors';
import { EntityCardTags } from './EntityCardTags';
import type { EntityListItem, EntityListTag } from './model';

/**
 * One entity card in a list page's grid — the port of
 * `apps/elitea-ui/src/components/Card.jsx`.
 *
 * Every number below was MEASURED on the live production app
 * (next.elitea.ai, `/app/agents/all`, 2000px viewport) rather than read off
 * the source, because `Card.jsx`'s inner `<MuiCard display: inline>` makes
 * its declared `margin: 0.625rem 1.375rem` a no-op — the content box lays
 * out flush with the card, and reading the source literally would have
 * inset everything by 22px that production does not inset:
 *
 *  - card box `7rem` tall, gradient fill + 1px gradient border
 *    (`shared/lib/cardGradient`). DISCLOSED DEVIATION: production's radius
 *    is 12px and the brand pack has no 12px token (`radiusSm|Md|Lg` are
 *    4/8/16), so `radiusLg` is used — the same 4px-over choice
 *    `shared/lib/cardGradient` and `shared/ui/GradientIconWrapper` already
 *    made, and R-T10 bans the ad-hoc literal that would close the gap;
 *  - top section `4.5rem` tall, `1.25rem` padding all round, `1rem` gap,
 *    items centred;
 *  - icon tile `2.25rem` circle holding a `1rem` glyph;
 *  - title `headingSmall` (14px/24px/600), two-line clamp;
 *  - bottom section `2.5rem` tall, padding `0 0.75rem 0 1.125rem`, `0.5rem`
 *    gap, items top-aligned, holding a `1.75rem`-tall row of author avatars
 *    and tag chips.
 *
 * A real `role="button"` + `tabIndex` (not MUI's `ButtonBase`) because the
 * bottom row holds its own clickable tag chips; nesting interactive
 * elements inside a `<button>` is invalid HTML and axe rejects it.
 */
export interface EntityCardProps {
  readonly item: EntityListItem;
  /** Top-right slot — publish/like/pin/status affordances the owning page composes. */
  readonly actions?: ReactNode;
  readonly onTagClick?: (tag: EntityListTag) => void;
  /** Overrides the `entity-card` test id — `pages/toolkits` keeps `toolkit-card`, which its Playwright journeys already select on. */
  readonly 'data-testid'?: string;
}

export function EntityCard({ item, actions, onTagClick, 'data-testid': dataTestId = 'entity-card' }: EntityCardProps): ReactNode {
  const authors = item.authors ?? [];
  const tags = item.tags ?? [];
  const hasBottomDivider = authors.length > 0 && tags.length > 0;

  const activate = (): void => {
    item.onClick?.();
  };

  return (
    <Box
      data-testid={dataTestId}
      // oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- the card's own bottom row holds clickable tag chips; a real <button> may not nest interactive content (and MUI's Divider emits an <hr>, which a button may not contain either).
      role="button"
      tabIndex={0}
      aria-label={item.name}
      onClick={activate}
      onKeyDown={(event) => {
        if (event.key !== 'Enter' && event.key !== ' ') return;
        event.preventDefault();
        activate();
      }}
      sx={cardSx(item.onClick !== undefined)}
    >
      {actions !== undefined && <Box sx={actionsSx}>{actions}</Box>}
      <Box sx={topRowSx}>
        <Box
          data-testid="entity-card-icon"
          sx={iconTileSx}
        >
          {item.icon}
        </Box>
        <Tooltip
          title={item.description === undefined || item.description === '' ? item.name : `${item.name} — ${item.description}`}
          placement="top"
          enterDelay={1000}
          enterNextDelay={1000}
        >
          <Typography
            variant="headingSmall"
            data-testid="entity-card-name"
            sx={titleSx}
          >
            {item.name}
          </Typography>
        </Tooltip>
      </Box>
      <Box sx={bottomRowSx}>
        <Box sx={bottomLeftSx}>
          <EntityCardAuthors authors={authors} />
          {hasBottomDivider && (
            <Divider
              data-testid="entity-card-section-divider"
              orientation="vertical"
              flexItem
              sx={sectionDividerSx}
            />
          )}
          <EntityCardTags
            tags={tags}
            {...(onTagClick === undefined ? {} : { onTagClick })}
          />
        </Box>
      </Box>
    </Box>
  );
}

function cardSx(clickable: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    ...cardGradientSx(theme),
    boxSizing: 'border-box',
    width: '100%',
    height: '7rem',
    display: 'flex',
    flexDirection: 'column',
    cursor: clickable ? 'pointer' : 'default',
    '&:focus-visible': { outline: `0.125rem solid ${theme.vars.palette.border.lines}`, outlineOffset: '0.125rem' },
  });
}

/** `Card.jsx`'s `topRightSection`: `0.75rem` in from the top-right corner. */
const actionsSx: SxProps<Theme> = (theme: Theme) => ({
  position: 'absolute',
  top: theme.spacing(1.5),
  right: theme.spacing(1.5),
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(0.25),
  zIndex: 1,
});

const topRowSx: SxProps<Theme> = (theme: Theme) => ({
  boxSizing: 'border-box',
  display: 'flex',
  flexDirection: 'row',
  alignItems: 'center',
  gap: theme.spacing(2),
  height: '4.5rem',
  width: '100%',
  padding: theme.spacing(2.5),
  minWidth: 0,
});

/** `components/EntityIcon.jsx`'s round gradient frame — 2.25rem with a 1rem glyph. */
const iconTileSx: SxProps<Theme> = (theme: Theme) => ({
  flexShrink: 0,
  width: '2.25rem',
  height: '2.25rem',
  borderRadius: theme.vars.shape.radiusPill,
  position: 'relative',
  overflow: 'hidden',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  background: theme.vars.palette.background.icon.entityGradient,
  color: theme.vars.palette.icon.fill.default,
  '& > svg': { width: '1rem', height: '1rem' },
  '&::before': {
    content: '""',
    position: 'absolute',
    inset: 0,
    borderRadius: theme.vars.shape.radiusPill,
    padding: '0.0625rem',
    background: theme.vars.palette.background.icon.entityBorderGradient,
    // Masks read only the alpha channel, so an always-opaque `currentColor`
    // paints the same mask a raw `#fff` would (R-T1: no colour literals).
    WebkitMask: 'linear-gradient(currentColor 0 0) content-box, linear-gradient(currentColor 0 0)',
    WebkitMaskComposite: 'xor',
    maskComposite: 'exclude',
    pointerEvents: 'none',
  },
});

/** `Card.jsx`'s `cardTitle`: two-line clamp, `text.secondary` (white in the dark scheme). */
const titleSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.text.secondary,
  minWidth: 0,
  maxHeight: '3rem',
  wordBreak: 'break-word',
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  display: '-webkit-box',
  WebkitBoxOrient: 'vertical',
  WebkitLineClamp: '2',
});

const bottomRowSx: SxProps<Theme> = (theme: Theme) => ({
  boxSizing: 'border-box',
  display: 'flex',
  flexDirection: 'row',
  justifyContent: 'space-between',
  alignItems: 'flex-start',
  gap: theme.spacing(1),
  height: '2.5rem',
  width: '100%',
  padding: `0 ${theme.spacing(1.5)} 0 ${theme.spacing(2.25)}`,
  minWidth: 0,
});

const bottomLeftSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1),
  height: '1.75rem',
  minWidth: 0,
  overflow: 'hidden',
  flex: 1,
});

const sectionDividerSx: SxProps<Theme> = { height: '0.9375rem', alignSelf: 'center' };
