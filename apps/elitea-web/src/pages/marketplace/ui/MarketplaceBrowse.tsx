/**
 * The Marketplace's right-sidebar **Browse** tab (ADR-0051 decisions 1 and 8):
 * search, kind, the facets the server counts (category, tag, publisher, tier,
 * required toolkit types, rating, hired here) and sort — every control the
 * listing grid answers to, in the shell's side panel (the desktop inspector or
 * the web side panel) and nowhere else on the page.
 *
 * It registers under the panel's `filters` tab id, the one list pages use: the
 * id names the slot a page fills (and keeps the panel's remembered layout), and
 * "Browse" is what this page calls it.
 */
import type { ChangeEvent, ReactNode } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormControl from '@mui/material/FormControl';
import FormControlLabel from '@mui/material/FormControlLabel';
import FormGroup from '@mui/material/FormGroup';
import FormLabel from '@mui/material/FormLabel';
import MenuItem from '@mui/material/MenuItem';
import RadioGroup from '@mui/material/RadioGroup';
import TextField from '@mui/material/TextField';
import type { SxProps, Theme } from '@mui/material/styles';

import type { MarketplaceFacetCount, MarketplaceFacets } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseCheckbox, CHECKBOX_MODES } from '@/shared/ui/BaseCheckbox';
import { filtersPanelSx } from '@/shared/ui/EntityRail';
import { SidePanelTab } from '@/shared/ui/side-panel';

import { SORT_OPTIONS, TIERS } from '../constants';
import { sortFromSearch, type MarketplaceFilters } from '../filters';
import { kindFromSearch, kindLabel } from '../listing';
import type { MarketplaceKind } from '../types';

export interface MarketplaceBrowseProps {
  readonly filters: MarketplaceFilters;
  /** What the search box shows; the URL's query trails it by the debounce. */
  readonly typedQuery: string;
  readonly onTypedQueryChange: (query: string) => void;
  readonly facets: MarketplaceFacets | undefined;
  readonly onChange: (patch: Partial<MarketplaceFilters>) => void;
  readonly onKindChange: (kind: MarketplaceKind) => void;
}

const NO_FACETS: readonly MarketplaceFacetCount[] = [];

interface FacetGroupProps {
  readonly legend: string;
  readonly counts: readonly MarketplaceFacetCount[];
  readonly picked: readonly string[];
  readonly onToggle: (value: string) => void;
  readonly onClear: () => void;
  readonly testId: string;
}

/** A multi-select facet: a fieldset of checkboxes, each with the server's count. */
function FacetGroup({ legend, counts, picked, onToggle, onClear, testId }: FacetGroupProps): ReactNode {
  if (counts.length === 0 && picked.length === 0) return null;
  const shown = counts.length > 0 ? counts : picked.map((value) => ({ value, label: value, count: 0 }));
  return (
    <FormControl component="fieldset" data-testid={testId}>
      <Box sx={legendRowSx}>
        <FormLabel component="legend">{legend}</FormLabel>
        {picked.length > 0 && (
          <Button
            size="small"
            onClick={onClear}
            aria-label={t('marketplace.browse.clearFacet', 'Clear {{facet}}', { facet: legend })}
          >
            {t('marketplace.browse.clear', 'Clear')}
          </Button>
        )}
      </Box>
      <FormGroup>
        {shown.map((entry) => (
          <FormControlLabel
            key={entry.value}
            sx={optionSx}
            control={
              <BaseCheckbox
                size="small"
                sx={controlSx}
                checked={picked.includes(entry.value)}
                onChange={() => onToggle(entry.value)}
              />
            }
            label={`${entry.label} (${entry.count})`}
          />
        ))}
      </FormGroup>
    </FormControl>
  );
}

const toggled = (list: readonly string[], value: string): string[] =>
  list.includes(value) ? list.filter((item) => item !== value) : [...list, value];

interface SelectFieldProps {
  readonly label: string;
  readonly value: string;
  readonly onChange: (value: string) => void;
  readonly children: ReactNode;
}

/** A labelled select in the side panel; the label is also the accessible name. */
function SelectField({ label, value, onChange, children }: SelectFieldProps): ReactNode {
  return (
    <TextField
      select
      size="small"
      value={value}
      onChange={(event: ChangeEvent<HTMLInputElement>) => onChange(event.target.value)}
      label={label}
      slotProps={{ htmlInput: { 'aria-label': label } }}
    >
      {children}
    </TextField>
  );
}

interface PanelPartProps {
  readonly filters: MarketplaceFilters;
  readonly facets: MarketplaceFacets | undefined;
  readonly onChange: (patch: Partial<MarketplaceFilters>) => void;
}

/** The facets the server counts, as checkbox groups. */
function BrowseFacets({ filters, facets, onChange }: PanelPartProps): ReactNode {
  return (
    <>
      <FacetGroup
        legend={t('marketplace.browse.category', 'Category')}
        counts={facets?.categories ?? NO_FACETS}
        picked={filters.categories}
        onToggle={(value) => onChange({ categories: toggled(filters.categories, value) })}
        onClear={() => onChange({ categories: [] })}
        testId="marketplace-facet-category"
      />
      <FacetGroup
        legend={t('marketplace.browse.tags', 'Tags')}
        counts={facets?.tags ?? NO_FACETS}
        picked={filters.tags}
        onToggle={(value) => onChange({ tags: toggled(filters.tags, value) })}
        onClear={() => onChange({ tags: [] })}
        testId="marketplace-facet-tag"
      />
      <FacetGroup
        legend={t('marketplace.browse.requires', 'Requires')}
        counts={facets?.required_types ?? NO_FACETS}
        picked={filters.requires}
        onToggle={(value) => onChange({ requires: toggled(filters.requires, value) })}
        onClear={() => onChange({ requires: [] })}
        testId="marketplace-facet-requires"
      />
    </>
  );
}

