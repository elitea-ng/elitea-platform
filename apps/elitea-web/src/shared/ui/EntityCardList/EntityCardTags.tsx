import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Divider from '@mui/material/Divider';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { EntityListTag } from './model';

/**
 * A card's bottom-row tag strip, ported from
 * `apps/elitea-ui/src/components/CardTagSection.jsx` +
 * `CardTagSectionItem.jsx`: at most two tag names, each `0.5rem` padded,
 * separated by a `0.9375rem`-tall vertical rule, then a `+N` overflow
 * counter when more tags exist. The strip is `1.75rem` tall, which is what
 * sets the card's bottom row height.
 */
const MAX_TAGS = 2;

export interface EntityCardTagsProps {
  readonly tags: readonly EntityListTag[];
  readonly onTagClick?: (tag: EntityListTag) => void;
}

export function EntityCardTags({ tags, onTagClick }: EntityCardTagsProps): ReactNode {
  if (tags.length === 0) return null;
  const shown = tags.slice(0, MAX_TAGS);
  const extra = tags.length - shown.length;
  return (
    <Box sx={sectionSx}>
      {shown.map((tag, index) => (
        <Box
          key={tag.id ?? tag.name}
          component="span"
          data-testid="entity-card-tag-chip"
          sx={itemSx(onTagClick !== undefined)}
          onClick={
            onTagClick === undefined
              ? undefined
              : (event) => {
                  event.stopPropagation();
                  onTagClick(tag);
                }
          }
        >
          <Typography
            variant="bodySmall"
            sx={textSx(index > 0)}
          >
            {tag.name}
          </Typography>
          {(index < shown.length - 1 || extra > 0) && (
            <Divider
              orientation="vertical"
              flexItem
              sx={dividerSx}
            />
          )}
        </Box>
      ))}
      {extra > 0 && (
        <Box
          component="span"
          data-testid="entity-card-tag-overflow"
          sx={itemSx(false)}
        >
          <Typography
            variant="bodySmall"
            sx={textSx(false)}
          >
            {`+${String(extra)}`}
          </Typography>
        </Box>
      )}
    </Box>
  );
}

const sectionSx: SxProps<Theme> = {
  display: 'flex',
  height: '1.75rem',
  minWidth: 0,
  overflow: 'hidden',
  padding: 0,
};

function itemSx(clickable: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    display: 'flex',
    alignItems: 'center',
    height: '1.75rem',
    whiteSpace: 'nowrap',
    caretColor: 'transparent',
    cursor: clickable ? 'pointer' : 'default',
    color: theme.vars.palette.text.primary,
    '&:hover': { color: clickable ? theme.vars.palette.text.secondary : undefined },
  });
}

/** `CardTagSectionItem.jsx`: `0.5rem` right padding always, `0.5rem` left only after the first chip. */
function textSx(paddingLeft: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    paddingLeft: paddingLeft ? theme.spacing(1) : 0,
    paddingRight: theme.spacing(1),
    color: 'inherit',
    overflow: 'hidden',
    textOverflow: 'ellipsis',
  });
}

const dividerSx: SxProps<Theme> = { height: '0.9375rem', alignSelf: 'center' };
