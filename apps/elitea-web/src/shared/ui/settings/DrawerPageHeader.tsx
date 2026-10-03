import { memo, useCallback } from 'react';

import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import SvgIcon from '@mui/material/SvgIcon';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { ArrowLeftIcon } from '../icons/arrow-left-icon';
import { AddButton } from '../AddButton/AddButton';
import { combineSx } from '../lib/combineSx';
import { SimpleSearchBar } from '../SimpleSearchBar';
import { t } from '@/shared/i18n';

export interface DrawerPageHeaderSlotProps {
  searchInput?: {
    search: string;
    onChangeSearch: (value: string) => void;
    placeholder?: string;
  };
  addButton?: {
    onAdd?: () => void;
    disabled?: boolean;
    tooltip?: string;
    tourId?: string;
  };
}

export interface DrawerPageHeaderProps {
  showBorder?: boolean;
  sx?: SxProps<Theme>;
  showBackButton?: boolean;
  title: string;
  showSearchInput?: boolean;
  showAddButton?: boolean;
  extraContent?: React.ReactNode;
  slotProps?: DrawerPageHeaderSlotProps;
  onBack?: () => void;
}

/**
 * Fixed-height header with back button + title on the left, and optional
 * search, extra content, and add button on the right. Ported from
 * `apps/elitea-ui/src/[fsd]/features/settings/ui/drawer-page/DrawerPageHeader.jsx`.
 */
// oxlint-disable-next-line eslint/complexity — ported from baseline
export const DrawerPageHeader = memo(function DrawerPageHeader({
  showBorder = false,
  sx,
  showBackButton = false,
  title,
  showSearchInput = false,
  showAddButton = false,
  extraContent,
  slotProps,
  onBack,
}: DrawerPageHeaderProps) {
  const { search, onChangeSearch, placeholder } = slotProps?.searchInput ?? {};
  const { onAdd, tooltip: addButtonTooltip } = slotProps?.addButton ?? {};
  const styles = getStyles();

  const handleInputChange = useCallback(
    (value: string) => {
      onChangeSearch?.(value);
    },
    [onChangeSearch],
  );

  return (
    <Box
      sx={combineSx(styles.container(showBorder), sx)}
    >
      <Box sx={styles.titleContainer}>
        {showBackButton && (
          <IconButton
            color="tertiary"
            onClick={onBack}
            sx={styles.iconButton}
            aria-label={t('shared.ui.settings.header.back', 'Back')}
          >
            <SvgIcon
              component={ArrowLeftIcon}
              inheritViewBox
            />
          </IconButton>
        )}
        {/* The settings page's title: `headingLarge`, the one page-title size
            the app and the admin console share (typography spec §2). */}
        <Typography
          variant="headingLarge"
          color="text.secondary"
          component="h1"
        >
          {title}
        </Typography>
      </Box>
      <Box sx={styles.body}>
        {showSearchInput && (
          // #6646: the shared search box, not a bare `<input>`. The bare
          // input had no search glyph and no hover state, so Secrets, Tokens
          // and Notifications looked unlike every other search in the app.
          // `debounceMs={0}` keeps the caller's every-keystroke filtering.
          <SimpleSearchBar
            value={search ?? ''}
            onChange={handleInputChange}
            debounceMs={0}
            placeholder={placeholder ?? t('shared.ui.settings.header.searchPlaceholder', 'Search something amazing!')}
            aria-label={t('shared.ui.settings.header.search', 'Search')}
            sx={searchBarSx}
          />
        )}
        {extraContent}
        {showAddButton && (
          <AddButton
            {...(onAdd ? { onAdd } : {})}
            {...(addButtonTooltip ? { tooltip: addButtonTooltip } : {})}
          />
        )}
      </Box>
    </Box>
  );
});

const searchBarSx: SxProps<Theme> = { flexShrink: 0, width: '15rem' };

const getStyles = (): {
  container: (showBorder: boolean) => SxProps<Theme>;
  titleContainer: SxProps<Theme>;
  body: SxProps<Theme>;
  iconButton: SxProps<Theme>;
} => ({
  container:
    (showBorder: boolean): SxProps<Theme> =>
    (theme) => ({
      height: '3.8rem',
      minHeight: '3.8rem',
      width: '100%',
      borderBottom: showBorder ? `0.0625rem solid ${theme.vars.palette.border.table}` : undefined,
      boxSizing: 'border-box',
      display: 'flex',
      justifyContent: 'space-between',
      alignItems: 'center',
      padding: '0 1.5rem',
    }),

  titleContainer: {
    display: 'flex',
    alignItems: 'center',
    gap: '1rem',
  },

  body: {
    flex: 1,
    height: '100%',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'flex-end',
    gap: '1rem',
  },

  iconButton: (theme) => ({
    margin: '0',
    '&:hover svg path': {
      fill: theme.vars.palette.icon.fill.secondary,
    },
  }),
});
