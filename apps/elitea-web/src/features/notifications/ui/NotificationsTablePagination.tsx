/**
 * The Notifications table's footer: page size, "start - end of total", and
 * the two arrows. Ported from `EliteaUI/src/[fsd]/entities/grid-table/ui/
 * GridTablePagination.jsx`.
 *
 * Its own file because `NotificationsTable.tsx` is at the 400-line budget
 * without it, and because the footer is the part with arithmetic in it —
 * `../lib/table.ts`'s `pageRange` is unit-tested against exactly the numbers
 * this renders.
 */
import { memo, useCallback } from 'react';

import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import SvgIcon from '@mui/material/SvgIcon';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { SingleSelect } from '@/shared/ui/SingleSelect';
import { ArrowLeftIcon } from '@/shared/ui/icons/arrow-left-icon';
import { ArrowRightIcon } from '@/shared/ui/icons/arrow-right-icon';

import { NOTIFICATION_PAGE_SIZE_OPTIONS, pageRange } from '../lib/table';

export interface NotificationsTablePaginationProps {
  readonly page: number;
  readonly pageSize: number;
  readonly total: number;
  readonly onPageChange: (page: number) => void;
  readonly onPageSizeChange: (pageSize: number) => void;
}

export const NotificationsTablePagination = memo(function NotificationsTablePagination({
  page,
  pageSize,
  total,
  onPageChange,
  onPageSizeChange,
}: NotificationsTablePaginationProps) {
  const { startRow, endRow, isFirstPage, isLastPage } = pageRange(page, pageSize, total);

  const handlePrev = useCallback(() => onPageChange(page - 1), [onPageChange, page]);
  const handleNext = useCallback(() => onPageChange(page + 1), [onPageChange, page]);
  const handlePageSize = useCallback(
    (value: string) => onPageSizeChange(Number(value)),
    [onPageSizeChange],
  );

  return (
    <Box sx={footerSx}>
      <Box sx={footerInnerSx}>
        <Box sx={footerLeftSx}>
          <Typography variant="bodyMedium" sx={footerLabelSx}>
            {t('routes.settings.notifications.pageSize', 'Rows per page:')}
          </Typography>
          <SingleSelect
            value={String(pageSize)}
            onChange={handlePageSize}
            options={NOTIFICATION_PAGE_SIZE_OPTIONS.map((size) => ({
              value: String(size),
              label: String(size),
            }))}
            label={t('routes.settings.notifications.pageSize', 'Rows per page:')}
            sx={pageSizeSelectSx}
          />
        </Box>
        <Typography variant="bodyMedium" sx={pageInfoSx}>
          {`${startRow} - ${endRow} of ${total}`}
        </Typography>
        <Box sx={footerRightSx}>
          <IconButton
            onClick={handlePrev}
            disabled={isFirstPage}
            size="small"
            aria-label={t('routes.settings.notifications.previousPage', 'Previous page')}
          >
            <SvgIcon component={ArrowLeftIcon} inheritViewBox sx={arrowIconSx} />
          </IconButton>
          <IconButton
            onClick={handleNext}
            disabled={isLastPage}
            size="small"
            aria-label={t('routes.settings.notifications.nextPage', 'Next page')}
          >
            <SvgIcon component={ArrowRightIcon} inheritViewBox sx={arrowIconSx} />
          </IconButton>
        </Box>
      </Box>
    </Box>
  );
});

const footerSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexShrink: 0,
  paddingTop: '1rem',
  borderTop: `0.0625rem solid ${theme.vars.palette.border.lines}`,
});

/* `GridTablePagination.jsx`'s `paddingContent`: `1.625rem` between the three
 * groups, `1rem` in from the left edge. */
const footerInnerSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  gap: '1.625rem',
  paddingLeft: '1rem',
};

const footerLeftSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.5rem' };

const footerLabelSx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.text.primary });

const pageSizeSelectSx: SxProps<Theme> = { minWidth: '4.5rem' };

const pageInfoSx: SxProps<Theme> = (theme) => ({
  minWidth: '4.5rem',
  textAlign: 'center',
  color: theme.vars.palette.text.secondary,
});

const footerRightSx: SxProps<Theme> = { display: 'flex', alignItems: 'center' };

const arrowIconSx: SxProps<Theme> = { width: '1rem', height: '1rem' };
