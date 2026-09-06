import type { ReactNode } from 'react';
import { useMemo, useState } from 'react';

import AddOutlinedIcon from '@mui/icons-material/AddOutlined';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';

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
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';
import { EntityListRail, RAIL_CONTENT_WIDTH, useEntityRailVisible, useRailTagSelection } from '@/shared/ui/EntityRail';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';
import { usePermissionSet, useSidebarCollapsedStore } from '@/widgets/sidebar';

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
export type SkillTab = (typeof SKILL_TABS)[number];

export function resolveSkillTab(tab: string | undefined): SkillTab {
  return tab === 'public' ? 'public' : 'all';
}

export function Skills(): ReactNode {
  const navigate = useNavigate();
  const routeParams = useParams({ strict: false }) as { readonly tab?: string };
  const activeTab = resolveSkillTab(routeParams.tab);
  const projectId = useSelectedProjectId();
  const [query, setQuery] = useState('');
  const [pendingDelete, setPendingDelete] = useState<SkillRecord>();
  const [actionError, setActionError] = useState<string>();
  const skills = useSkills(projectId, query);
  const mutations = useSkillMutations(projectId);
  const navRailCollapsed = useSidebarCollapsedStore((state) => state.collapsed);
  const railVisible = useEntityRailVisible(navRailCollapsed);
  const { selectedTags } = useRailTagSelection();
  const permissions = usePermissionSet(projectId);
  const visibleSkills = useMemo(() => filterSkillsByTags(skills.data?.items ?? [], selectedTags), [skills.data, selectedTags]);

  const handleExport = async (skill: SkillRecord): Promise<void> => {
    if (!projectId) return;
    setActionError(undefined);
    try {
      downloadMarkdown(await exportSkill(projectId, skill.id), `${skill.name || 'skill'}.md`);
    } catch {
      setActionError(t('skills.page.exportError', 'Failed to export the skill.'));
    }
  };

  const searchBar = (
    <SimpleSearchBar
      value={query}
      onChange={setQuery}
      placeholder={t('skills.page.search', 'Search skills')}
    />
  );

  return (
    <Box sx={pageSx}>
      <Box sx={contentWidthSx(railVisible)}>
        <Box sx={headerSx}>
          <Typography variant="headingLarge">{t('skills.page.title', 'Skills')}</Typography>
          <Box sx={actionsSx}>
            <SkillImportButton
              isImporting={mutations.importFile.isPending}
              onImport={async (file) => {
                await mutations.importFile.mutateAsync(file);
              }}
            />
            <BaseBtn
              variant="contained"
              startIcon={<AddOutlinedIcon />}
              onClick={() => void navigate({ to: '/skills/create' })}
            >
              {t('skills.page.create', 'Create skill')}
            </BaseBtn>
          </Box>
        </Box>
        <Tabs
          value={activeTab}
          onChange={(_event, next: SkillTab) => {
            void navigate({ to: '/skills/$tab', params: { tab: next } });
          }}
        >
          <Tab
            value="all"
            label={t('skills.page.tabAll', 'Skills')}
          />
          <Tab
            value="public"
            label={t('skills.page.tabPublic', 'Public')}
          />
        </Tabs>
        {activeTab === 'public' && (
          <PublicSkillsCatalog
            projectId={projectId}
            permissions={permissions}
          />
        )}
        {activeTab === 'all' && !railVisible && searchBar}
        {actionError && <Typography role="alert">{actionError}</Typography>}
        {activeTab === 'all' && (
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
          onCreate={() => {
            void navigate({ to: '/skills/create' });
          }}
          railVisible={false}
        />
        )}
      </Box>
      <EntityListRail
        projectId={projectId}
        navRailCollapsed={navRailCollapsed}
        search={searchBar}
      />
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

const pageSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(2),
  padding: theme.spacing(3),
});
/** `CARD_LIST_WIDTH` (`apps/elitea-ui/src/common/constants.js:511`) — see `pages/agents/Applications.tsx` for the shared rationale. */
const contentWidthSx = (railVisible: boolean): SxProps<Theme> => (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(2),
  width: railVisible ? RAIL_CONTENT_WIDTH : '100%',
});
const headerSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', justifyContent: 'space-between' };
const actionsSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: 1 };
