import type { ReactNode } from 'react';

import PushPinIcon from '@mui/icons-material/PushPin';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { formatDistanceToNow } from 'date-fns';

import { t } from '@/shared/i18n';
import { Monogram } from '@/shared/ui/Monogram';

import { CardSelectBox } from './EntityCard';
import { EntityCardAuthors } from './EntityCardAuthors';
import type { EntityListItem, EntityListTag } from './model';
import { useEntityRowSlot } from './rowSlot';
import { useSelectableRow } from './selection';
import * as styles from './entityRichCard.styles';

/**
 * The Agents card of issue 3133. Where the compact card (`EntityCard`) ports
 * the old app's tile, this one leads with who the agent is:
 *
 *  - a neutral card on the theme's card surface with a hairline border, a
 *    hover lift and the brand's focus ring; no per-card tint;
 *  - the agent's own avatar (the page passes the live-avatar renderer as
 *    `item.icon`), or a brand-tinted monogram, never a placeholder glyph;
 *  - the name with a small status badge and, when pinned, a filled pin; the
 *    persona role; a two-line description;
 *  - a muted footer: the creator, at most two tag chips and "+N", and when it
 *    last changed;
 *  - the actions (the page's primary action and the row slot's drag, pin and
 *    folder controls) in a cluster drawn on hover and on focus within, so a
 *    keyboard user tabbing into the card sees them, and always on a device
 *    that cannot hover.
 *
 * As on the compact card, the NAME is the card's button and a click anywhere
 * else bubbles to the card's own handler; every control is a sibling of that
 * button, never nested in it (axe `nested-interactive`).
 */
export interface EntityRichCardProps {
  readonly item: EntityListItem;
  readonly onTagClick?: (tag: EntityListTag) => void;
}

export function EntityRichCard({ item, onTagClick }: EntityRichCardProps): ReactNode {
  const row = useSelectableRow(item.id, item.onClick);
  const slot = useEntityRowSlot();
  const pinned = slot?.isPinned?.(item) ?? false;

  return (
    <Box
      data-testid="entity-card"
      data-variant="rich"
      data-selected={row.selected ? 'true' : undefined}
      onClick={row.activate}
      sx={styles.cardSx(item.onClick !== undefined, row.selected)}
    >
      {row.selectable && (
        <CardSelectBox
          name={item.name}
          selected={row.selected}
          onToggle={row.toggle}
        />
      )}
      <Box sx={styles.headerSx}>
        <Box
          data-testid="entity-card-icon"
          sx={styles.avatarSx}
        >
          {item.icon ?? (
            <Monogram
              name={item.name}
              size="100%"
            />
          )}
        </Box>
        <Box sx={styles.headingSx}>
          <CardTitleRow
            item={item}
            pinned={pinned}
          />
          {item.subtitle !== undefined && item.subtitle !== '' && (
            <Typography
              variant="bodySmall"
              data-testid="entity-card-subtitle"
              sx={styles.subtitleSx}
            >
              {item.subtitle}
            </Typography>
          )}
        </Box>
      </Box>
      {item.description !== undefined && item.description !== '' && (
        <Typography
          variant="bodyMedium"
          data-testid="entity-card-description"
          sx={styles.descriptionSx}
        >
          {item.description}
        </Typography>
      )}
      <CardFooter
        item={item}
        {...(onTagClick === undefined ? {} : { onTagClick })}
      />
      <CardActions item={item} />
    </Box>
  );
}

/** The name (the card's button), the filled pin of a pinned row and the status badge. */
function CardTitleRow({ item, pinned }: { readonly item: EntityListItem; readonly pinned: boolean }): ReactNode {
  return (
    <Box sx={styles.titleRowSx}>
      <Typography
        component="button"
        type="button"
        variant="headingSmall"
        data-testid="entity-card-name"
        data-entity-card-open=""
        sx={styles.titleSx}
      >
        {item.name}
      </Typography>
      {pinned && (
        <Box
          component="span"
          data-testid="entity-card-pinned"
          sx={styles.pinnedSx}
        >
          <PushPinIcon
            fontSize="inherit"
            titleAccess={t('shared.entityList.pinned', 'Pinned')}
          />
        </Box>
      )}
      {item.badge !== undefined && (
        <Typography
          component="span"
          variant="labelSmall"
          data-testid="entity-card-status"
          sx={styles.badgeSx}
        >
          {item.badge}
        </Typography>
      )}
    </Box>
  );
}

