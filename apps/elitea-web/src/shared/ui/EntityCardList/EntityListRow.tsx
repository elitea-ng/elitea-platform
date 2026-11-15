import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { CalendarIcon } from '@/shared/ui/icons/calendar-icon';

import { EntityCardAuthors } from './EntityCardAuthors';
import type { EntityListItem } from './model';
import type { EntityRowSlot } from './rowSlot';
import { useSelectableRow } from './selection';

interface TableRowProps {
  readonly item: EntityListItem;
  readonly columns: string;
  readonly showType: boolean;
  readonly showStatus: boolean;
  readonly slot: EntityRowSlot | undefined;
}

/** One row of the table: the selection checkbox, the name cell, then the columns the list has. */
export function TableRow({ item, columns, showType, showStatus, slot }: TableRowProps): ReactNode {
  const row = useSelectableRow(item.id, item.onClick);
  return (
    <Box
      component="tr"
      data-testid="entity-list-row"
      data-selected={row.selected ? 'true' : undefined}
      sx={rowSx(columns, item.onClick !== undefined)}
      onClick={row.activate}
    >
      {row.selectable && (
        <Box
          component="td"
          sx={checkCellSx}
          onClick={(event) => {
            event.stopPropagation();
          }}
        >
          <BaseCheckbox
            size="small"
            checked={row.selected}
            data-testid="entity-row-select"
            aria-label={t('shared.entityList.select', 'Select {{name}}', { name: item.name })}
            onChange={row.toggle}
          />
        </Box>
      )}
      <NameCell item={item} />
      {showStatus && (
        <Box
          component="td"
          sx={cellSx}
        >
          <Typography
            variant="bodyMedium"
            data-testid="entity-row-status"
            sx={clampSx}
          >
            {item.badge ?? ''}
          </Typography>
        </Box>
      )}
      {showType && (
        <Box
          component="td"
          sx={cellSx}
        >
          <Typography
            variant="bodyMedium"
            sx={clampSx}
          >
            {item.typeLabel ?? ''}
          </Typography>
        </Box>
      )}
      <Box
        component="td"
        sx={cellSx}
      >
        <EntityCardAuthors authors={item.authors ?? []} />
      </Box>
      <Box
        component="td"
        sx={createdCellSx}
      >
        {item.createdAt !== undefined && (
          <>
            <Box
              component={CalendarIcon}
              sx={calendarIconSx}
            />
            <Typography
              variant="bodyMedium"
              component="span"
            >
              {formatCreated(item.createdAt)}
            </Typography>
          </>
        )}
      </Box>
      {slot !== undefined && (
        <Box
          component="td"
          data-testid="entity-row-slot"
          sx={cellSx}
          onClick={(event) => {
            event.stopPropagation();
          }}
        >
          {slot.renderCell(item)}
        </Box>
      )}
    </Box>
  );
}

/** The name cell: the icon tile, the name, the description and the "Forked from" link. */
function NameCell({ item }: { readonly item: EntityListItem }): ReactNode {
  return (
      <Box
    component="td"
    sx={nameCellSx}
  >
    <Box sx={rowIconSx}>{item.icon}</Box>
    <Box sx={nameTextSx}>
      <Typography
        variant="bodyMedium"
        sx={rowNameSx}
      >
        {item.name}
      </Typography>
      {item.description !== undefined && item.description !== '' && (
        <Typography
          variant="bodySmall"
          sx={rowDescriptionSx}
        >
          {item.description}
        </Typography>
      )}
      {item.forkedFrom !== undefined && (
        <Box
          component="span"
          // oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- same client-side-only-navigation reasoning as `EntityCard.tsx`'s own copy of this link.
          role="link"
          tabIndex={0}
          data-testid="entity-row-forked-from"
          sx={forkedFromSx}
          onClick={(event) => {
            event.stopPropagation();
            item.forkedFrom?.onClick();
          }}
          onKeyDown={(event) => {
            if (event.key !== 'Enter' && event.key !== ' ') return;
            event.preventDefault();
            event.stopPropagation();
            item.forkedFrom?.onClick();
          }}
        >
          {t('shared.entityList.forkedFrom', 'Forked from')}
        </Box>
      )}
    </Box>
  </Box>
  );
}

/** `TIME_FORMAT.MMMDD` (`apps/elitea-ui/src/common/constants.js`) — "Mar 14". */
function formatCreated(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  return date.toLocaleDateString(undefined, { month: 'short', day: '2-digit' });
}

function rowSx(columns: string, clickable: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    display: 'grid',
    gridTemplateColumns: columns,
    alignItems: 'center',
    width: '100%',
    height: '3.5rem',
    minHeight: '3.5rem',
    flexShrink: 0,
    cursor: clickable ? 'pointer' : 'default',
    borderBottom: `0.0625rem solid ${theme.vars.palette.border.table}`,
    transition: 'background-color 0.2s ease',
    '&:hover, &[data-selected="true"]': { backgroundColor: theme.vars.palette.background.userInputBackground },
    '&:last-of-type': { borderBottom: 'none' },
  });
}

export const checkCellSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 0 };

const cellSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  padding: `${theme.spacing(0.75)} ${theme.spacing(2)}`,
  minWidth: 0,
  overflow: 'hidden',
});

const nameCellSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1.5),
  padding: `${theme.spacing(0.75)} ${theme.spacing(2)}`,
  minWidth: 0,
  overflow: 'hidden',
});

const rowIconSx: SxProps<Theme> = (theme: Theme) => ({
  flexShrink: 0,
  width: '1.75rem',
  height: '1.75rem',
  borderRadius: theme.vars.shape.radiusPill,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  background: theme.vars.palette.background.icon.entityGradient,
  color: theme.vars.palette.text.primary,
  '& > svg': { width: '0.875rem', height: '0.875rem' },
});

const nameTextSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', minWidth: 0 };

const clampSx: SxProps<Theme> = {
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const rowNameSx: SxProps<Theme> = (theme: Theme) => ({ ...clampSx, color: theme.vars.palette.text.secondary });

const rowDescriptionSx: SxProps<Theme> = (theme: Theme) => ({ ...clampSx, color: theme.vars.palette.text.primary });

/** #915's "Forked from" link, same treatment as `EntityCard.tsx`'s own copy. */
const forkedFromSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'inline-block',
  color: theme.vars.palette.text.link,
  cursor: 'pointer',
  fontSize: theme.typography.bodySmall.fontSize,
  '&:hover': { textDecoration: 'underline' },
  '&:focus-visible': { outline: `0.125rem solid ${theme.vars.palette.border.lines}`, outlineOffset: '0.125rem' },
});

const createdCellSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  gap: theme.spacing(1),
  padding: `${theme.spacing(0.75)} ${theme.spacing(2)}`,
  minWidth: 0,
});

const calendarIconSx: SxProps<Theme> = { width: '1rem', height: '1rem', flexShrink: 0 };
