import type { ReactNode } from 'react';

import AddOutlinedIcon from '@mui/icons-material/AddOutlined';
import CancelIcon from '@mui/icons-material/Cancel';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import { facetValue, useListSearch, type EntityListSidebarProps, type ListFilterChip, type SearchRecord } from './listSidebarModel';
import { useListSearchQuery } from './PageSearch';

type SidebarProps = EntityListSidebarProps;

/** The chips the bar shows: the search text, each facet that is on, then the page's own. */
function summaryChips(props: SidebarProps, query: string, search: SearchRecord, patch: (next: SearchRecord) => void): ListFilterChip[] {
  const chips: ListFilterChip[] = [];
  if (query.trim() !== '') {
    chips.push({ id: 'query', label: t('widgets.listSidebar.summary.search', 'Search: {{query}}', { query }), onRemove: () => patch({ query: '' }) });
  }
  for (const facet of props.facets ?? []) {
    const value = facetValue(search, facet);
    if (value === undefined) continue;
    const label = facet.options.find((option) => option.value === value)?.label ?? value;
    chips.push({ id: facet.param, label: `${facet.chipLabel}: ${label}`, onRemove: () => patch({ [facet.param]: facet.array === true ? [] : '' }) });
  }
  return [...chips, ...(props.extraChips ?? [])];
}

/** Clear: the search, every facet and the page's own params go in one write; what lives outside the URL is cleared after it. */
function clearAll(props: SidebarProps, patch: (next: SearchRecord) => void): void {
  const next: SearchRecord = { query: '', ...props.clearParams };
  for (const facet of props.facets ?? []) next[facet.param] = facet.array === true ? [] : '';
  patch(next);
  props.onClearExtra?.();
}

/**
 * The one-line bar above the list while the sidebar is closed or a drawer: the
 * search text and every active filter as a removable chip, Clear, and New.
 */
export function ListSummaryBar(props: SidebarProps & { readonly onOpenSidebar: () => void }): ReactNode {
  const { testIdPrefix, newAction, listControls = true, onOpenSidebar } = props;
  const query = useListSearchQuery();
  const { search, patch } = useListSearch();
  const chips = summaryChips(props, query, search, patch);
  if (!listControls && newAction === undefined) return null;

  return (
    <Box
      component="section"
      aria-label={t('widgets.listSidebar.summary.label', 'Active filters')}
      data-testid={`${testIdPrefix}-summary-bar`}
      sx={summarySx}
    >
      <Box sx={summaryChipsSx}>
        {listControls && chips.length === 0 && (
          <BaseBtn
            variant="text"
            onClick={onOpenSidebar}
            data-testid={`${testIdPrefix}-summary-search`}
          >
            {t('widgets.listSidebar.summary.searchAndFilter', 'Search and filter')}
          </BaseBtn>
        )}
        {chips.map((chip) => (
          <Chip
            key={chip.id}
            size="small"
            label={chip.label}
            onDelete={chip.onRemove}
            data-testid={`${testIdPrefix}-summary-chip-${chip.id}`}
            deleteIcon={<CancelIcon titleAccess={t('widgets.listSidebar.summary.remove', 'Remove filter {{filter}}', { filter: chip.label })} />}
          />
        ))}
        {chips.length > 0 && (
          <BaseBtn
            variant="text"
            onClick={() => clearAll(props, patch)}
            data-testid={`${testIdPrefix}-summary-clear`}
          >
            {t('widgets.listSidebar.summary.clear', 'Clear')}
          </BaseBtn>
        )}
      </Box>
      {newAction !== undefined && (
        <BaseBtn
          variant="contained"
          startIcon={<AddOutlinedIcon />}
          onClick={newAction.onClick}
          disabled={newAction.disabled ?? false}
          data-testid={`${newAction.testId ?? `${testIdPrefix}-new`}-summary`}
        >
          {newAction.label}
        </BaseBtn>
      )}
    </Box>
  );
}

const summarySx: SxProps<Theme> = (theme: Theme) => ({
  flexShrink: 0,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  gap: theme.spacing(1.5),
  paddingInline: '1.5rem',
  paddingBlock: theme.spacing(1),
  borderBottom: 1,
  borderColor: 'divider',
});

const summaryChipsSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: 0.75, minWidth: 0, flex: 1 };
