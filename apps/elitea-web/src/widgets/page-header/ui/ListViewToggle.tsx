import { useEffect, type ReactNode } from 'react';

import GridViewOutlinedIcon from '@mui/icons-material/GridViewOutlined';
import TableRowsOutlinedIcon from '@mui/icons-material/TableRowsOutlined';
import ToggleButton from '@mui/material/ToggleButton';
import ToggleButtonGroup from '@mui/material/ToggleButtonGroup';
import Tooltip from '@mui/material/Tooltip';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useSearch } from '@tanstack/react-router';

import { t } from '@/shared/i18n';
import { ENTITY_LIST_VIEW, type EntityListView } from '@/shared/ui/EntityCardList';

import { readPersistedListView, writePersistedListView } from '../lib/listViewPersistence';

/**
 * The table/card switch on a list page's header row.
 *
 * Ported from `apps/elitea-ui/src/components/ViewToggle.jsx`, which
 * `pages/Applications/Applications.jsx:123`, `pages/Pipelines/Pipelines.jsx:
 * 273`, `[fsd]/pages/skills/Skills.jsx:70` and `pages/Toolkits/Toolkits.jsx:32`
 * all mount in their header's middle slot. `pages/agents/Applications.tsx` and
 * `pages/pipelines/Pipelines.tsx` each disclosed it as unported; this is the
 * port.
 *
 * The VALUE lives in the URL, exactly as the baseline puts it (`?view=cards` /
 * `?view=table`), because the renderer that obeys it —
 * `shared/ui/EntityCardList` — already reads that key and nothing else. The
 * toggle therefore adds no second source of truth.
 *
 * The CHOICE is additionally remembered per page in `localStorage`
 * (`../lib/listViewPersistence.ts`), which the baseline does not do: its
 * toggle resets to cards on every fresh visit. A reader who works in the table
 * gets the table back.
 *
 * Restoring is a `replace` navigation, not a render-time read of the store: a
 * component that told the user "table" while `EntityCardList` still drew cards
 * would be two views of one value disagreeing for a frame. Writing the URL
 * first makes both read the same key.
 */
export interface ListViewToggleProps {
  /** The `localStorage` bucket this page remembers its choice in — `agents`, `pipelines`. */
  readonly pageKey: string;
  /** Each button gets `data-testid={`${testIdPrefix}-table-view-button`}` / `-card-view-button`. */
  readonly testIdPrefix: string;
}

/** `?view=` narrowed to the two values THIS toggle owns (`pages/apps` writes `grid`/`list` on the same key). */
function urlView(value: unknown): EntityListView | undefined {
  if (value === ENTITY_LIST_VIEW.table) return ENTITY_LIST_VIEW.table;
  if (value === ENTITY_LIST_VIEW.cards) return ENTITY_LIST_VIEW.cards;
  return undefined;
}

export function ListViewToggle({ pageKey, testIdPrefix }: ListViewToggleProps): ReactNode {
  const navigate = useNavigate();
  const search = useSearch({ strict: false }) as { readonly view?: unknown };
  const current = urlView(search.view);

  useEffect(() => {
    if (current !== undefined) return;
    const remembered = readPersistedListView(pageKey);
    if (remembered === undefined) return;
    void navigate({ to: '.', replace: true, search: (prev: Record<string, unknown>) => ({ ...prev, view: remembered }) });
  }, [current, pageKey, navigate]);

  const view = current ?? ENTITY_LIST_VIEW.cards;

  const select = (next: EntityListView): void => {
    if (next === view) return;
    writePersistedListView(pageKey, next);
    void navigate({ to: '.', search: (prev: Record<string, unknown>) => ({ ...prev, view: next }) });
  };

  return (
    <ToggleButtonGroup
      size="small"
      exclusive
      value={view}
      aria-label={t('widgets.pageHeader.viewToggle.label', 'List view')}
      sx={groupSx}
    >
      <Tooltip title={t('widgets.pageHeader.viewToggle.table', 'Table view')}>
        <ToggleButton
          value={ENTITY_LIST_VIEW.table}
          aria-label={t('widgets.pageHeader.viewToggle.table', 'Table view')}
          data-testid={`${testIdPrefix}-table-view-button`}
          onClick={() => select(ENTITY_LIST_VIEW.table)}
        >
          <TableRowsOutlinedIcon fontSize="small" />
        </ToggleButton>
      </Tooltip>
      <Tooltip title={t('widgets.pageHeader.viewToggle.cards', 'Card list view')}>
        <ToggleButton
          value={ENTITY_LIST_VIEW.cards}
          aria-label={t('widgets.pageHeader.viewToggle.cards', 'Card list view')}
          data-testid={`${testIdPrefix}-card-view-button`}
          onClick={() => select(ENTITY_LIST_VIEW.cards)}
        >
          <GridViewOutlinedIcon fontSize="small" />
        </ToggleButton>
      </Tooltip>
    </ToggleButtonGroup>
  );
}

const groupSx: SxProps<Theme> = { flexShrink: 0 };
