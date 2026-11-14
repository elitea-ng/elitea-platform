import type { ReactNode } from 'react';
import { useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useParams } from '@tanstack/react-router';

import {
  exportSkill,
  PublicSkillsCatalog,
  SkillImportButton,
  SkillsList,
  useSkillMutations,
  useSkills,
  type SkillRecord,
} from '@/features/skills';
import { t } from '@/shared/i18n';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';
import { EntityListFilters, useRailTagSelection } from '@/shared/ui/EntityRail';
import { EntityFolderPanel, EntityFolderScope, useFolderFilter } from '@/widgets/entity-folders';
import { PageToolbar, pageBodySx, pageSx, useListSearchQuery } from '@/widgets/page-header';
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

const SEARCH_TEST_ID = 'skill-search-input';
export type SkillTab = (typeof SKILL_TABS)[number];

export function resolveSkillTab(tab: string | undefined): SkillTab {
  return tab === 'public' ? 'public' : 'all';
}

/**
 * The skills list page: one toolbar (the All / Public tabs, the search, the
 * Filters button, Import and "+ New skill"), the selected tab below it, and
 * the tag filter in the shell's side panel. The Public tab is the published
 * catalog, which keeps its own search over the catalog's own request.
 */
export function Skills(): ReactNode {
  const navigate = useNavigate();
  const routeParams = useParams({ strict: false }) as { readonly tab?: string };
  const activeTab = resolveSkillTab(routeParams.tab);
  const projectId = useSelectedProjectId();
  const query = useListSearchQuery();
  const [pendingDelete, setPendingDelete] = useState<SkillRecord>();
  const [actionError, setActionError] = useState<string>();
  const skills = useSkills(projectId, query);
  const mutations = useSkillMutations(projectId);
  const { selectedTags } = useRailTagSelection();
  const permissions = usePermissionSet(projectId);
  const folderFilter = useFolderFilter(projectId, 'skill');
  const visibleSkills = useMemo(
    () => filterSkillsByTags(skills.data?.items ?? [], selectedTags).filter((skill) => folderFilter.includes(skill.id)),
    [skills.data, selectedTags, folderFilter],
  );
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
    <Box sx={pageSx}>
      <PageToolbar
        tabs={{
          items: [
            { value: 'all', label: t('skills.page.tabAll', 'Skills') },
            { value: 'public', label: t('skills.page.tabPublic', 'Public') },
          ],
          selectedIndex: SKILL_TABS.indexOf(activeTab),
          onChange: (_event, index) => {
            const next = SKILL_TABS[index];
            if (next !== undefined) void navigate({ to: '/skills/$tab', params: { tab: next } });
          },
          ariaLabel: title,
          testIdPrefix: 'skills-tab',
        }}
        search={onAllTab ? { placeholder: t('skills.page.search', 'Search skills'), 'data-testid': SEARCH_TEST_ID } : undefined}
        filters={onAllTab ? { activeCount: selectedTags.length } : undefined}
        secondaryActions={
          <SkillImportButton
            isImporting={mutations.importFile.isPending}
            onImport={async (file) => {
              await mutations.importFile.mutateAsync(file);
            }}
          />
        }
        primaryAction={createAction}
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
        )}
      </Box>
      {onAllTab && (
        <EntityListFilters
          projectId={projectId}
          subject={title}
          foldersSlot={
            <EntityFolderPanel
              projectId={projectId}
              kind="skill"
              permissions={permissions}
            />
          }
        />
      )}
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
    </Box>
  );
}

const alertSx: SxProps<Theme> = (theme: Theme) => ({ paddingInline: theme.spacing(3), paddingTop: theme.spacing(2), color: theme.vars.palette.error.main });
