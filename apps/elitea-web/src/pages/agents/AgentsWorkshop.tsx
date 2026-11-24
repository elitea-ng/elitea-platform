import { useEffect, useMemo, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';

import { useNavigate, useParams } from '@tanstack/react-router';

import { EntityImportButton, useEntityImport, workshopList } from '@/features/agent-lifecycle';
import { ProposalsInboxButton } from '@/features/agent-proposals';
import { TEAM_PERMISSIONS, TeamsPanel } from '@/features/agent-teams';
import type { Application } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';
import { EntitySelectionProvider } from '@/shared/ui/EntityCardList';
import { EntityListFiltersBody, useRailTagSelection } from '@/shared/ui/EntityRail';
import { EntityFolderDnd, EntityFolderPanel, EntityFolderScope, MoveSelectionControl, useFolderFilter, useFolderOrder } from '@/widgets/entity-folders';
import { EntityListSidebar, ListSelectionSection, pageBodySx, pageSx, useListSearchQuery, useListSelection } from '@/widgets/page-header';
import { usePermissionSet } from '@/widgets/sidebar';

import { toApplicationRow } from './lib/applicationRows';
import { useProbationUndo } from './lib/useProbationUndo';
import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { ApplicationListPanel } from './ui/ApplicationListPanel';

/** The two modes of the page: the project's agents, and its saved teams (ADR-0050 decision 8). */
const AGENTS_MODE = 'all';
const TEAMS_MODE = 'teams';

/** Old status tabs (`/agents/drafts`) land on the list with their status filter on. */
const LEGACY_STATUS_TABS: Readonly<Record<string, string>> = { drafts: 'draft', published: 'published' };

type Permissions = ReturnType<typeof usePermissionSet>;

/** An agent's id as the folders know it (a number on the wire, a string in the list rows). */
const entityIdOf = (application: Application): number => Number(application.id);

/** An old status tab or an unknown one lands on the list; a status tab keeps its filter. */
function useLegacyTabRedirect(tab: string | undefined): void {
  const navigate = useNavigate();
  useEffect(() => {
    if (tab === undefined || tab === AGENTS_MODE || tab === TEAMS_MODE) return;
    const legacy = LEGACY_STATUS_TABS[tab];
    void navigate({
      to: '/agents/$tab',
      params: { tab: AGENTS_MODE },
      replace: true,
      search: (prev: Record<string, unknown>) => ({ ...prev, ...(legacy === undefined ? {} : { statuses: [legacy] }) }),
    });
  }, [tab, navigate]);
}

/** The sidebar's agent-list actions: the proposals inbox (ADR-0050 decision 5a, for a person who may create agents) and Import. */
function AgentListActions({ projectId, canCreate }: { readonly projectId: string | undefined; readonly canCreate: boolean }): ReactNode {
  const navigate = useNavigate();
  const entityImport = useEntityImport(projectId);
  return (
    <>
      {canCreate && (
        <ProposalsInboxButton
          projectId={projectId}
          onCreated={(created) => void navigate({ to: '/agents/$tab/$agentId', params: { tab: AGENTS_MODE, agentId: String(created.applicationId) } })}
        />
      )}
      <EntityImportButton
        testIdPrefix="agents"
        isImporting={entityImport.run.isPending}
        onImport={async (request) => {
          await entityImport.run.mutateAsync(request);
        }}
      />
    </>
  );
}

/** What the sidebar shows in both modes: New agent, Import and the Agents | Teams switch. */
/** The row action of an automatic hire still inside its 14-day undo window. */
function undoAction(undo: (() => void) | undefined): { readonly label: string; readonly onClick: () => void } | undefined {
  return undo === undefined ? undefined : { label: t('pages.agents.list.undoHire', 'Undo hire'), onClick: undo };
}

function useSidebarBase(projectId: string | undefined, permissions: Permissions, mode: string) {
  const navigate = useNavigate();
  const canCreate = permissions.has(PERMISSIONS.applications.create);
  const title = t('pages.agents.applications.tabsAriaLabel', 'Agents');
  const showModes = permissions.has(TEAM_PERMISSIONS.list);
  return {
    subject: title,
    testIdPrefix: 'agent',
    pageKey: 'agents',
    searchPlaceholder: t('pages.agents.applications.search', 'Search agents'),
    newAction: canCreate
      ? { label: t('pages.agents.applications.create', 'New agent'), testId: 'agents-create', onClick: () => void navigate({ to: '/agents/create' }) }
      : undefined,
    secondaryActions: (
      <AgentListActions
        projectId={projectId}
        canCreate={canCreate}
      />
    ),
    modes: showModes
      ? {
          items: [
            { value: AGENTS_MODE, label: title, testId: 'agents-mode-all' },
            { value: TEAMS_MODE, label: t('pages.agents.applications.teamsTab', 'Teams'), testId: 'agents-mode-teams' },
          ],
          active: mode,
          onSelect: (value: string) => void navigate({ to: '/agents/$tab', params: { tab: value } }),
        }
      : undefined,
  };
}

type SidebarBase = ReturnType<typeof useSidebarBase>;

/**
 * The Workshop's agents (ADR-0051 M1): one list, no status tabs. Status is a
 * badge on every row and a filter in the right sidebar, next to origin, with
 * the counts the server answers; the search, New agent, Import, sort, the
 * card/table switch and the folders are all in that sidebar's List tab, so the
 * page has no toolbar row. While the sidebar is closed, or a drawer, a summary
 * bar above the list keeps the search text and the active filters on screen.
 * The sidebar's Agents | Teams switch opens the project's saved teams.
 */
export function AgentsWorkshop(): ReactNode {
  const params = useParams({ strict: false }) as { tab?: string };
  const projectId = useSelectedProjectId();
  const permissions = usePermissionSet(projectId);
  const mode = params.tab === TEAMS_MODE && permissions.has(TEAM_PERMISSIONS.list) ? TEAMS_MODE : AGENTS_MODE;
  const base = useSidebarBase(projectId, permissions, mode);
  useLegacyTabRedirect(params.tab);

  return (
    <Box sx={pageSx}>
      {mode === TEAMS_MODE ? (
        <TeamsMode
          base={base}
          projectId={projectId}
        />
      ) : (
        <AgentsListMode
          base={base}
          projectId={projectId}
          permissions={permissions}
        />
      )}
    </Box>
  );
}

/** The Teams mode: the sidebar keeps its switch and New agent, and the body lists the project's saved teams. */
function TeamsMode({ base, projectId }: { readonly base: SidebarBase; readonly projectId: string | undefined }): ReactNode {
  const navigate = useNavigate();
  return (
    <>
      <EntityListSidebar
        {...base}
        listControls={false}
      />
      <Box
        sx={pageBodySx}
        role="tabpanel"
      >
        <TeamsPanel
          projectId={projectId}
          onStarted={(conversation) => void navigate({ to: '/chat/$conversationId', params: { conversationId: conversation.id } })}
        />
      </Box>
    </>
  );
}

function emptyCopy(filtered: boolean): { readonly title: string; readonly description: string } {
  if (filtered) {
    return {
      title: t('pages.agents.privateList.emptyFound.title', 'Nothing found.'),
      description: t('pages.agents.privateList.emptyFound.description', 'Create yours now!'),
    };
  }
  return {
    title: t('pages.agents.privateList.empty.title', 'No agents yet'),
    description: t(
      'pages.agents.privateList.empty.description',
      'Create your first agent to get started. Agents combine a prompt, a model and the toolkits they are allowed to call.',
    ),
  };
}

interface AgentsListModeProps {
  readonly base: SidebarBase;
  readonly projectId: string | undefined;
  readonly permissions: Permissions;
}

/** The agents list: the rows, the facets and the selection over them, and the sidebar that holds their controls. */
function AgentsListMode({ base, projectId, permissions }: AgentsListModeProps): ReactNode {
  const navigate = useNavigate();
  const query = useListSearchQuery();
  const { selectedTags, toggleTag } = useRailTagSelection();
  const folderId = useFolderFilter(projectId, 'agent').folderId;
  const list = workshopList.useListData({ projectId, agentsType: 'classic', enabled: true, query, tags: selectedTags, folderId });
  const undoHireFor = useProbationUndo(projectId ?? '', list.rows);
  const ids = useMemo(() => list.rows.map((row) => row.id), [list.rows]);
  const sortFields = useMemo(
    () => ({
      items: [
        { value: 'createdAt', label: t('pages.agents.applications.sortCreated', 'Created') },
        { value: 'name', label: t('pages.agents.applications.sortName', 'Name') },
      ],
      defaultField: 'createdAt',
    }),
    [],
  );
  const ordered = useFolderOrder({ projectId, kind: 'agent', folderId, sortFields, rows: list.rows, idOf: entityIdOf });
  const selected = useListSelection(ids);
  const bulk = workshopList.useBulkActions(projectId, 'agent');
  const [confirmingDelete, setConfirmingDelete] = useState(false);
  const facets = useMemo(() => workshopList.facets(list.wire?.facets), [list.wire]);
  const copy = emptyCopy(list.filtered);
  const open = (id: string): void => void navigate({ to: '/agents/$tab/$agentId', params: { tab: AGENTS_MODE, agentId: id } });

  return (
    <EntityFolderDnd
      projectId={projectId}
      kind="agent"
      permissions={permissions}
      selectedIds={selected.ids}
      folderOrder={ordered.folderOrder}
    >
      <EntityListSidebar
        {...base}
        facets={facets}
        sortFields={ordered.sortFields}
        extraChips={selectedTags.map((tag) => ({ id: `tag-${tag}`, label: t('pages.agents.applications.tagChip', 'Tag: {{tag}}', { tag }), onRemove: () => toggleTag(tag) }))}
        clearParams={{ 'tags[]': [] }}
        folders={
          <EntityFolderPanel
            projectId={projectId}
            kind="agent"
            permissions={permissions}
          />
        }
        filters={<EntityListFiltersBody projectId={projectId} />}
        selection={
          selected.ids.length === 0 ? undefined : (
            <ListSelectionSection
              testIdPrefix="agent"
              count={selected.ids.length}
              noun={t('pages.agents.applications.selectionNoun', 'agents')}
              onClear={selected.clear}
              onAddTag={(tag) => void bulk.addTag(selected.ids, tag).then(selected.settle)}
              onExport={() => void bulk.exportMany(selected.ids).then(selected.settle)}
              onDelete={() => setConfirmingDelete(true)}
              extra={
                <MoveSelectionControl
                  projectId={projectId}
                  kind="agent"
                  permissions={permissions}
                  ids={selected.ids}
                  onSettled={selected.settle}
                  testIdPrefix="agent"
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
            kind="agent"
            permissions={permissions}
          >
            <ApplicationListPanel
              rows={ordered.rows.map((application) => toApplicationRow(application, open, workshopList.rowBadge(application, list.rows), undoAction(undoHireFor(application))))}
              isLoading={list.isLoading}
              isError={list.isError}
              errorMessage={t('pages.agents.privateList.error', 'Failed to load applications.')}
              emptyTitle={copy.title}
              emptyDescription={copy.description}
              onCreate={() => void navigate({ to: '/agents/create' })}
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
        name={t('pages.agents.applications.deleteSelection', '{{count}} agents', { count: selected.ids.length })}
        confirming={bulk.busy}
        onClose={() => setConfirmingDelete(false)}
        onConfirm={() => {
          void bulk.deleteMany(selected.ids).then((result) => {
            selected.settle(result);
            setConfirmingDelete(false);
          });
        }}
      />
    </EntityFolderDnd>
  );
}
