import { useEffect, useRef, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useSearch } from '@tanstack/react-router';

import { t } from '@/shared/i18n';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';

/**
 * The one search box of every list page and type gallery, in the page's
 * toolbar (`PageToolbar`'s `search`), web and desktop alike.
 *
 * WHY THE URL. The box and the rows have no common owner below the route:
 * `pages/agents/Applications.tsx` renders the toolbar, and the selected tab —
 * `PrivateAgentsList`, `Latest` — renders the rows. The route's `query` param
 * is that owner, and it also makes a filtered list linkable and reload-proof.
 * `query` is a shell-wide param (`routes/-search/common.ts`), so every page
 * under the shell reads the same name.
 *
 * The write is `replace`, so typing a query does not fill the browser's back
 * stack with one entry per debounced keystroke.
 *
 * `/` (outside a text field) focuses the box. ⌘F / Ctrl+F is left to the
 * browser's own find-in-page: taking it over would hide a tool people use. Below a 720px wide toolbar the box folds to its icon and opens
 * again while it has focus (`PageToolbar` is the size container).
 */
export interface PageSearchProps {
  readonly placeholder?: string;
  readonly 'data-testid'?: string;
}

/** The current search text for the page under the route. `''` when nothing is searched. */
export function useListSearchQuery(): string {
  const search = useSearch({ strict: false }) as { readonly query?: unknown };
  return typeof search.query === 'string' ? search.query : '';
}

function isTextField(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  return target.isContentEditable || target.tagName === 'INPUT' || target.tagName === 'TEXTAREA' || target.tagName === 'SELECT';
}

/** `/`, outside a text field: the shortcut that focuses the page's search. */
export function isSearchShortcut(event: Pick<KeyboardEvent, 'key' | 'metaKey' | 'ctrlKey' | 'altKey' | 'target'>): boolean {
  if (event.altKey || event.metaKey || event.ctrlKey) return false;
  return event.key === '/' && !isTextField(event.target);
}

export function PageSearch({ placeholder, 'data-testid': dataTestId }: PageSearchProps): ReactNode {
  const navigate = useNavigate();
  const value = useListSearchQuery();
  const boxRef = useRef<HTMLDivElement>(null);
  const label = placeholder ?? t('widgets.pageHeader.search.placeholder', 'Search');

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent): void => {
      if (event.defaultPrevented || !isSearchShortcut(event)) return;
      const input = boxRef.current?.querySelector('input');
      if (input === null || input === undefined) return;
      event.preventDefault();
      input.focus();
      input.select();
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, []);

  return (
    <Box
      ref={boxRef}
      component="search"
      data-testid="page-search"
      sx={fieldSx}
    >
      <SimpleSearchBar
        value={value}
        onChange={(next) => {
          void navigate({ to: '.', replace: true, search: (prev: Record<string, unknown>) => ({ ...prev, query: next }) });
        }}
        placeholder={label}
        aria-label={label}
        {...(dataTestId === undefined ? {} : { 'data-testid': dataTestId })}
      />
    </Box>
  );
}

/** 16rem, folded to its icon in a narrow toolbar until it has focus or a query. */
const fieldSx: SxProps<Theme> = {
  width: '16rem',
  flexShrink: 0,
  transition: 'width 0.15s ease',
  '@container page-toolbar (max-width: 45rem)': {
    width: '2.5rem',
    '&:focus-within, &:has(input:not(:placeholder-shown))': { width: '16rem' },
  },
};