/** A row's last change as "Updated 3 days ago", or nothing for a missing or zero-sentinel time. */
function updatedLabel(updatedAt: string | undefined): string | undefined {
  if (updatedAt === undefined) return undefined;
  const parsed = new Date(updatedAt);
  // The list used to answer "0001-01-01T00:00:00Z" for every row (see the Application schema).
  if (Number.isNaN(parsed.getTime()) || parsed.getUTCFullYear() < 2000) return undefined;
  return t('shared.entityList.updatedAgo', 'Updated {{distance}} ago', { distance: formatDistanceToNow(parsed) });
}

/** The muted footer: the creator, the tag chips and the last change; the fork marker when there is one. */
function CardFooter({ item, onTagClick }: { readonly item: EntityListItem; readonly onTagClick?: (tag: EntityListTag) => void }): ReactNode {
  const updated = updatedLabel(item.updatedAt);
  return (
    <Box sx={styles.footerSx}>
      <EntityCardAuthors authors={item.authors ?? []} />
      <TagChips
        tags={item.tags ?? []}
        {...(onTagClick === undefined ? {} : { onTagClick })}
      />
      {updated !== undefined && (
        <Box
          component="time"
          dateTime={item.updatedAt}
          data-testid="entity-card-updated"
          sx={styles.updatedSx}
        >
          {updated}
        </Box>
      )}
      {item.forkedFrom !== undefined && (
        <Tooltip
          title={t('shared.entityList.forkedFromTooltip', 'Forked from another entity. Switch to the table view to open the original.')}
          placement="top"
        >
          <Typography
            component="span"
            variant="bodySmall"
            data-testid="entity-card-forked-from"
            sx={styles.updatedSx}
          >
            {t('shared.entityList.forkedFrom', 'Forked from')}
          </Typography>
        </Tooltip>
      )}
    </Box>
  );
}

const MAX_TAGS = 2;

/** At most two compact chips, then "+N" (the rest named in its tooltip). A chip filters by its tag when the page offers that. */
function TagChips({ tags, onTagClick }: { readonly tags: readonly EntityListTag[]; readonly onTagClick?: (tag: EntityListTag) => void }): ReactNode {
  if (tags.length === 0) return null;
  const shown = tags.slice(0, MAX_TAGS);
  const rest = tags.slice(MAX_TAGS);
  return (
    <Box sx={styles.tagsSx}>
      {shown.map((tag) => (
        <Box
          key={tag.id ?? tag.name}
          component="span"
          data-testid="entity-card-tag-chip"
          sx={styles.tagChipSx(onTagClick !== undefined)}
          onClick={
            onTagClick === undefined
              ? undefined
              : (event) => {
                  event.stopPropagation();
                  onTagClick(tag);
                }
          }
        >
          {tag.name}
        </Box>
      ))}
      {rest.length > 0 && (
        <Tooltip title={rest.map((tag) => tag.name).join(', ')}>
          <Box
            component="span"
            data-testid="entity-card-tag-overflow"
            sx={styles.tagChipSx(false)}
          >
            {`+${String(rest.length)}`}
          </Box>
        </Tooltip>
      )}
    </Box>
  );
}

/** The hover and focus cluster: the page's primary action, then the row slot's controls (drag, pin, folder). */
function CardActions({ item }: { readonly item: EntityListItem }): ReactNode {
  const rowAction = useEntityRowSlot()?.renderCardAction(item);
  const primary = item.primaryAction;
  if (primary === undefined && (rowAction === undefined || rowAction === null)) return null;
  return (
    <Box
      data-entity-card-actions=""
      sx={styles.actionsSx}
    >
      {rowAction}
      {primary !== undefined && (
        <Button
          size="small"
          variant="contained"
          disabled={primary.disabled === true}
          data-testid="entity-card-primary-action"
          aria-label={t('shared.entityList.primaryActionLabel', '{{action}} {{name}}', { action: primary.label, name: item.name })}
          onClick={(event) => {
            event.stopPropagation();
            primary.onClick();
          }}
        >
          {primary.label}
        </Button>
      )}
    </Box>
  );
}