/** Publisher, highest risk tier, minimum rating and sort. */
function BrowseSelects({ filters, facets, onChange }: PanelPartProps): ReactNode {
  const publishers = facets?.publishers ?? NO_FACETS;
  return (
    <>
      {(publishers.length > 0 || filters.publisherId !== '') && (
        <SelectField
          label={t('marketplace.browse.publisher', 'Publisher')}
          value={filters.publisherId}
          onChange={(publisherId) => onChange({ publisherId })}
        >
          <MenuItem value="">{t('marketplace.browse.anyPublisher', 'Any publisher')}</MenuItem>
          {publishers.map((publisher) => (
            <MenuItem key={publisher.value} value={publisher.value}>
              {`${publisher.label} (${publisher.count})`}
            </MenuItem>
          ))}
        </SelectField>
      )}
      <SelectField
        label={t('marketplace.browse.tier', 'Highest risk tier')}
        value={filters.maxTier}
        onChange={(maxTier) => onChange({ maxTier })}
      >
        <MenuItem value="">{t('marketplace.browse.anyTier', 'Any tier')}</MenuItem>
        {TIERS.map((tier) => (
          <MenuItem key={tier} value={tier}>
            {tier}
          </MenuItem>
        ))}
      </SelectField>
      <SelectField
        label={t('marketplace.browse.rating', 'Rating')}
        value={String(filters.minRating)}
        onChange={(rating) => onChange({ minRating: Number(rating) })}
      >
        <MenuItem value="0">{t('marketplace.browse.anyRating', 'Any rating')}</MenuItem>
        <MenuItem value="3">{t('marketplace.browse.rating3', '3 stars and up')}</MenuItem>
        <MenuItem value="4">{t('marketplace.browse.rating4', '4 stars and up')}</MenuItem>
      </SelectField>
      <SelectField
        label={t('marketplace.browse.sort', 'Sort by')}
        value={filters.sort}
        onChange={(key) => onChange({ sort: sortFromSearch(key) })}
      >
        {SORT_OPTIONS.map((option) => (
          <MenuItem key={option.key} value={option.key}>
            {t(option.labelKey, option.labelFallback)}
          </MenuItem>
        ))}
      </SelectField>
    </>
  );
}

function searchPlaceholder(kind: MarketplaceKind): string {
  if (kind === 'skills') return t('marketplace.browse.searchSkills', 'Search for skills');
  if (kind === 'teams') return t('marketplace.browse.searchTeams', 'Search for teams');
  return t('marketplace.browse.searchAgents', 'Search for agents');
}

const KINDS: readonly MarketplaceKind[] = ['agents', 'skills', 'teams'];

/** The Kind radios: agents, skills or teams. */
function KindChoice({
  kind,
  onKindChange,
}: {
  readonly kind: MarketplaceKind;
  readonly onKindChange: (kind: MarketplaceKind) => void;
}): ReactNode {
  return (
    <FormControl component="fieldset">
      <FormLabel component="legend">{t('marketplace.browse.kind', 'Kind')}</FormLabel>
      <RadioGroup
        value={kind}
        onChange={(event: ChangeEvent<HTMLInputElement>) => onKindChange(kindFromSearch(event.target.value))}
      >
        {KINDS.map((value) => (
          <FormControlLabel
            key={value}
            value={value}
            sx={optionSx}
            control={<BaseCheckbox mode={CHECKBOX_MODES.radio} size="small" sx={controlSx} />}
            label={kindLabel(value)}
            data-testid={`marketplace-kind-${value}`}
          />
        ))}
      </RadioGroup>
    </FormControl>
  );
}

export function MarketplaceBrowse({
  filters,
  typedQuery,
  onTypedQueryChange,
  facets,
  onChange,
  onKindChange,
}: MarketplaceBrowseProps): ReactNode {
  const { kind } = filters;
  return (
    <SidePanelTab
      id="filters"
      title={t('marketplace.browse.title', 'Browse')}
      subject={t('marketplace.title', 'Marketplace')}
    >
      <Box data-testid="marketplace-browse" sx={filtersPanelSx}>
        <TextField
          type="search"
          size="small"
          value={typedQuery}
          onChange={(event: ChangeEvent<HTMLInputElement>) => onTypedQueryChange(event.target.value)}
          label={t('marketplace.browse.search', 'Search')}
          placeholder={searchPlaceholder(kind)}
          slotProps={{ htmlInput: { 'data-testid': 'marketplace-search' } }}
        />
        <KindChoice kind={kind} onKindChange={onKindChange} />
        <FormControlLabel
          sx={optionSx}
          control={
            <BaseCheckbox
              size="small"
              sx={controlSx}
              checked={filters.hiredHere}
              onChange={(event) => onChange({ hiredHere: event.target.checked })}
            />
          }
          label={t('marketplace.browse.hiredHere', 'Hired in this project ({{count}})', {
            count: facets?.hired_here ?? 0,
          })}
          data-testid="marketplace-hired-here"
        />
        <BrowseFacets filters={filters} facets={facets} onChange={onChange} />
        <BrowseSelects filters={filters} facets={facets} onChange={onChange} />
      </Box>
    </SidePanelTab>
  );
}

/** One row for every option, Kind radios and checkboxes alike: the same control skin and size, indent and row height. */
const optionSx: SxProps<Theme> = { marginLeft: 0, marginRight: 0, minHeight: '2rem' };

/** The same padding and glyph size on the radio and the checkbox, so the two columns of options line up. */
const controlSx: SxProps<Theme> = { padding: '0.375rem', '& svg': { width: '1rem', height: '1rem' } };

const legendRowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', justifyContent: 'space-between' };
