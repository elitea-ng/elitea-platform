import type { ReactNode } from 'react';

import CloseIcon from '@mui/icons-material/Close';
import KeyboardDoubleArrowLeftIcon from '@mui/icons-material/KeyboardDoubleArrowLeft';
import KeyboardDoubleArrowRightIcon from '@mui/icons-material/KeyboardDoubleArrowRight';
import SearchIcon from '@mui/icons-material/Search';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { NewFolderIcon } from '@/shared/ui/icons/new-folder-icon';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';

interface BucketPanelHeaderProps {
  readonly collapsed: boolean;
  readonly searchOpen: boolean;
  readonly query: string;
  readonly onQueryChange: (value: string) => void;
  readonly onSearchOpen: () => void;
  readonly onSearchClose: () => void;
  readonly onCreate: () => void;
  readonly onToggleCollapsed: () => void;
}

/**
 * The "BUCKETS" title row — uppercase `subtitle`, the two 28px round actions
 * (new bucket, search) and the collapse chevron pinned to the right — plus the
 * search field the second action reveals.
 *
 * Ported from `BucketHeader.jsx` + `BucketSearch.jsx`. Measured on
 * next.elitea.ai: 59.2px tall, `28px 24px 24px`, under a `border.lines` rule.
 */
export function BucketPanelHeader(props: BucketPanelHeaderProps): ReactNode {
  const createLabel = t('artifacts.buckets.create', 'Create bucket');
  const searchLabel = t('artifacts.buckets.searchBuckets', 'Search buckets');

  return (
    <>
      <Box sx={headerSx(props.collapsed)}>
        <Box sx={actionsSx}>
          {!props.collapsed && (
            <>
              <Typography
                variant="subtitle"
                color="text.primary"
              >
                {t('artifacts.buckets.title', 'Buckets')}
              </Typography>
              <Tooltip
                title={createLabel}
                placement="top"
              >
                <IconButton
                  color="secondary"
                  aria-label={createLabel}
                  sx={roundButtonSx}
                  onClick={props.onCreate}
                >
                  <NewFolderIcon style={smallIconStyle} />
                </IconButton>
              </Tooltip>
              <Tooltip
                title={searchLabel}
                placement="top"
              >
                <IconButton
                  color="secondary"
                  aria-label={searchLabel}
                  sx={roundButtonSx}
                  onClick={props.onSearchOpen}
                >
                  <SearchIcon
                    fontSize="small"
                    sx={smallIconSx}
                  />
                </IconButton>
              </Tooltip>
            </>
          )}
        </Box>
        <IconButton
          color="tertiary"
          aria-label={props.collapsed
            ? t('artifacts.buckets.expandPanel', 'Expand buckets panel')
            : t('artifacts.buckets.collapsePanel', 'Collapse buckets panel')}
          onClick={props.onToggleCollapsed}
        >
          {props.collapsed
            ? <KeyboardDoubleArrowRightIcon fontSize="small" />
            : <KeyboardDoubleArrowLeftIcon fontSize="small" />}
        </IconButton>
      </Box>
      {props.searchOpen && !props.collapsed && (
        <Box sx={searchRowSx}>
          <SimpleSearchBar
            value={props.query}
            debounceMs={0}
            onChange={props.onQueryChange}
            placeholder={t('artifacts.buckets.search', 'Search buckets')}
          />
          <IconButton
            color="tertiary"
            aria-label={t('artifacts.buckets.closeSearch', 'Close search')}
            onClick={props.onSearchClose}
          >
            <CloseIcon fontSize="small" />
          </IconButton>
        </Box>
      )}
    </>
  );
}

const smallIconStyle = { width: '1rem', height: '1rem' };
const headerSx = (collapsed: boolean): SxProps<Theme> => (theme) => ({
  display: 'flex',
  flexDirection: 'row',
  justifyContent: collapsed ? 'center' : 'space-between',
  alignItems: 'center',
  flexShrink: 0,
  height: '3.7rem',
  boxSizing: 'border-box',
  padding: theme.spacing(3.5, 3, 3),
  borderBottom: collapsed ? 'none' : `0.0625rem solid ${theme.vars.palette.border.lines}`,
});
const actionsSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexDirection: 'row',
  alignItems: 'center',
  gap: theme.spacing(1),
});
const roundButtonSx: SxProps<Theme> = (theme) => ({
  minWidth: '1.75rem',
  width: '1.75rem',
  height: '1.75rem',
  boxSizing: 'border-box',
  padding: theme.spacing(0.75),
});
const smallIconSx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.icon.fill.secondary });
const searchRowSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1),
  padding: theme.spacing(1.5, 1.6, 0),
});
