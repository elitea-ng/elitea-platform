import type { ReactNode } from 'react';

import { t } from '@/shared/i18n';
import { EntityFolderPanel } from '@/widgets/entity-folders';
import { EntityListSidebar, ListSelectionSection, type PageToolbarPrimaryAction } from '@/widgets/page-header';
import type { usePermissionSet } from '@/widgets/sidebar';

import { toolkitsCopy } from '../lib/toolkitsCopy';
import type { useToolkitSelection } from '../lib/useToolkitSelection';

import { ToolkitsFiltersBody } from './ToolkitsFilters';

interface ToolkitsSidebarTypes {
  readonly tagList: readonly { readonly id: number; readonly name: string }[];
  readonly selected: readonly string[];
  readonly onSelect: (typeName: string) => void;
  readonly onClear: () => void;
  readonly isLoading: boolean;
}

interface ToolkitsSidebarProps {
  readonly isMCP: boolean;
  readonly projectId: string | undefined;
  readonly isPublic: boolean;
  readonly canDelete: boolean;
  readonly permissions: ReturnType<typeof usePermissionSet>;
  readonly createAction: PageToolbarPrimaryAction | undefined;
  readonly types: ToolkitsSidebarTypes;
  readonly selection: ReturnType<typeof useToolkitSelection>;
}

/**
 * The toolkits and MCPs list's right sidebar (ADR-0051 M1): search, New, the
 * types filter, the author card and the folders in one List tab, and the
 * selection section (Delete only: a toolkit has no tags and no export) while
 * rows are selected. No table view, so no card/table switch.
 */
export function ToolkitsSidebar({ isMCP, projectId, isPublic, canDelete, permissions, createAction, types, selection }: ToolkitsSidebarProps): ReactNode {
  const copy = toolkitsCopy(isMCP);
  const kind = isMCP ? 'mcp' : 'toolkit';
  const { selected, setConfirmingDelete, deleting, deleteError } = selection;
  return (
    <EntityListSidebar
      subject={copy.title}
      testIdPrefix={kind}
      pageKey={isMCP ? 'mcps' : 'toolkits'}
      searchPlaceholder={copy.searchPlaceholder}
      newAction={createAction}
      viewToggle={false}
      extraChips={types.selected.map((type) => ({ id: `type-${type}`, label: t('pages.toolkits.toolkits.typeChip', 'Type: {{type}}', { type }), onRemove: () => types.onSelect(type) }))}
      onClearExtra={types.onClear}
      folders={
        <EntityFolderPanel
          enabled={!isPublic}
          projectId={projectId}
          kind={kind}
          permissions={permissions}
        />
      }
      filters={
        <ToolkitsFiltersBody
          projectId={projectId}
          isMCP={isMCP}
          tagList={types.tagList}
          selectedTypes={types.selected}
          onSelectType={types.onSelect}
          onClearTypes={types.onClear}
          isLoading={types.isLoading}
        />
      }
      selection={
        selected.ids.length === 0 || isPublic ? undefined : (
          <ListSelectionSection
            testIdPrefix={kind}
            count={selected.ids.length}
            noun={isMCP ? t('pages.toolkits.toolkits.selectionNounMcp', 'MCPs') : t('pages.toolkits.toolkits.selectionNoun', 'toolkits')}
            onClear={selected.clear}
            onDelete={canDelete ? () => setConfirmingDelete(true) : undefined}
            busy={deleting}
            error={deleteError}
          />
        )
      }
    />
  );
}
