/**
 * The Marketplace's right-sidebar **Browse** tab (ADR-0051 decisions 1 and 8):
 * search, kind, categories and sort — every control the listing grid answers
 * to, in the shell's side panel (the desktop inspector or the web side panel)
 * and nowhere else on the page.
 *
 * It registers under the panel's `filters` tab id, the one list pages use: the
 * id names the slot a page fills (and keeps the panel's remembered layout), and
 * "Browse" is what this page calls it. Categories are multi-select for agents
 * and skills alike.
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

import { t } from '@/shared/i18n';
import { BaseCheckbox, CHECKBOX_MODES } from '@/shared/ui/BaseCheckbox';
import { filtersPanelSx } from '@/shared/ui/EntityRail';
import { SidePanelTab } from '@/shared/ui/side-panel';

import { SORT_OPTIONS } from '../constants';
import type { MarketplaceKind } from '../types';

export interface MarketplaceBrowseProps {
  readonly kind: MarketplaceKind;
  readonly onKindChange: (kind: MarketplaceKind) => void;
  readonly query: string;
  readonly onQueryChange: (query: string) => void;
  readonly categories: readonly string[];
  readonly selectedCategories: readonly string[];
  readonly onToggleCategory: (category: string) => void;
  readonly onClearCategories: () => void;
  readonly sortKey: string;
  readonly onSortChange: (key: string) => void;
}

export function MarketplaceBrowse({
  kind,
  onKindChange,
  query,
  onQueryChange,
  categories,
  selectedCategories,
  onToggleCategory,
  onClearCategories,
  sortKey,
  onSortChange,
}: MarketplaceBrowseProps): ReactNode {
  const sortLabel = t('marketplace.browse.sort', 'Sort by');
  const sortOptions = SORT_OPTIONS.filter((option) => kind === 'agents' || !option.agentsOnly);
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
          value={query}
          onChange={(event: ChangeEvent<HTMLInputElement>) => onQueryChange(event.target.value)}
          label={t('marketplace.browse.search', 'Search')}
          placeholder={kind === 'agents' ? t('marketplace.browse.searchAgents', 'Search for agents') : t('marketplace.browse.searchSkills', 'Search for skills')}
          slotProps={{ htmlInput: { 'data-testid': 'marketplace-search' } }}
        />
        <FormControl component="fieldset">
          <FormLabel component="legend">{t('marketplace.browse.kind', 'Kind')}</FormLabel>
          <RadioGroup
            value={kind}
            onChange={(event: ChangeEvent<HTMLInputElement>) => onKindChange(event.target.value === 'skills' ? 'skills' : 'agents')}
          >
            <FormControlLabel value="agents" sx={optionSx} control={<BaseCheckbox mode={CHECKBOX_MODES.radio} size="small" sx={controlSx} />} label={t('marketplace.kind.agents', 'Agents')} data-testid="marketplace-kind-agents" />
            <FormControlLabel value="skills" sx={optionSx} control={<BaseCheckbox mode={CHECKBOX_MODES.radio} size="small" sx={controlSx} />} label={t('marketplace.kind.skills', 'Skills')} data-testid="marketplace-kind-skills" />
          </RadioGroup>
        </FormControl>
        {categories.length > 0 && (
          <FormControl component="fieldset">
            <Box sx={legendRowSx}>
              <FormLabel component="legend">{t('marketplace.browse.category', 'Category')}</FormLabel>
              {selectedCategories.length > 0 && (
                <Button size="small" onClick={onClearCategories} data-testid="marketplace-clear-categories">
                  {t('marketplace.browse.clear', 'Clear')}
                </Button>
              )}
            </Box>
            <FormGroup>
              {categories.map((category) => (
                <FormControlLabel
                  key={category}
                  sx={optionSx}
                  control={<BaseCheckbox size="small" sx={controlSx} checked={selectedCategories.includes(category)} onChange={() => onToggleCategory(category)} />}
                  label={category}
                />
              ))}
            </FormGroup>
          </FormControl>
        )}
        <TextField
          select
          size="small"
          value={sortKey}
          onChange={(event: ChangeEvent<HTMLInputElement>) => onSortChange(event.target.value)}
          label={sortLabel}
          slotProps={{ htmlInput: { 'aria-label': sortLabel } }}
        >
          {sortOptions.map((option) => (
            <MenuItem key={option.key} value={option.key}>
              {t(option.labelKey, option.labelFallback)}
            </MenuItem>
          ))}
        </TextField>
      </Box>
    </SidePanelTab>
  );
}

/** One row for every option, Kind radios and Category checkboxes alike: the same control skin and size, indent and row height. */
const optionSx: SxProps<Theme> = { marginLeft: 0, marginRight: 0, minHeight: '2rem' };

/** The same padding and glyph size on the radio and the checkbox, so the two columns of options line up. */
const controlSx: SxProps<Theme> = { padding: '0.375rem', '& svg': { width: '1rem', height: '1rem' } };

const legendRowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', justifyContent: 'space-between' };
