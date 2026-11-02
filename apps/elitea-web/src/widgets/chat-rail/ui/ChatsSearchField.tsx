/**
 * The Chats list's title filter, at the top of the list while the caption's
 * search is open. It filters as the person types (the rail sends the query a
 * moment after the last keystroke); Escape closes it and shows every chat
 * again.
 */
import type { KeyboardEvent } from 'react';

import SearchIcon from '@mui/icons-material/Search';
import Box from '@mui/material/Box';
import InputBase from '@mui/material/InputBase';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import type { ChatRailFilter } from '../model/chatRail';

export function ChatsSearchField({ filter }: { readonly filter: ChatRailFilter }): React.JSX.Element {
  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>): void => {
    if (event.key !== 'Escape') return;
    filter.setQuery('');
    filter.setSearchOpen(false);
  };
  return (
    <Box sx={(theme: Theme) => ({ display: 'flex', alignItems: 'center', gap: 1, paddingLeft: 1, paddingRight: 0.5, minHeight: '1.75rem', color: theme.vars.palette.text.metrics })}>
      <SearchIcon aria-hidden sx={{ width: '1rem', height: '1rem' }} />
      <InputBase
        value={filter.query}
        placeholder={t('desktop.chats.searchPlaceholder', 'Search chats')}
        onChange={(event) => filter.setQuery(event.target.value)}
        onKeyDown={onKeyDown}
        inputRef={(node: HTMLInputElement | null) => node?.focus()}
        inputProps={{ 'aria-label': t('desktop.chats.searchLabel', 'Search chats'), 'data-testid': 'shell-chats-search-input', type: 'search' }}
        sx={(theme: Theme) => ({
          flex: 1,
          paddingX: 0.5,
          borderRadius: theme.vars.shape.radiusSm,
          border: `1px solid ${theme.vars.palette.divider}`,
          background: theme.vars.palette.background.default,
          typography: 'labelSmall',
        })}
      />
    </Box>
  );
}
