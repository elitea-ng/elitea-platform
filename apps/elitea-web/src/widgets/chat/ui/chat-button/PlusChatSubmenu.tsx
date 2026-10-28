import { memo, useCallback, useEffect, useRef } from 'react';

import { t } from '@/shared/i18n';

import AddIcon from '@mui/icons-material/Add';
import SearchIcon from '@mui/icons-material/Search';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import InputBase from '@mui/material/InputBase';
import MenuList from '@mui/material/MenuList';
import Switch from '@mui/material/Switch';
import Typography from '@mui/material/Typography';

import { MenuRow, MenuRowIcon, MenuRowText, menuRowSx } from '@/shared/ui/native-menu';

import type { Theme } from '@mui/material/styles';

/**
 * Submenu used inside PlusChatButton for searching and selecting items
 * (agents, pipelines, toolkits, attachments, tools).
 */
export interface PlusChatSubmenuProps {
  /** A defined `checked` value renders a switch. Agent and pipeline rows omit it. */
  items?: { key: string; label: string; onClick?: () => void; checked?: boolean; pending?: boolean }[];
  searchValue?: string;
  onSearchChange?: (e: React.ChangeEvent<HTMLInputElement>) => void;
  searchPlaceholder?: string;
  onCreateNew?: (() => void) | undefined;
  createNewLabel?: string;
  showCreateNew?: boolean;
  isLoading?: boolean;
  emptyMessage?: string;
  noResultsMessage?: string;
  onScroll?: (e: React.SyntheticEvent<HTMLDivElement>) => void;
  showToggle?: boolean;
}

type PlusChatSubmenuItem = NonNullable<PlusChatSubmenuProps['items']>[number];

/**
 * Case-insensitive substring filter of `items` by `label`; an empty
 * `searchValue` short-circuits to `items` unchanged.
 *
 * Split out (along with `submenuStatusLabel` below) purely to keep the
 * component under the §3.5 cyclomatic-complexity-12 budget — a helper
 * function has its OWN complexity budget, so pulling these branches out of
 * the component body buys back the headroom the props' own default values
 * consume (each destructured default is itself counted as a branch).
 */
function filterSubmenuItems(items: PlusChatSubmenuItem[], searchValue: string): PlusChatSubmenuItem[] {
  return searchValue
    ? items.filter((item) => item.label.toLowerCase().includes(searchValue.toLowerCase()))
    : items;
}

/**
 * The quiet status line shown in place of the items list — while loading, or once the (possibly filtered) list is
 * empty — and `null` when the items list itself should render instead.
 */
function submenuStatusLabel(
  isLoading: boolean,
  hasItems: boolean,
  searchValue: string,
  emptyMessage: string,
  noResultsMessage: string,
): string | null {
  if (isLoading) {
    return t('widgets.chat.plusChatSubmenu.loadingLabel', 'Loading...');
  }
  if (hasItems) {
    return null;
  }
  return searchValue ? noResultsMessage : emptyMessage;
}

