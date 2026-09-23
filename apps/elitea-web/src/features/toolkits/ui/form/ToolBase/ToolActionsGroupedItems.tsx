import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Checkbox from '@mui/material/Checkbox';
import Stack from '@mui/material/Stack';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { InfoTooltip } from '@/shared/ui/InfoTooltip';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';

import { ChipWithCheckIcon } from './ChipWithCheckIcon';
import type { ToolGroupSection } from './toolGroups';

/**
 * The GROUPED Tools picker: a search box, then one section per tool group,
 * each headed by a select-all control, the group's name, its consequence
 * badge, an ⓘ explanation and a selected/total count.
 *
 * It renders only the grouped half. The "Unavailable" chips (a previously
 * selected tool the toolkit no longer offers) stay with `ToolActionsItems`
 * ABOVE this, and the search deliberately does not touch them: an unavailable
 * tool is a problem to fix, and a filter that hid it would hide the problem
 * (ELITEA-2691).
 *
 * THE COUNTS ARE NOT THE VISIBLE COUNTS. `selectedCount`/`totalCount` are
 * computed over the whole group by `buildToolGroupSections`, so a search that
 * narrows Read to four chips still reads "10 / 26" — the number a person is
 * actually deciding with. A filtered count would silently answer a different
 * question than the one the header asks (ELITEA-2690).
 */
export interface ToolActionsGroupedItemsProps {
  readonly sections: readonly ToolGroupSection[];
  readonly selectedTools: readonly string[];
  readonly query: string;
  readonly onQueryChange: (value: string) => void;
  /** True when the toolkit has tools but none matches the query. */
  readonly noMatches: boolean;
  readonly onSelectTool: (value: string) => () => void;
  readonly onToggleGroup: (section: ToolGroupSection) => void;
  readonly disabled: boolean | undefined;
}

const searchSx: SxProps<Theme> = { marginTop: '0.5rem', maxWidth: '22rem' };
const sectionSx: SxProps<Theme> = { marginTop: '1rem' };
const headerSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.5rem', flexWrap: 'wrap' };
const chipsSx: SxProps<Theme> = { marginTop: '0.5rem', gap: '1rem', flexWrap: 'wrap' };
const badgeSx = (theme: Theme) => ({
  padding: '0.125rem 0.5rem',
  borderRadius: theme.vars.shape.radiusPill ?? 9999,
  backgroundColor: theme.vars.palette.background.secondary,
  color: theme.vars.palette.text.secondary,
});

function GroupHeader(props: { readonly section: ToolGroupSection; readonly onToggle: () => void; readonly disabled: boolean | undefined }): ReactNode {
  const { section, onToggle, disabled } = props;
  return (
    <Box sx={headerSx}>
      {/*
        * The select-all control is a real checkbox with a real accessible
        * name, not a click handler on the heading text: it is the only
        * control on this screen that can change 26 selections at once, and
        * a keyboard user has to be able to reach it. `indeterminate` is what
        * makes a partial group readable at a glance.
        */}
      <Checkbox
        size="small"
        checked={section.allSelected}
        indeterminate={section.selectedCount > 0 && !section.allSelected}
        onChange={onToggle}
        disabled={disabled}
        slotProps={{ input: { 'aria-label': section.label } }}
        data-testid={`tool-group-toggle-${section.id.replaceAll('_', '-')}`}
      />
      <Typography variant="bodyMedium">{section.label}</Typography>
      {section.badge !== '' && (
        <Typography
          variant="labelSmall"
          sx={badgeSx}
          data-testid={`tool-group-badge-${section.id.replaceAll('_', '-')}`}
        >
          {section.badge}
        </Typography>
      )}
      {section.hint !== '' && (
        <InfoTooltip
          title={section.hint}
          data-testid={`tool-group-hint-${section.id.replaceAll('_', '-')}`}
        />
      )}
      <Typography
        variant="labelSmall"
        color="text.secondary"
        data-testid={`tool-group-count-${section.id.replaceAll('_', '-')}`}
      >
        {`${section.selectedCount} / ${section.totalCount}`}
      </Typography>
    </Box>
  );
}

export function ToolActionsGroupedItems(props: ToolActionsGroupedItemsProps): ReactNode {
  const { sections, selectedTools, query, onQueryChange, noMatches, onSelectTool, onToggleGroup, disabled } = props;
  return (
    <Box data-testid="tool-groups">
      <SimpleSearchBar
        value={query}
        onChange={onQueryChange}
        placeholder={t('features.toolkits.toolGroups.searchPlaceholder', 'Search tools')}
        debounceMs={0}
        sx={searchSx}
        data-testid="tool-group-search"
      />
      {noMatches && (
        // Inline text, not an alert or a modal: an empty search result is a
        // normal state of a search box (ELITEA-2695 step 10).
        <Typography
          variant="bodySmall"
          color="text.secondary"
          sx={sectionSx}
          data-testid="tool-group-no-matches"
        >
          {t('features.toolkits.toolGroups.noMatches', 'No tools match "{{query}}"', { query })}
        </Typography>
      )}
      {sections.map((section) => (
        <Box
          key={section.id}
          sx={sectionSx}
          data-testid={`tool-group-${section.id.replaceAll('_', '-')}`}
        >
          <GroupHeader
            section={section}
            onToggle={() => onToggleGroup(section)}
            disabled={disabled}
          />
          <Stack
            sx={chipsSx}
            useFlexGap
            direction="row"
            spacing={1}
          >
            {section.tools.map((option) => (
              <ChipWithCheckIcon
                clickable={!disabled}
                key={option.value}
                isSelected={selectedTools.includes(option.value)}
                label={option.label}
                onClick={onSelectTool(option.value)}
                warning={false}
              />
            ))}
          </Stack>
        </Box>
      ))}
    </Box>
  );
}
