/**
 * The section list of Admin › Configuration (ADR-0059 decision 6; R3.7,
 * issue 2538): the sections the caller may read, grouped under their console
 * area, and a search box over them.
 *
 *  - The list is the server's: the schema list names only the sections the
 *    caller can read (R0.5), so a section that is absent here has no
 *    placeholder either. "Withheld" (listed, with the reason it cannot be
 *    served here) stays as it was: such a row is marked "Not available here".
 *  - Each section names its area in its `access` declaration. The groups
 *    follow the console's area order; a section whose area this build does
 *    not know is listed last, under "Other", rather than dropped.
 *  - The search matches what the page shows: a section's title and the
 *    labels of its fields, ignoring case. Nothing matching says so.
 */
import { useMemo, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import List from '@mui/material/List';
import ListItemButton from '@mui/material/ListItemButton';
import ListItemText from '@mui/material/ListItemText';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';

import { adminConsoleNavAreas } from './adminConsoleAreas';
import type { AdminConfigSection } from './api/adminConfigurationApi';
import { fieldLabel } from './configurationFields';

export interface ConfigurationSectionNavProps {
  readonly sections: readonly AdminConfigSection[];
  readonly activeId: string | undefined;
  /** The row's props: a link for a section that moved to a page, else a button that selects it. */
  readonly rowProps: (section: AdminConfigSection) => Record<string, unknown>;
  /** The row's second line, when the section cannot be served here. */
  readonly secondary: (section: AdminConfigSection) => string | undefined;
}

const OTHER_AREA = 'other';

interface SectionGroup {
  readonly id: string;
  readonly label: string;
  readonly sections: readonly AdminConfigSection[];
}

/** Whether the section's title or one of its field labels holds the query. */
export function sectionMatches(section: AdminConfigSection, query: string): boolean {
  const needle = query.trim().toLocaleLowerCase();
  if (needle === '') return true;
  const texts = [section.title, ...(section.fields ?? []).map(fieldLabel)];
  return texts.some((text) => text.toLocaleLowerCase().includes(needle));
}

/** The sections under their areas, in console order, each group in schema order. */
export function groupSectionsByArea(sections: readonly AdminConfigSection[]): readonly SectionGroup[] {
  const areas = adminConsoleNavAreas();
  const known = new Set<string>(areas.map((area) => area.id));
  const areaOf = (section: AdminConfigSection): string => {
    const area = section.access?.area;
    return area !== undefined && known.has(area) ? area : OTHER_AREA;
  };
  const groups: SectionGroup[] = areas.map((area) => ({
    id: area.id,
    label: area.label,
    sections: sections.filter((section) => areaOf(section) === area.id),
  }));
  groups.push({
    id: OTHER_AREA,
    label: t('pages.admin.configuration.area.other', 'Other'),
    sections: sections.filter((section) => areaOf(section) === OTHER_AREA),
  });
  return groups.filter((group) => group.sections.length > 0);
}

export function ConfigurationSectionNav({ sections, activeId, rowProps, secondary }: ConfigurationSectionNavProps): ReactNode {
  const [query, setQuery] = useState('');
  const groups = useMemo(
    () => groupSectionsByArea(sections.filter((section) => sectionMatches(section, query))),
    [sections, query],
  );
  return (
    <Box sx={{ width: '16rem', flex: '0 0 auto', display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
      <SimpleSearchBar
        value={query}
        onChange={setQuery}
        debounceMs={0}
        placeholder={t('pages.admin.configuration.search', 'Search sections and fields')}
        aria-label={t('pages.admin.configuration.searchLabel', 'Search configuration')}
        data-testid="admin-configuration-search"
      />
      <Box component="nav" aria-label={t('pages.admin.configuration.sections', 'Configuration sections')}>
        {groups.length === 0 && sections.length > 0 ? (
          <Typography variant="bodyMedium" color="text.secondary" data-testid="admin-configuration-search-empty">
            {t('pages.admin.configuration.searchEmpty', 'No section or field matches “{{query}}”.', {
              query: query.trim(),
            })}
          </Typography>
        ) : null}
        {groups.map((group) => (
          <Box
            key={group.id}
            component="section"
            aria-labelledby={`admin-configuration-area-${group.id}`}
            data-testid={`admin-configuration-area-${group.id}`}
          >
            <Typography
              id={`admin-configuration-area-${group.id}`}
              variant="labelSmall"
              component="h2"
              color="text.secondary"
              sx={{ margin: '0.5rem 0 0.25rem', textTransform: 'uppercase' }}
            >
              {group.label}
            </Typography>
            {/* `component="div"`: `ListItemButton` renders a `<div>`, and a
                `<ul>` of divs is an axe `list` violation (journey 34). */}
            <List component="div" dense disablePadding>
              {group.sections.map((section) => (
                <ListItemButton key={section.id} selected={section.id === activeId} {...rowProps(section)}>
                  <ListItemText primary={section.title} secondary={secondary(section)} />
                </ListItemButton>
              ))}
            </List>
          </Box>
        ))}
      </Box>
    </Box>
  );
}
