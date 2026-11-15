import type { ReactNode } from 'react';
import { useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useParams, useSearch } from '@tanstack/react-router';

import {
  exportSkill,
  PublicSkillsCatalog,
  SkillImportButton,
  SkillsList,
  useSkillBulkActions,
  useSkillMutations,
  useSkills,
  type SkillRecord,
} from '@/features/skills';
import { t } from '@/shared/i18n';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';
import { EntitySelectionProvider } from '@/shared/ui/EntityCardList';
import { EntityListFiltersBody, useRailTagSelection } from '@/shared/ui/EntityRail';
import { EntityFolderDnd, EntityFolderPanel, EntityFolderScope, MoveSelectionControl, useFolderFilter } from '@/widgets/entity-folders';
import { EntityListSidebar, ListSelectionSection, pageBodySx, pageSx, useListSearchQuery, useListSelection } from '@/widgets/page-header';
import { usePermissionSet } from '@/widgets/sidebar';

import { useSelectedProjectId } from './lib/useSelectedProjectId';

/**
 * Client-side tag filter for the skills list.
 *
 * Real, unlike the agents/pipelines/user-public side: a `SkillRecord`
 * carries its own `tags: readonly string[]`
 * (`features/skills/model/types.ts:7`) and elitea-main's skills repo really
 * populates it (`internal/infra/db/repos/skills.go:53,482,521`), so the
 * rail's selection can be applied here without a server round trip
 * (`fetchSkills` accepts no `tags` request param).
 *
 * AND (every-of) matching, the same predicate the already-ported
 * `pages/user-public/lib/merge-and-sort.ts` uses for the same selection —
 * itself a port of the baseline's `selectedTagIdList.every(...)`
 * (`AllStuffList.jsx:150-179`). Kept identical so the two tag filters in
 * this app cannot disagree about what selecting two chips means.
 */
export function filterSkillsByTags<T extends { readonly tags?: readonly string[] | undefined }>(items: readonly T[], selectedTags: readonly string[]): T[] {
  if (selectedTags.length === 0) return [...items];
  return items.filter((item) => selectedTags.every((selected) => (item.tags ?? []).includes(selected)));
}

/** Sorts the listed skills by the sidebar's sort field. The list endpoint answers newest first, so no field means no reordering. */
export function sortSkills<T extends { readonly name: string; readonly created_at?: string | undefined }>(items: readonly T[], sortBy: string | undefined, ascending: boolean): T[] {
  if ((sortBy === undefined || sortBy === '') && !ascending) return [...items];
  const direction = ascending ? 1 : -1;
  const key = (item: T): string => (sortBy === 'name' ? item.name : (item.created_at ?? ''));
  return [...items].sort((a, b) => key(a).localeCompare(key(b)) * direction);
}

function downloadMarkdown(content: string, filename: string): void {
  const url = URL.createObjectURL(new Blob([content], { type: 'text/markdown' }));
  const link = document.createElement('a');
  link.href = url;
  link.download = filename;
  link.click();
  URL.revokeObjectURL(url);
}

/**
 * The two tabs `/skills/$tab` addresses.
 *
 * The route has carried a `:tab` segment since it was ported and nothing read
 * it — the page rendered the project's own list whatever the segment said, so
 * `/skills/public` and `/skills/all` were the same screen. They are not any
 * more: `public` is the published catalog, which is what the `:tab` segment was
 * for in the baseline's own `SkillsTabs` constant.
 *
 * Any other value resolves to `all`, so an old link cannot land on a blank tab.
 */
export const SKILL_TABS = ['all', 'public'] as const;

export type SkillTab = (typeof SKILL_TABS)[number];

export function resolveSkillTab(tab: string | undefined): SkillTab {
  return tab === 'public' ? 'public' : 'all';
}

/**
 * The skills list page (ADR-0051 M1): the project's skills with every control
 * in the right sidebar's List tab — search, New skill, Import, sort, the
 * card/table switch, the folders and the tag filter — and no toolbar row. A
 * summary bar above the list keeps the search text and the active tags on
 * screen while the sidebar is closed. The Skills | Public switch in the sidebar
 * chooses between the project's own skills and the published catalog, which
 * keeps its own search over the catalog's own request. Rows are selectable for
 * Add tag, Export and Delete.
 */
