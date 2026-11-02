import type { ReactNode } from 'react';

import Box from '@mui/material/Box';

import { t } from '@/shared/i18n';
import { filtersPanelSx, RailTagsPanelView } from '@/shared/ui/EntityRail';
import { SidePanelTab } from '@/shared/ui/side-panel';

import { ToolkitsAuthorCard } from './ToolkitsAuthorCard';

/**
 * The toolkits / MCPs list's Filters tab in the shell's side panel: the
 * "Types" filter over the loaded rows, then the viewer's author card. The
 * same panel the other list pages show (`shared/ui/EntityRail`'s
 * `EntityListFilters`), with the toolkit types where they show tags.
 */
export interface ToolkitsFiltersProps {
  readonly projectId: string | undefined;
  readonly isMCP: boolean;
  readonly subject: string;
  readonly tagList: readonly { readonly id: number; readonly name: string }[];
  readonly selectedTypes: readonly string[];
  readonly onSelectType: (typeName: string) => void;
  readonly onClearTypes: () => void;
  readonly isLoading: boolean;
}

export function ToolkitsFilters({ projectId, isMCP, subject, tagList, selectedTypes, onSelectType, onClearTypes, isLoading }: ToolkitsFiltersProps): ReactNode {
  return (
    <SidePanelTab
      id="filters"
      title={t('shell.sidePanel.filters', 'Filters')}
      subject={subject}
    >
      <Box
        data-testid="toolkits-filters"
        sx={filtersPanelSx}
      >
        <RailTagsPanelView
          title={t('pages.toolkits.toolkits.types', 'Types')}
          emptyText={t('pages.toolkits.toolkits.noTypes', 'No types to display.')}
          tags={tagList}
          selectedTags={selectedTypes}
          onToggleTag={onSelectType}
          onClearTags={onClearTypes}
          isLoading={isLoading}
          isError={false}
        />
        <ToolkitsAuthorCard
          projectId={projectId}
          isMCP={isMCP}
        />
      </Box>
    </SidePanelTab>
  );
}
