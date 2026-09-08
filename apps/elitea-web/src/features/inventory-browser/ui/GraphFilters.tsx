/**
 * The four controls that narrow the entity list: a search box and three
 * facets.
 *
 * THE SEARCH BOX SUBMITS, it does not filter as you type. Every keystroke here
 * would be an INVOCATION on the provider — an engine call, not a cached GET —
 * so a typed word is eight runs of the graph matcher. The legacy screen
 * submitted too, for the same reason.
 */
import { useState } from 'react';

import Box from '@mui/material/Box';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import type { GraphFilter } from '../model/useGraphBrowser';

const rowSx = { display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 1 } as const;
const facetSx = { minWidth: '10rem' } as const;

export interface GraphFiltersProps {
  readonly filter: GraphFilter;
  readonly types: readonly string[];
  readonly layers: readonly string[];
  readonly sources: readonly string[];
  readonly onChange: (filter: GraphFilter) => void;
}

export function GraphFilters({
  filter,
  types,
  layers,
  sources,
  onChange,
}: GraphFiltersProps): React.JSX.Element {
  // The typed text is LOCAL until it is submitted. Holding it in the shared
  // filter would make every keystroke a new query key and a new invocation.
  const [draft, setDraft] = useState(filter.query);

  return (
    <Box
      component="form"
      sx={rowSx}
      data-testid="inventory-graph-filters"
      onSubmit={(event) => {
        event.preventDefault();
        onChange({ ...filter, query: draft.trim() });
      }}
    >
      <TextField
        size="small"
        value={draft}
        // On the INPUT — see InventoryAskPanel for why a test id on TextField
        // itself names the root <div> and not something a caller can type in.
        slotProps={{ htmlInput: { 'data-testid': 'inventory-search-input' } }}
        label={t('inventory.graph.search', 'Search entities')}
        onChange={(event) => {
          setDraft(event.target.value);
        }}
      />
      <BaseBtn variant="secondary" size="small" type="submit" data-testid="inventory-search-submit">
        {t('inventory.graph.searchAction', 'Search')}
      </BaseBtn>

      <Facet
        testId="inventory-filter-type"
        label={t('inventory.graph.type', 'Type')}
        anyLabel={t('inventory.graph.anyType', 'Any type')}
        value={filter.entityType}
        options={types}
        onChange={(value) => {
          onChange({ ...filter, entityType: value });
        }}
      />
      <Facet
        testId="inventory-filter-layer"
        label={t('inventory.graph.layer', 'Layer')}
        anyLabel={t('inventory.graph.anyLayer', 'Any layer')}
        value={filter.layer}
        options={layers}
        onChange={(value) => {
          onChange({ ...filter, layer: value });
        }}
      />
      <Facet
        testId="inventory-filter-source"
        label={t('inventory.graph.source', 'Source')}
        anyLabel={t('inventory.graph.anySource', 'Any source')}
        value={filter.sourceToolkit}
        options={sources}
        onChange={(value) => {
          onChange({ ...filter, sourceToolkit: value });
        }}
      />
    </Box>
  );
}

interface FacetProps {
  readonly testId: string;
  readonly label: string;
  readonly anyLabel: string;
  readonly value: string;
  readonly options: readonly string[];
  readonly onChange: (value: string) => void;
}

/**
 * One facet.
 *
 * It renders even with no options, and that is deliberate: an empty select
 * says "this graph holds no layers", while a control that disappears says
 * nothing at all and leaves the user wondering where the filter went.
 */
function Facet({ testId, label, anyLabel, value, options, onChange }: FacetProps): React.JSX.Element {
  return (
    <TextField
      select
      size="small"
      sx={facetSx}
      label={label}
      value={value}
      slotProps={{ htmlInput: { 'data-testid': testId } }}
      onChange={(event) => {
        onChange(event.target.value);
      }}
    >
      <MenuItem value="">{anyLabel}</MenuItem>
      {options.map((option) => (
        <MenuItem key={option} value={option}>
          {option}
        </MenuItem>
      ))}
    </TextField>
  );
}
