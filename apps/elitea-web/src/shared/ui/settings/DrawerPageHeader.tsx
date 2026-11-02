import { memo, useCallback } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { AddButton } from '../AddButton/AddButton';
import { combineSx } from '../lib/combineSx';
import { PageCrumbFallback, useHasPageChrome, usePageCrumbs } from '../page-chrome';
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
  title: string;
  showSearchInput?: boolean;
  showAddButton?: boolean;
  extraContent?: React.ReactNode;
  slotProps?: DrawerPageHeaderSlotProps;
}

/**
 * A settings page's toolbar: optional search, extra content and add button.
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/ui/drawer-page/
 * DrawerPageHeader.jsx`.
 *
 * The title is the shell's: it is registered as the page's crumb
 * (`Settings ▸ Profile`, `shared/ui/page-chrome`) and drawn here only where
 * no shell title row exists (the desktop Settings dialog, a unit test). There
 * is no back button: the section crumb and the settings navigation cover it.
 */
// oxlint-disable-next-line eslint/complexity — ported from baseline
export const DrawerPageHeader = memo(function DrawerPageHeader({
  showBorder = false,
  sx,
  title,
  showSearchInput = false,
  showAddButton = false,
  extraContent,
  slotProps,
}: DrawerPageHeaderProps) {
  usePageCrumbs([{ label: title }]);
  const inShell = useHasPageChrome();
  const { search, onChangeSearch, placeholder } = slotProps?.searchInput ?? {};
  const { onAdd, tooltip: addButtonTooltip } = slotProps?.addButton ?? {};
  const styles = getStyles();

  const handleInputChange = useCallback(
    (value: string) => {
      onChangeSearch?.(value);
    },
    [onChangeSearch],
  );

  // Inside a shell the title is in the title row; with no search, add button
  // or extra content there is nothing left for this row to hold, and an empty
  // bordered band would sit over the page.
  if (inShell && !showSearchInput && !showAddButton && extraContent === undefined) return null;

  return (
    <Box
      sx={combineSx(styles.container(showBorder), sx)}
    >
      <PageCrumbFallback title={title} />
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
            placeholder={placeholder ?? t('shared.ui.settings.header.search', 'Search')}
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
  body: SxProps<Theme>;
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

  body: {
    flex: 1,
    height: '100%',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'flex-end',
    gap: '1rem',
  },
});
