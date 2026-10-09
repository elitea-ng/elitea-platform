import type { ReactNode, UIEvent } from 'react';
import { useCallback } from 'react';

import Box from '@mui/material/Box';
import ListItemIcon from '@mui/material/ListItemIcon';
import ListItemText from '@mui/material/ListItemText';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';

/**
 * Shared presentational dropdown behind each of `ToolMenu`'s four "add"
 * buttons (Toolkit/MCP/Agent/Pipeline) — one `Menu` composition instead of
 * four near-identical copies. DISCLOSED SIMPLIFICATION of the baseline's
 * `UnifiedDropdown` (`apps/elitea-ui/src/components/UnifiedDropdown`): that
 * component (Figma-exact dimensions, `DROPDOWN_CONSTANTS`) has no port
 * anywhere in `shared/ui` (S1's 67-component list does not include it —
 * verified by directory listing) and building one is a `shared/ui`-owned
 * concern outside this unit's ownership fence. This is a real, minimal
 * MUI `Menu`/`MenuItem` composition — theme-driven spacing (`theme.spacing`)
 * instead of the baseline's pixel-exact Figma tokens, same class of
 * disclosed layout simplification `McpAuthStatusBadge.tsx` already
 * documents for the same reason.
 */
export interface ToolMenuDropdownItem {
  readonly key: string;
  readonly label: string;
  readonly description?: string | undefined;
  readonly icon?: ReactNode;
  readonly onClick: () => void;
}

export interface ToolMenuDropdownProps {
  readonly anchorEl: HTMLElement | null;
  readonly onClose: () => void;
  readonly items: readonly ToolMenuDropdownItem[];
  readonly search: string;
  readonly onSearchChange: (value: string) => void;
  readonly searchPlaceholder: string;
  readonly isLoading?: boolean;
  readonly emptyMessage: string;
  readonly onCreateNew?: (() => void) | undefined;
  readonly createNewLabel?: string;
  readonly onScrollNearEnd?: (() => void) | undefined;
  /** Forwarded to `SimpleSearchBar`'s own `debounceMs` (default 300). Exposed mainly so tests can pass `0` for synchronous assertions. */
  readonly searchDebounceMs?: number;
}

const paperSx: SxProps<Theme> = { width: '17.5rem', maxHeight: '23.3rem' };
const searchBoxSx: SxProps<Theme> = { px: 1.5, pt: 1, pb: 0.5 };
const statusItemSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.secondary });

/** Fires `onScrollNearEnd` once the scroller is within 48px of its bottom — the infinite-scroll trigger for callers that supply one (today: only the toolkit-instance dropdowns, which are the only source with a real `limit`/`offset` to page through). */
function useNearEndScrollHandler(onScrollNearEnd: (() => void) | undefined) {
  return useCallback(
    (event: UIEvent<HTMLElement>) => {
      if (!onScrollNearEnd) return;
      const el = event.currentTarget;
      if (el.scrollHeight - el.scrollTop - el.clientHeight < 48) onScrollNearEnd();
    },
    [onScrollNearEnd],
  );
}

export function ToolMenuDropdown({
  anchorEl,
  onClose,
  items,
  search,
  onSearchChange,
  searchPlaceholder,
  isLoading = false,
  emptyMessage,
  onCreateNew,
  createNewLabel,
  onScrollNearEnd,
  searchDebounceMs,
}: ToolMenuDropdownProps): ReactNode {
  const handleScroll = useNearEndScrollHandler(onScrollNearEnd);

  return (
    <Menu
      anchorEl={anchorEl}
      open={Boolean(anchorEl)}
      onClose={onClose}
      slotProps={{ paper: { sx: paperSx, onScroll: handleScroll } }}
      anchorOrigin={{ vertical: 'bottom', horizontal: 'left' }}
    >
      <Box
        sx={searchBoxSx}
        onKeyDown={(event) => event.stopPropagation()}
      >
        <SimpleSearchBar
          value={search}
          onChange={onSearchChange}
          {...(searchDebounceMs !== undefined ? { debounceMs: searchDebounceMs } : {})}
          placeholder={searchPlaceholder}
        />
      </Box>

      {onCreateNew && (
        <MenuItem
          onClick={onCreateNew}
          divider
        >
          <ListItemText primary={createNewLabel ?? t('agents.toolMenu.createNew', 'Create new')} />
        </MenuItem>
      )}

      {!isLoading && items.length === 0 && (
        <MenuItem
          disabled
          sx={statusItemSx}
        >
          <ListItemText primary={emptyMessage} />
        </MenuItem>
      )}

      {items.map((item) => (
        <MenuItem
          key={item.key}
          onClick={item.onClick}
        >
          {item.icon && <ListItemIcon>{item.icon}</ListItemIcon>}
          <ListItemText
            primary={item.label}
            secondary={item.description}
          />
        </MenuItem>
      ))}

      {/* After the rows, like EliteaUI's picker: while the next page (or a new search) loads, this sits below what is already listed. */}
      {isLoading && (
        <MenuItem
          disabled
          sx={statusItemSx}
        >
          <ListItemText primary={t('agents.toolMenu.loading', 'Loading…')} />
        </MenuItem>
      )}
    </Menu>
  );
}
