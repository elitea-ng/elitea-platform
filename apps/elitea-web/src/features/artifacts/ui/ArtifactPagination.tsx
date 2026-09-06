import type { MouseEvent, ReactNode } from 'react';
import { useState } from 'react';

import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { ArrowLeftIcon } from '@/shared/ui/icons/arrow-left-icon';
import { ArrowRightIcon } from '@/shared/ui/icons/arrow-right-icon';

const PAGE_SIZES = [10, 25, 50, 100];

interface ArtifactPaginationProps {
  readonly total: number;
  readonly page: number;
  readonly pageSize: number;
  readonly onPageChange: (page: number) => void;
  readonly onPageSizeChange: (size: number) => void;
}

/**
 * The table footer: "Rows per page: N", the 1-based range, and the two arrows.
 *
 * Ported from `GridTablePagination.jsx` — left-aligned above a full-width top
 * rule, NOT MUI's own `TablePagination`, which right-aligns and prints an
 * en-dash range ("1–1 of 1") the baseline never shows.
 */
export function ArtifactPagination(props: ArtifactPaginationProps): ReactNode {
  const [sizeAnchor, setSizeAnchor] = useState<HTMLElement>();
  if (props.total === 0) return null;
  const firstPage = props.page === 0;
  const lastPage = (props.page + 1) * props.pageSize >= props.total;
  const startRow = props.page * props.pageSize + 1;
  const endRow = Math.min((props.page + 1) * props.pageSize, props.total);

  return (
    <Box sx={footerSx}>
      <Box sx={contentSx}>
        <Box sx={leftSx}>
          <Typography
            variant="bodyMedium"
            color="text.primary"
          >
            {t('artifacts.table.rowsPerPage', 'Rows per page:')}
          </Typography>
          <Box
            component="button"
            type="button"
            aria-haspopup="listbox"
            aria-label={t('artifacts.table.rowsPerPage', 'Rows per page:')}
            sx={sizeButtonSx}
            onClick={(event: MouseEvent<HTMLElement>) => setSizeAnchor(event.currentTarget)}
          >
            <Typography variant="bodyMedium">{props.pageSize}</Typography>
            <ExpandMoreIcon fontSize="small" />
          </Box>
          <Menu
            anchorEl={sizeAnchor}
            open={sizeAnchor !== undefined}
            onClose={() => setSizeAnchor(undefined)}
          >
            {PAGE_SIZES.map((size) => (
              <MenuItem
                key={size}
                selected={size === props.pageSize}
                onClick={() => {
                  props.onPageSizeChange(size);
                  setSizeAnchor(undefined);
                }}
              >
                {size}
              </MenuItem>
            ))}
          </Menu>
        </Box>
        <Typography
          variant="bodyMedium"
          sx={rangeSx}
        >
          {`${startRow} - ${endRow} of ${props.total}`}
        </Typography>
        <Box sx={arrowsSx}>
          <IconButton
            size="small"
            color="tertiary"
            disabled={firstPage}
            aria-label={t('artifacts.table.previousPage', 'Go to previous page')}
            onClick={() => props.onPageChange(props.page - 1)}
          >
            <ArrowLeftIcon style={arrowStyle} />
          </IconButton>
          <IconButton
            size="small"
            color="tertiary"
            disabled={lastPage}
            aria-label={t('artifacts.table.nextPage', 'Go to next page')}
            onClick={() => props.onPageChange(props.page + 1)}
          >
            <ArrowRightIcon style={arrowStyle} />
          </IconButton>
        </Box>
      </Box>
    </Box>
  );
}

const arrowStyle = { width: '1rem', height: '1rem' };
const footerSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexShrink: 0,
  paddingTop: theme.spacing(2),
  marginTop: theme.spacing(1.5),
  marginInline: theme.spacing(3),
  marginBottom: theme.spacing(2),
  borderTop: `0.0625rem solid ${theme.vars.palette.border.lines}`,
});
const contentSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  flexWrap: 'nowrap',
  gap: theme.spacing(3.25),
  paddingLeft: theme.spacing(2),
});
const leftSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  flexShrink: 0,
  whiteSpace: 'nowrap',
  gap: theme.spacing(1),
});
const rangeSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.text.secondary,
  minWidth: '4.5rem',
  flexShrink: 0,
  whiteSpace: 'nowrap',
  textAlign: 'center',
});
const arrowsSx: SxProps<Theme> = { display: 'flex', alignItems: 'center' };
/**
 * The page-size control is a plain button + `Menu`, not `shared/ui`'s
 * `SingleSelect`: that component is hard-wired to MUI's `standard` variant,
 * whose underline the baseline turns off with its own `showBorder={false}`
 * prop — which this port does not have, and which cannot be reached from a
 * call site either (R-T6 bans the `.MuiInput-underline` selector that would
 * hide it, and `SingleSelect` is already at the 12-prop budget, so a
 * `showBorder` prop cannot be added to it).
 */
const sizeButtonSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(0.25),
  minWidth: '2.75rem',
  padding: theme.spacing(0.25, 0.25, 0.25, 0.5),
  border: 0,
  appearance: 'none',
  background: 'transparent',
  color: theme.vars.palette.text.secondary,
  cursor: 'pointer',
});
