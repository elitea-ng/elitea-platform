import { memo, useCallback, useEffect, useRef } from 'react';

import { t } from '@/shared/i18n';

import Box from '@mui/material/Box';
import MenuItem from '@mui/material/MenuItem';
import MenuList from '@mui/material/MenuList';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

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
 * Label for the single disabled status `MenuItem` shown in place of the
 * items list — while loading, or once the (possibly filtered) list is
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
        {/* Search bar */}
        <Box
          sx={{
            padding: '0.25rem 1rem',
            borderBottom: '0.0625rem solid',
            borderColor: 'border.lines',
          }}
        >
          <TextField
            inputRef={searchRef}
            size="small"
            placeholder={searchPlaceholder}
            value={searchValue}
            onChange={onSearchChange}
            variant="standard"
            sx={{ color: 'text.primary' }}
          />
        </Box>

        {/* Items list */}
        <Box
          sx={{
            maxHeight: '20.3125rem',
            overflowY: 'auto',
            display: 'flex',
            flexDirection: 'column',
          }}
          onScroll={onScroll}
        >
          {/*
            * `MenuList`, not a plain `Box`: MUI 9.2's `MenuItem` reads
            * `MenuListContext` unconditionally and THROWS
            * ("MUI: MenuListContext is missing…") when it is absent, so a
            * `MenuItem` rendered under a bare `Popper`/`Paper` crashes the
            * whole chat on the first submenu open. `MenuList` is the
            * provider (same fix already in place in
            * `ChatInternalToolsConfigButton.tsx` and
            * `features/pipelines/ui/FStringAutocompletePopper.tsx`); it also
            * restores the arrow-key roving focus these rows should have had.
            * `disablePadding` keeps the previous flush-to-the-edges spacing.
            */}
          <MenuList disablePadding sx={{ outline: 'none' }}>
            {/* Create new button */}
            {showCreateNew && (
              <MenuItem
                onClick={onCreateNew}
                data-testid="plus-submenu-create-new"
                sx={{
                  padding: '0.5rem 1.25rem',
                  height: '2.5rem',
                  gap: 0.75,
                  color: 'text.secondary',
                  '&:hover': {
                    backgroundColor: 'action.hover',
                  },
                }}
              >
                <Typography variant="bodyMedium">{createNewLabel}</Typography>
              </MenuItem>
            )}

            {/* Items */}
            {filteredItems.map((item) => (
              <MenuItem
                key={item.key}
                onClick={handleItemClick(item)}
                disabled={item.pending === true}
                // The rows carry no accessible name of their own — the label is
                // a nested `Typography`, so the accessibility tree reports an
                // unnamed `menuitem` and a journey can only reach them by text
                // content. `data-item-key` is the same identity the list is
                // keyed on, which is what lets a test address ONE row when two
                // entities share a display name.
                data-testid="plus-submenu-item"
                data-item-key={item.key}
                sx={{
                  padding: '0.5rem 1rem',
                  height: '2.5rem',
                  gap: 1,
                  color: 'text.secondary',
                  '&:hover': {
                    backgroundColor: 'action.hover',
                  },
                }}
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
                <Typography variant="bodyMedium">{item.label}</Typography>
              </MenuItem>
            ))}

            {/* Loading / empty / no-results status row */}
            {statusLabel !== null && (
              <MenuItem disabled sx={{ padding: '0.5rem 1rem' }}>
                <Typography variant="bodyMedium" color="text.secondary">
                  {statusLabel}
                </Typography>
              </MenuItem>
            )}

          </MenuList>

          {/* Toggle placeholder (reserved for future use) */}
          {showToggle && (
            <Box
              sx={(theme: Theme) => ({
                padding: '0.5rem 1rem',
                color: theme.vars.palette.text.disabled,
                fontSize: theme.typography.bodySmall.fontSize,
              })}
            >
              {t('widgets.chat.plusChatSubmenu.togglePlaceholder', 'Toggle options coming soon')}
            </Box>
          )}
        </Box>
      </Box>
    );
  },
);

PlusChatSubmenu.displayName = 'PlusChatSubmenu';