export const PlusChatSubmenu = memo(
  ({
    items = [],
    searchValue = '',
    onSearchChange,
    searchPlaceholder = t('widgets.chat.plusChatSubmenu.searchPlaceholder', 'Search...'),
    onCreateNew,
    createNewLabel = t('widgets.chat.plusChatSubmenu.createNewLabel', 'Create new'),
    showCreateNew = false,
    isLoading = false,
    emptyMessage = t('widgets.chat.plusChatSubmenu.emptyMessage', 'No items available'),
    noResultsMessage = t('widgets.chat.plusChatSubmenu.noResultsMessage', 'No items found'),
    onScroll,
    showToggle,
  }: PlusChatSubmenuProps) => {
    const searchRef = useRef<HTMLInputElement>(null);

    // Auto-focus search input when submenu opens
    useEffect(() => {
      searchRef.current?.focus();
    }, []);

    const handleItemClick = useCallback((item: { onClick?: () => void }) => () => {
      item.onClick?.();
    }, []);

    const filteredItems = filterSubmenuItems(items, searchValue);
    const statusLabel = submenuStatusLabel(
      isLoading,
      filteredItems.length > 0,
      searchValue,
      emptyMessage,
      noResultsMessage,
    );

    return (
      <Box>
        {/*
          * The header: search, then the "Create …" row (#6629). Both stay at
          * the top while the list below scrolls, so "Create Agent" is one
          * click away however long the list is. The search is the project
          * picker's plain field (no underline), the create row a native menu
          * row; it is a button, not a `MenuItem`, because it sits outside
          * the `MenuList` below and a `MenuItem` throws without that list's
          * context.
          */}
        <Box data-testid="plus-submenu-header">
          <InputBase
            inputRef={searchRef}
            fullWidth
            placeholder={searchPlaceholder}
            value={searchValue}
            onChange={onSearchChange}
            inputProps={{ 'aria-label': searchPlaceholder }}
            startAdornment={<SearchIcon aria-hidden data-testid="plus-submenu-search-icon" sx={submenuIconSx} />}
            sx={(theme: Theme) => ({
              ...theme.typography.labelSmall,
              gap: 1,
              paddingX: 1.5,
              paddingY: 1,
              borderBottom: `1px solid ${theme.vars.palette.divider}`,
            })}
          />
          {showCreateNew && (
            <Box sx={{ paddingX: 0.5, paddingTop: 0.5 }}>
              <ButtonBase
                onClick={onCreateNew}
                data-testid="plus-submenu-create-new"
                sx={(theme: Theme) => ({ ...menuRowSx(theme), width: '100%', justifyContent: 'flex-start' })}
              >
                <MenuRowIcon>
                  <AddIcon aria-hidden data-testid="plus-submenu-create-icon" sx={submenuIconSx} />
                </MenuRowIcon>
                <MenuRowText>{createNewLabel}</MenuRowText>
              </ButtonBase>
            </Box>
          )}
        </Box>

        {/*
          * #6629: one fixed height for every submenu, the Modules list's.
          * The lists used to size to their rows, so each category opened at
          * a different size.
          */}
        <Box
          data-testid="plus-submenu-list"
          sx={{
            height: '20.3125rem',
            overflowY: 'auto',
            display: 'flex',
            flexDirection: 'column',
            padding: 0.5,
          }}
          onScroll={onScroll}
        >
          {/*
            * `MenuList`, not a plain `Box`: MUI 9.2's `MenuItem` reads
            * `MenuListContext` unconditionally and THROWS
            * ("MUI: MenuListContext is missing…") when it is absent, so a
            * `MenuItem` rendered under a bare `Popper`/`Paper` crashes the
            * whole chat on the first submenu open. `MenuList` is the
            * provider; it also gives the rows arrow-key roving focus.
            */}
          <MenuList disablePadding sx={{ outline: 'none' }}>
            {filteredItems.map((item) => (
              <MenuRow
                key={item.key}
                onClick={handleItemClick(item)}
                disabled={item.pending === true}
                // `data-item-key` is the same identity the list is keyed on,
                // which is what lets a test address ONE row when two entities
                // share a display name.
                data-testid="plus-submenu-item"
                data-item-key={item.key}
              >
                {item.checked !== undefined && (
                  <Switch
                    size="small"
                    checked={item.checked}
                    disabled={item.pending === true}
                    onClick={(e) => e.stopPropagation()}
                    onChange={handleItemClick(item)}
                  />
                )}
                <MenuRowText>{item.label}</MenuRowText>
              </MenuRow>
            ))}
          </MenuList>

          {/* Loading / empty / no-results: quiet text in the list, not a row. */}
          {statusLabel !== null && (
            <Typography
              variant="bodySmall"
              sx={(theme: Theme) => ({ paddingX: 1, paddingY: 1, color: theme.vars.palette.text.metrics })}
            >
              {statusLabel}
            </Typography>
          )}

          {/* Toggle placeholder (reserved for future use) */}
          {showToggle && (
            <Typography variant="bodySmall" sx={(theme: Theme) => ({ paddingX: 1, paddingY: 1, color: theme.vars.palette.text.disabled })}>
              {t('widgets.chat.plusChatSubmenu.togglePlaceholder', 'Toggle options coming soon')}
            </Typography>
          )}
        </Box>
      </Box>
    );
  },
);

PlusChatSubmenu.displayName = 'PlusChatSubmenu';

/** #6629: the search and plus glyphs at 16px, in the menu icons' quiet colour. */
const submenuIconSx = (theme: Theme) => ({
  width: '1rem',
  height: '1rem',
  flexShrink: 0,
  color: theme.vars.palette.text.metrics,
});
