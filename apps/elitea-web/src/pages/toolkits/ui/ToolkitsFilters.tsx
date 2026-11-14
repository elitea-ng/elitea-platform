import type { ReactNode } from 'react';

import { t } from '@/shared/i18n';
import { RailTagsPanelView } from '@/shared/ui/EntityRail';

import { ToolkitsAuthorCard } from './ToolkitsAuthorCard';

/**
 * The toolkits / MCPs list's own filters in the List tab of the shell's right
 * panel (`EntityListSidebar`): the "Types" filter over the loaded rows, then
 * the viewer's author card. The same panels the other list pages show
 * (`shared/ui/EntityRail`'s `EntityListFiltersBody`), with the toolkit types
 * where they show tags.
 */
export interface ToolkitsFiltersBodyProps {
  readonly projectId: string | undefined;
  readonly isMCP: boolean;
  readonly tagList: readonly { readonly id: number; readonly name: string }[];
  readonly selectedTypes: readonly string[];
  readonly onSelectType: (typeName: string) => void;
  readonly onClearTypes: () => void;
  readonly isLoading: boolean;
}

export function ToolkitsFiltersBody({ projectId, isMCP, tagList, selectedTypes, onSelectType, onClearTypes, isLoading }: ToolkitsFiltersBodyProps): ReactNode {
  return (
    <>
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
    </>
  );
}
