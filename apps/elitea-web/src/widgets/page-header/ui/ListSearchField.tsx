import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useSearch } from '@tanstack/react-router';

import { t } from '@/shared/i18n';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';

/**
 * The list pages' search box, on the header row.
 *
 * Ported by USE CASE from `apps/elitea-ui/src/components/RightPanel.jsx:
 * 67-86`, which mounts `SearchBar` with `data-testid="agent-search-input"` /
 * `"pipeline-search-input"`. The baseline puts that box in the right rail and
 * keeps its value in Redux; this app puts it in the shared header
 * (`PageHeader`'s `search` slot) and keeps the value in the route's own
 * `query` search param.
 *
 * WHY THE URL. The box and the rows have no common owner below the route:
 * `pages/agents/Applications.tsx` renders the header, and the selected tab —
 * `PrivateAgentsList`, `Latest` — renders the rows. A store would work; the
 * URL works and also makes a filtered list linkable and reload-proof, which
 * the baseline's Redux copy is not.
 *
 * The write is `replace`, so typing a query does not fill the browser's back
 * stack with one entry per debounced keystroke.
 */
export interface ListSearchFieldProps {
  readonly placeholder?: string;
  readonly 'data-testid'?: string;
}

/** The current search text for the page under the route. `''` when nothing is searched. */
export function useListSearchQuery(): string {
  const search = useSearch({ strict: false }) as { readonly query?: unknown };
  return typeof search.query === 'string' ? search.query : '';
}

export function ListSearchField({ placeholder, 'data-testid': dataTestId }: ListSearchFieldProps): ReactNode {
  const navigate = useNavigate();
  const value = useListSearchQuery();

  return (
    <Box sx={fieldSx}>
      <SimpleSearchBar
        value={value}
        onChange={(next) => {
          void navigate({ to: '.', replace: true, search: (prev: Record<string, unknown>) => ({ ...prev, query: next }) });
        }}
        placeholder={placeholder ?? t('widgets.pageHeader.search.placeholder', 'Search')}
        {...(dataTestId === undefined ? {} : { 'data-testid': dataTestId })}
      />
    </Box>
  );
}

/** `SearchBar`'s own width in the baseline's rail (`RightPanel.jsx`'s 25% grid column at a 1440px viewport). */
const fieldSx: SxProps<Theme> = { width: '16rem', flexShrink: 0 };
