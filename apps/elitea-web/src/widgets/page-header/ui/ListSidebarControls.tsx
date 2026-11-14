import type { ReactNode } from 'react';

import ArrowDownwardIcon from '@mui/icons-material/ArrowDownward';
import ArrowUpwardIcon from '@mui/icons-material/ArrowUpward';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';
import ToggleButton from '@mui/material/ToggleButton';
import ToggleButtonGroup from '@mui/material/ToggleButtonGroup';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { facetValues, toggledFacet, type EntityListSidebarProps, type ListFacet, type SearchRecord } from './listSidebarModel';

/** A titled group of controls in the List tab. */
export function Section({ label, children }: { readonly label: string; readonly children: ReactNode }): ReactNode {
  return (
    <Box sx={sectionSx}>
      <Typography
        variant="labelSmall"
        component="h3"
        sx={sectionTitleSx}
      >
        {label}
      </Typography>
      {children}
    </Box>
  );
}

export function ModeSwitch({ modes, label }: { readonly modes: NonNullable<EntityListSidebarProps['modes']>; readonly label: string }): ReactNode {
  return (
    <ToggleButtonGroup
      size="small"
      exclusive
      fullWidth
      value={modes.active}
      aria-label={label}
      onChange={(_event, next: string | null) => {
        if (next !== null) modes.onSelect(next);
      }}
    >
      {modes.items.map((mode) => (
        <ToggleButton
          key={mode.value}
          value={mode.value}
          data-testid={mode.testId}
        >
          {mode.label}
        </ToggleButton>
      ))}
    </ToggleButtonGroup>
  );
}

interface FacetGroupProps {
  readonly facet: ListFacet;
  readonly search: SearchRecord;
  readonly patch: (next: SearchRecord) => void;
  readonly testIdPrefix: string;
}

/** One facet: a chip per value that occurs, with its count. A chip is a toggle: it says whether it is on in text, not only in colour. */
export function FacetGroup({ facet, search, patch, testIdPrefix }: FacetGroupProps): ReactNode {
  const current = facetValues(search, facet);
  const shown = facet.options.filter((option) => option.count > 0 || current.includes(option.value));
  // A value that is on stays offered even at zero rows, so the filter can always be switched off.
  const missing = current.filter((value) => !shown.some((option) => option.value === value)).map((value) => ({ value, label: value, count: 0 }));
  const options = [...shown, ...missing];
  if (options.length === 0) return null;
  return (
    <Section label={facet.label}>
      <Box
        component="fieldset"
        aria-label={facet.label}
        sx={chipsSx}
      >
        {options.map((option) => {
          const on = current.includes(option.value);
          return (
            <Chip
              key={option.value}
              clickable
              size="small"
              color={on ? 'primary' : 'default'}
              variant={on ? 'filled' : 'outlined'}
              aria-pressed={on}
              data-testid={`${testIdPrefix}-facet-${facet.param}-${option.value}`}
              label={`${option.label} (${option.count})`}
              onClick={() => patch({ [facet.param]: toggledFacet(facet, current, option.value) })}
            />
          );
        })}
      </Box>
    </Section>
  );
}

interface SortControlProps {
  readonly fields: NonNullable<EntityListSidebarProps['sortFields']>;
  readonly search: SearchRecord;
  readonly patch: (next: SearchRecord) => void;
  readonly testIdPrefix: string;
}

/** The sort field and its direction, in the route's `sort_by` and `sort_order`. */
export function SortControl({ fields, search, patch, testIdPrefix }: SortControlProps): ReactNode {
  const field = typeof search['sort_by'] === 'string' && fields.items.some((item) => item.value === search['sort_by']) ? search['sort_by'] : fields.defaultField;
  const ascending = search['sort_order'] === 'asc';
  const label = t('widgets.listSidebar.sort', 'Sort');
  const directionLabel = ascending ? t('widgets.listSidebar.sortAscending', 'Ascending. Switch to descending') : t('widgets.listSidebar.sortDescending', 'Descending. Switch to ascending');
  return (
    <Section label={label}>
      <Box sx={sortRowSx}>
        <Select
          size="small"
          value={field}
          onChange={(event) => patch({ sort_by: event.target.value, sort_order: event.target.value === 'name' ? 'asc' : 'desc' })}
          inputProps={{ 'aria-label': label, 'data-testid': `${testIdPrefix}-sort-field` }}
          sx={sortSelectSx}
        >
          {fields.items.map((item) => (
            <MenuItem
              key={item.value}
              value={item.value}
            >
              {item.label}
            </MenuItem>
          ))}
        </Select>
        <IconButton
          size="small"
          aria-label={directionLabel}
          data-testid={`${testIdPrefix}-sort-direction`}
          onClick={() => patch({ sort_order: ascending ? 'desc' : 'asc' })}
        >
          {ascending ? <ArrowUpwardIcon fontSize="small" /> : <ArrowDownwardIcon fontSize="small" />}
        </IconButton>
      </Box>
    </Section>
  );
}

const sectionSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', flexDirection: 'column', gap: theme.spacing(0.75) });

const sectionTitleSx: SxProps<Theme> = (theme: Theme) => ({ margin: 0, color: theme.vars.palette.text.metrics });

const chipsSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: 0.75, border: 0, margin: 0, padding: 0, minWidth: 0 };

const sortRowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: 1 };

const sortSelectSx: SxProps<Theme> = { flex: 1, minWidth: 0 };
