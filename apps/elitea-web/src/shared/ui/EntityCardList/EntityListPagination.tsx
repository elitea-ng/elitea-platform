import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { ArrowLeftIcon } from '@/shared/ui/icons/arrow-left-icon';
import { ArrowRightIcon } from '@/shared/ui/icons/arrow-right-icon';

/**
 * A list page's pagination footer — ported from
 * `apps/elitea-ui/src/[fsd]/entities/grid-table/ui/GridTablePagination.jsx`:
 * a `border.lines` top rule with `1rem` of padding above the controls, then
 * "Rows per page:" + a borderless page-size select, the `start - end of
 * total` counter (min-width `4.5rem`, centred), and prev/next arrow
 * buttons. Renders nothing at all when there are no rows, exactly as the
 * baseline does.
 */
export const PAGE_SIZE_OPTIONS: readonly number[] = [10, 20, 50, 100];

export interface EntityListPaginationProps {
  readonly page: number;
  readonly pageSize: number;
  readonly total: number;
  readonly onPageChange: (page: number) => void;
  readonly onPageSizeChange: (pageSize: number) => void;
}

export function EntityListPagination({ page, pageSize, total, onPageChange, onPageSizeChange }: EntityListPaginationProps): ReactNode {
  if (total === 0) return null;
  const isFirstPage = page === 0;
  const isLastPage = page * pageSize + pageSize >= total;
  const startRow = page * pageSize + 1;
  const endRow = Math.min(page * pageSize + pageSize, total);

  return (
    <Box
      sx={footerSx}
      data-testid="entity-list-pagination"
    >
      <Box sx={contentSx}>
        <Box sx={leftSx}>
          <Typography variant="bodyMedium">{t('shared.entityList.pagination.rowsPerPage', 'Rows per page:')}</Typography>
          <Select
            value={pageSize}
            variant="standard"
            disableUnderline
            aria-label={t('shared.entityList.pagination.rowsPerPage', 'Rows per page:')}
            onChange={(event) => {
              onPageSizeChange(Number(event.target.value));
            }}
            sx={selectSx}
          >
            {PAGE_SIZE_OPTIONS.map((option) => (
              <MenuItem
                key={option}
                value={option}
              >
                {option}
              </MenuItem>
            ))}
          </Select>
        </Box>
        <Typography
          variant="bodyMedium"
          data-testid="entity-list-page-info"
          sx={pageInfoSx}
        >
          {`${String(startRow)} - ${String(endRow)} of ${String(total)}`}
        </Typography>
        <Box sx={rightSx}>
          <IconButton
            size="small"
            disabled={isFirstPage}
            aria-label={t('shared.entityList.pagination.previous', 'Previous page')}
            onClick={() => {
              onPageChange(page - 1);
            }}
            sx={arrowButtonSx}
          >
            <Box
              component={ArrowLeftIcon}
              sx={arrowIconSx}
            />
          </IconButton>
          <IconButton
            size="small"
            disabled={isLastPage}
            aria-label={t('shared.entityList.pagination.next', 'Next page')}
            onClick={() => {
              onPageChange(page + 1);
            }}
            sx={arrowButtonSx}
          >
            <Box
              component={ArrowRightIcon}
              sx={arrowIconSx}
            />
          </IconButton>
        </Box>
      </Box>
    </Box>
  );
}

const footerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  paddingTop: theme.spacing(2),
  borderTop: `0.0625rem solid ${theme.vars.palette.border.lines}`,
});

const contentSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: '1.625rem',
  paddingLeft: theme.spacing(2),
});

const leftSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', alignItems: 'center', gap: theme.spacing(1) });

const selectSx: SxProps<Theme> = { minWidth: '1.75rem' };

const pageInfoSx: SxProps<Theme> = (theme: Theme) => ({
  minWidth: '4.5rem',
  textAlign: 'center',
  color: theme.vars.palette.text.secondary,
});

const rightSx: SxProps<Theme> = { display: 'flex', alignItems: 'center' };

const arrowButtonSx: SxProps<Theme> = (theme: Theme) => ({
  padding: theme.spacing(0.75),
  minWidth: 0,
  '&.Mui-disabled': { opacity: 0.4 },
});

const arrowIconSx: SxProps<Theme> = { width: '1rem', height: '1rem' };
