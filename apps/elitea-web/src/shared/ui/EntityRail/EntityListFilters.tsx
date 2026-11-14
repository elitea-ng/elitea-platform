import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { useLocation } from '@tanstack/react-router';

import { t } from '@/shared/i18n';
import { SidePanelTab } from '@/shared/ui/side-panel';

import { RailAuthorCard, shouldShowAuthorCard } from './RailAuthorCard';
import { RailTagsPanel } from './RailTagsPanel';
import { RailTrendingAuthors } from './RailTrendingAuthors';
import { useRailTagSelection, useRailViewer } from './useRailContext';
import type { RailStatKind } from './lib/railStatistics';

/**
 * A list page's Filters: the "Tags" filter, then the author card or the
 * trending authors — the baseline's right rail (`components/CardList.jsx:96`:
 * `RightPanel` wrapping `RightInfoPanel`), in the shell's right panel.
 *
 * It used to be a `position: fixed` column pinned 12px from the viewport's
 * right edge at `z-index: 1000`, so on the desktop frame it floated over the
 * title bar and the inspector, and every page had to narrow its own column to
 * keep its controls out from under it. It is now a `<SidePanelTab id="filters">`:
 * the desktop inspector or the web side panel owns its width and its place,
 * and the toolbar's Filters button opens it.
 *
 * Every list page renders exactly this one element. The author card vs
 * trending authors decision is the baseline's (`RightInfoPanel.jsx:25-33`),
 * made from the same three inputs (URL `author_id`, the selected project, the
 * viewer's personal project), with `preferTrendingAuthors` as the explicit
 * override for the public feeds and the agents/pipelines Admin tab
 * (`pages/Applications/PrivateAgentsList.jsx:141-151`).
 */
export interface EntityListFiltersProps {
  /** The project the tag list and the trending-author ranking are scoped to. */
  readonly projectId: string | undefined;
  /** The page's name: the tab's own window (desktop) is titled "Filters — <subject>". */
  readonly subject: string;
  /** The route's `author_id` search param, when the page has one (user-public does). Drives BOTH which panel shows and whose detail is fetched. */
  readonly authorIdFromUrl?: string;
  /** Forces the "Trending Authors" panel — the public feeds and the Admin tab. */
  readonly preferTrendingAuthors?: boolean;
  /** Overrides the pathname-prefix statistic lookup — for `/user-public/:tab`, whose prefix names no entity while its tab does. */
  readonly statKind?: RailStatKind;
  readonly indexesTotal?: number;
  /** The list's project-folder panel (issue 2048), drawn above the tags. The page composes it: `shared/` cannot import the widget that owns it. */
  readonly foldersSlot?: ReactNode;
}

export function EntityListFilters({ projectId, subject, authorIdFromUrl, preferTrendingAuthors = false, statKind, indexesTotal, foldersSlot }: EntityListFiltersProps): ReactNode {
  const location = useLocation();
  const tagSelection = useRailTagSelection();
  const viewer = useRailViewer(authorIdFromUrl);
  const showAuthor = !preferTrendingAuthors && shouldShowAuthorCard(authorIdFromUrl, projectId, viewer.personalProjectId);

  return (
    <SidePanelTab
      id="filters"
      title={t('shell.sidePanel.filters', 'Filters')}
      subject={subject}
    >
      <Box
        data-testid="entity-list-filters"
        sx={filtersPanelSx}
      >
        {foldersSlot}
        <RailTagsPanel
          projectId={projectId}
          selectedTags={tagSelection.selectedTags}
          onToggleTag={tagSelection.toggleTag}
          onClearTags={tagSelection.clearTags}
        />
        {showAuthor ? (
          <RailAuthorCard
            authorId={viewer.authorId}
            pathname={location.pathname}
            {...(statKind === undefined ? {} : { statKind })}
            {...(indexesTotal === undefined ? {} : { indexesTotal })}
          />
        ) : (
          <RailTrendingAuthors projectId={projectId} />
        )}
      </Box>
    </SidePanelTab>
  );
}

/** One padded column; the tags panel absorbs its free height (`RailTagsPanel`'s `panelSx`). */
export const filtersPanelSx: SxProps<Theme> = (theme: Theme) => ({
  flex: 1,
  minHeight: 0,
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(2),
  padding: theme.spacing(2),
  overflowY: 'auto',
});