export function Skills(): ReactNode {
  const navigate = useNavigate();
  const routeParams = useParams({ strict: false }) as { readonly tab?: string };
  const search = useSearch({ strict: false }) as { readonly sort_by?: string; readonly sort_order?: string };
  const activeTab = resolveSkillTab(routeParams.tab);
  const projectId = useSelectedProjectId();
  const query = useListSearchQuery();
  const [pendingDelete, setPendingDelete] = useState<SkillRecord>();
  const [confirmingBulkDelete, setConfirmingBulkDelete] = useState(false);
  const [actionError, setActionError] = useState<string>();
  const skills = useSkills(projectId, query);
  const mutations = useSkillMutations(projectId);
  const bulk = useSkillBulkActions(projectId);
  const { selectedTags, toggleTag } = useRailTagSelection();
  const permissions = usePermissionSet(projectId);
  const folderFilter = useFolderFilter(projectId, 'skill');
  const visibleSkills = useMemo(
    () => sortSkills(filterSkillsByTags(skills.data?.items ?? [], selectedTags).filter((skill) => folderFilter.includes(skill.id)), search.sort_by, search.sort_order === 'asc'),
    [skills.data, selectedTags, folderFilter, search.sort_by, search.sort_order],
  );
  const visibleIds = useMemo(() => visibleSkills.map((skill) => skill.id), [visibleSkills]);
  const selected = useListSelection(visibleIds);
  const title = t('skills.page.title', 'Skills');
  const openCreate = (): void => void navigate({ to: '/skills/create' });

  const handleExport = async (skill: SkillRecord): Promise<void> => {
    if (!projectId) return;
    setActionError(undefined);
    try {
      downloadMarkdown(await exportSkill(projectId, skill.id), `${skill.name || 'skill'}.md`);
    } catch {
      setActionError(t('skills.page.exportError', 'Failed to export the skill.'));
    }
  };

  const onAllTab = activeTab === 'all';
  const createAction = permissions.has(PERMISSIONS.applications.create)
    ? { label: t('pages.skills.create', 'New skill'), testId: 'skills-create', onClick: openCreate }
    : undefined;

  return (
    <EntityFolderDnd
      projectId={projectId}
      kind="skill"
      permissions={permissions}
      selectedIds={selected.ids}
    >
    <Box sx={pageSx}>
      <EntityListSidebar
        subject={title}
        testIdPrefix="skill"
        pageKey="skills"
        searchPlaceholder={t('skills.page.search', 'Search skills')}
        newAction={createAction}
        secondaryActions={
          <SkillImportButton
            isImporting={mutations.importFile.isPending}
            onImport={async (file) => {
              await mutations.importFile.mutateAsync(file);
            }}
          />
        }
        modes={{
          items: [
            { value: 'all', label: t('skills.page.tabAll', 'Skills'), testId: 'skills-mode-all' },
            { value: 'public', label: t('skills.page.tabPublic', 'Public'), testId: 'skills-mode-public' },
          ],
          active: activeTab,
          onSelect: (next) => void navigate({ to: '/skills/$tab', params: { tab: next } }),
        }}
        listControls={onAllTab}
        sortFields={{
          items: [
            { value: 'createdAt', label: t('skills.page.sortCreated', 'Created') },
            { value: 'name', label: t('skills.page.sortName', 'Name') },
          ],
          defaultField: 'createdAt',
        }}
        extraChips={selectedTags.map((tag) => ({ id: `tag-${tag}`, label: t('skills.page.tagChip', 'Tag: {{tag}}', { tag }), onRemove: () => toggleTag(tag) }))}
        clearParams={{ 'tags[]': [] }}
        folders={
          <EntityFolderPanel
            projectId={projectId}
            kind="skill"
            permissions={permissions}
          />
        }
        filters={<EntityListFiltersBody projectId={projectId} />}
        selection={
          selected.ids.length === 0 ? undefined : (
            <ListSelectionSection
              testIdPrefix="skill"
              count={selected.ids.length}
              noun={t('skills.page.selectionNoun', 'skills')}
              onClear={selected.clear}
              onAddTag={(tag) => void bulk.addTag(selected.ids, tag).then(selected.settle)}
              onExport={() => void bulk.exportMany(selected.ids).then(selected.settle)}
              onDelete={() => setConfirmingBulkDelete(true)}
              extra={
                <MoveSelectionControl
                  projectId={projectId}
                  kind="skill"
                  permissions={permissions}
                  ids={selected.ids}
                  onSettled={selected.settle}
                  testIdPrefix="skill"
                />
              }
              busy={bulk.busy}
              error={bulk.error}
            />
          )
        }
      />
      <Box sx={pageBodySx}>
        {actionError && (
          <Typography
            role="alert"
            sx={alertSx}
          >
            {actionError}
          </Typography>
        )}
        {activeTab === 'public' && (
          <PublicSkillsCatalog
            projectId={projectId}
            permissions={permissions}
          />
        )}
        {onAllTab && (
          <EntitySelectionProvider value={selected.selection}>
            <EntityFolderScope
              projectId={projectId}
              kind="skill"
              permissions={permissions}
            >
              <SkillsList
                items={visibleSkills}
                isLoading={skills.isFetching && skills.data === undefined}
                isError={skills.isError}
                query={query}
                onSelect={(skillId) => {
                  void navigate({
                    to: '/skills/$tab/$skillId',
                    params: { tab: 'all', skillId },
                  });
                }}
                onDelete={setPendingDelete}
                onExport={(skill) => {
                  void handleExport(skill);
                }}
                onCreate={openCreate}
              />
            </EntityFolderScope>
          </EntitySelectionProvider>
        )}
      </Box>
      <DeleteEntityModal
        open={pendingDelete !== undefined}
        {...(pendingDelete ? { name: pendingDelete.name } : {})}
        confirming={mutations.remove.isPending}
        onClose={() => setPendingDelete(undefined)}
        onConfirm={() => {
          if (!pendingDelete) return;
          void mutations.remove
            .mutateAsync({ skillId: pendingDelete.id })
            .then(() => setPendingDelete(undefined))
            .catch(() => setActionError(t('skills.page.deleteError', 'Failed to delete the skill.')));
        }}
      />
      <DeleteEntityModal
        open={confirmingBulkDelete}
        name={t('skills.page.deleteSelection', '{{count}} skills', { count: selected.ids.length })}
        confirming={bulk.busy}
        onClose={() => setConfirmingBulkDelete(false)}
        onConfirm={() => {
          void bulk.deleteMany(selected.ids).then((result) => {
            selected.settle(result);
            setConfirmingBulkDelete(false);
          });
        }}
      />
    </Box>
    </EntityFolderDnd>
  );
}

const alertSx: SxProps<Theme> = (theme: Theme) => ({ paddingInline: theme.spacing(3), paddingTop: theme.spacing(2), color: theme.vars.palette.error.main });
