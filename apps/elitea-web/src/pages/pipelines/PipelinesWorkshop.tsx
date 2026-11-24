import { useMemo, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';

import { useNavigate } from '@tanstack/react-router';

import { authorDisplayName, isDeletedAuthor } from '@/entities/author';
import { EntityImportButton, useEntityImport, workshopList } from '@/features/agent-lifecycle';
import type { Application } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';
import { EntitySelectionProvider } from '@/shared/ui/EntityCardList';
import { EntityListFiltersBody, useRailTagSelection } from '@/shared/ui/EntityRail';
import { EntityFolderDnd, EntityFolderPanel, EntityFolderScope, MoveSelectionControl, useFolderFilter, useFolderOrder } from '@/widgets/entity-folders';
import { EntityListSidebar, ListSelectionSection, pageBodySx, pageSx, useListSearchQuery, useListSelection } from '@/widgets/page-header';
import { usePermissionSet } from '@/widgets/sidebar';

import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { PipelineListPanel, type PipelineListRow } from './ui/PipelineListPanel';

function forkedFromId(application: Application): string | undefined {
  if (!application.is_forked) return undefined;
  const parentEntityId = application.meta?.['parent_entity_id'];
  return typeof parentEntityId === 'string' || typeof parentEntityId === 'number' ? String(parentEntityId) : undefined;
}

function toRow(application: Application, onSelectForkedFrom: (id: string) => void): PipelineListRow {
  const forkedFrom = forkedFromId(application);
  const badge = workshopList.statusBadge(application.status);
  return {
    id: application.id,
    name: application.name.trim() !== '' ? application.name : 'Untitled',
    description: application.description ?? '',
    // A deleted author's account answers no name or email (#6702).
    authors: (application.authors ?? []).map((author) => ({
      id: author.id, name: authorDisplayName(author), deleted: isDeletedAuthor(author),
    })),
    tags: application.tags ?? [],
    createdAt: application.created_at,
    ...(badge === undefined ? {} : { badge }),
    ...(forkedFrom === undefined ? {} : { forkedFrom: { onClick: () => onSelectForkedFrom(forkedFrom) } }),
  };
}

function emptyCopy(filtered: boolean): { readonly title: string; readonly description: string } {
  if (filtered) {
    return {
      title: t('pages.pipelines.privateList.emptyFound.title', 'Nothing found.'),
      description: t('pages.pipelines.privateList.emptyFound.description', 'Create yours now!'),
    };
  }
  return {
    title: t('pages.pipelines.privateList.empty.title', 'No pipelines yet'),
    description: t(
      'pages.pipelines.privateList.empty.description',
      'Create your first pipeline to get started. Pipelines chain agents, skills and toolkits into one repeatable flow.',
    ),
  };
}


/** A pipeline's id as the folders know it (a number on the wire, a string in the list rows). */
const entityIdOf = (application: Application): number => Number(application.id);

/** The sidebar's pipeline-list action: Import. */
function PipelineListActions({ projectId }: { readonly projectId: string | undefined }): ReactNode {
  const entityImport = useEntityImport(projectId);
  return (
    <EntityImportButton
      testIdPrefix="pipelines"
      acceptsTeams={false}
      isImporting={entityImport.run.isPending}
      onImport={async (request) => {
        await entityImport.run.mutateAsync(request);
      }}
    />
  );
}

/**
 * The Workshop's pipelines (ADR-0051 M1), the pipelines twin of
 * `pages/agents/AgentsWorkshop.tsx`: one list, status as a badge and a filter,
 * every control in the right sidebar's List tab, a summary bar while that
 * sidebar is closed, and multi-select with Add tag, Export and Delete.
 */
export function PipelinesWorkshop(): ReactNode {
  const navigate = useNavigate();
  const projectId = useSelectedProjectId();
  const permissions = usePermissionSet(projectId);
  const query = useListSearchQuery();
  const { selectedTags, toggleTag } = useRailTagSelection();
  const folderId = useFolderFilter(projectId, 'pipeline').folderId;
  const list = workshopList.useListData({ projectId, agentsType: 'pipeline', enabled: true, query, tags: selectedTags, folderId });
  const ids = useMemo(() => list.rows.map((row) => row.id), [list.rows]);
  const sortFields = useMemo(
    () => ({
      items: [
        { value: 'createdAt', label: t('pages.pipelines.pipelines.sortCreated', 'Created') },
        { value: 'name', label: t('pages.pipelines.pipelines.sortName', 'Name') },
      ],
      defaultField: 'createdAt',
    }),
    [],
  );
  const ordered = useFolderOrder({ projectId, kind: 'pipeline', folderId, sortFields, rows: list.rows, idOf: entityIdOf });
  const selected = useListSelection(ids);
  const bulk = workshopList.useBulkActions(projectId, 'pipeline');
  const [confirmingDelete, setConfirmingDelete] = useState(false);
  const facets = useMemo(() => workshopList.facets(list.wire?.facets), [list.wire]);
  const copy = emptyCopy(list.filtered);
  const open = (id: string): void => void navigate({ to: '/pipelines/$tab/$agentId', params: { tab: 'all', agentId: id } });
  const newAction = permissions.has(PERMISSIONS.applications.create)
    ? { label: t('pages.pipelines.pipelines.create', 'New pipeline'), testId: 'pipelines-create', onClick: () => void navigate({ to: '/pipelines/create' }) }
    : undefined;

  return (
    <EntityFolderDnd
      projectId={projectId}
      kind="pipeline"
      permissions={permissions}
      selectedIds={selected.ids}
      folderOrder={ordered.folderOrder}
    >
    <Box sx={pageSx}>
      <EntityListSidebar
        subject={t('pages.pipelines.pipelines.tabsAriaLabel', 'Pipelines')}
        testIdPrefix="pipeline"
        pageKey="pipelines"
        searchPlaceholder={t('pages.pipelines.pipelines.search', 'Search pipelines')}
        newAction={newAction}
        secondaryActions={<PipelineListActions projectId={projectId} />}
        facets={facets}
        sortFields={ordered.sortFields}
        extraChips={selectedTags.map((tag) => ({ id: `tag-${tag}`, label: t('pages.pipelines.pipelines.tagChip', 'Tag: {{tag}}', { tag }), onRemove: () => toggleTag(tag) }))}
        clearParams={{ 'tags[]': [] }}
        folders={
          <EntityFolderPanel
            projectId={projectId}
            kind="pipeline"
            permissions={permissions}
          />
        }
        filters={<EntityListFiltersBody projectId={projectId} />}
        selection={
          selected.ids.length === 0 ? undefined : (
            <ListSelectionSection
              testIdPrefix="pipeline"
              count={selected.ids.length}
              noun={t('pages.pipelines.pipelines.selectionNoun', 'pipelines')}
              onClear={selected.clear}
              onAddTag={(tag) => void bulk.addTag(selected.ids, tag).then(selected.settle)}
              onExport={() => void bulk.exportMany(selected.ids).then(selected.settle)}
              onDelete={() => setConfirmingDelete(true)}
              extra={
                <MoveSelectionControl
                  projectId={projectId}
                  kind="pipeline"
                  permissions={permissions}
                  ids={selected.ids}
                  onSettled={selected.settle}
                  testIdPrefix="pipeline"
                />
              }
              busy={bulk.busy}
              error={bulk.error}
            />
          )
        }
      />
      <Box
        sx={pageBodySx}
        role="tabpanel"
      >
        <EntitySelectionProvider value={selected.selection}>
          <EntityFolderScope
            projectId={projectId}
            kind="pipeline"
            permissions={permissions}
          >
            <PipelineListPanel
              rows={ordered.rows.map((application) => toRow(application, open))}
              isLoading={list.isLoading}
              isError={list.isError}
              errorMessage={t('pages.pipelines.privateList.error', 'Failed to load pipelines.')}
              emptyTitle={copy.title}
              emptyDescription={copy.description}
              onCreate={() => void navigate({ to: '/pipelines/create' })}
              onSelect={open}
              hasMore={list.hasMore}
              isLoadingMore={list.isLoadingMore}
              onLoadMore={list.loadMore}
            />
          </EntityFolderScope>
        </EntitySelectionProvider>
      </Box>
      <DeleteEntityModal
        open={confirmingDelete}
        name={t('pages.pipelines.pipelines.deleteSelection', '{{count}} pipelines', { count: selected.ids.length })}
        confirming={bulk.busy}
        onClose={() => setConfirmingDelete(false)}
        onConfirm={() => {
          void bulk.deleteMany(selected.ids).then((result) => {
            selected.settle(result);
            setConfirmingDelete(false);
          });
        }}
      />
    </Box>
    </EntityFolderDnd>
  );
}
