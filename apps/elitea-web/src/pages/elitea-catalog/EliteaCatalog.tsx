/**
 * ELITEA Catalog — the two-tab shell over the agent and skill catalogues.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/pages/elitea-catalog/EliteaCatalog.jsx`
 * (baseline 20b23c42), which composes `agent-hub`'s `AgentsTab` and
 * `skill-hub`'s `SkillsTab` behind a `?tab=` query param. Upstream did NOT
 * delete `agent-hub`; it wrapped it. `/agents-hub` survives there only as a
 * `LegacyCatalogRedirect` (`ProtectedRoutes.jsx:194`), and this port keeps
 * that shape — see `routes/_shell/agents-hub.tsx`.
 *
 * DELIBERATE DIVERGENCE from the baseline shell: no shared search box.
 * The baseline shell owns a single search field and pushes its value down as
 * a `query` prop, because its two tab bodies are search-less. This app's two
 * bodies are not: `pages/agents-hub/AgentHub.tsx` and
 * `features/skills/ui/PublicSkillsCatalog.tsx` each already own a
 * `shared/ui/CategoryFilter` with its own search field and category chips
 * (both were built that way before this shell existed). Hoisting a second
 * search box above them would either duplicate the control or require
 * rewriting both bodies — and the brief for this port is explicitly not to
 * rewrite either tab body. So the shell contributes the heading, the tabs and
 * the URL contract, and nothing else.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';
import Typography from '@mui/material/Typography';

import { useNavigate, useSearch } from '@tanstack/react-router';

import { PublicSkillsCatalog } from '@/features/skills';
import AgentHub from '@/pages/agents-hub/AgentHub';
import { useSelectedProjectId } from '@/pages/skills/lib/useSelectedProjectId';
import { t } from '@/shared/i18n';
import { usePermissionSet } from '@/widgets/sidebar';

export const CATALOG_TABS = ['agents', 'skills'] as const;
export type CatalogTab = (typeof CATALOG_TABS)[number];

/**
 * Which tab a raw `?tab=` value selects.
 *
 * Anything that is not the literal `skills` lands on `agents` — the same
 * rule the baseline's `activeTab` useMemo applies (`tab === 'skills' ?
 * 'skills' : 'agents'`), so a stale or hand-typed value degrades to the
 * default tab instead of rendering nothing.
 */
export function catalogTabFromSearch(tab: string | undefined): CatalogTab {
  return tab === 'skills' ? 'skills' : 'agents';
}

interface CatalogSearch {
  tab?: string;
}

export function EliteaCatalog(): ReactNode {
  // Read loosely, per this codebase's convention (see `AgentHub.tsx`'s own
  // note): a page must not depend on which route file mounts it.
  const search = useSearch({ strict: false }) as CatalogSearch;
  const navigate = useNavigate();
  const activeTab = catalogTabFromSearch(search.tab);

  const projectId = useSelectedProjectId();
  const permissions = usePermissionSet(projectId);

  return (
    <Box
      data-testid="elitea-catalog"
      sx={{ display: 'flex', flexDirection: 'column', height: '100%', minHeight: 0, boxSizing: 'border-box' }}
    >
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1, padding: '1.5rem 1.5rem 0 1.5rem' }}>
        <Typography
          variant="headingLarge"
          component="h1"
          data-testid="catalog-page-heading"
        >
          {t('pages.eliteaCatalog.title', 'Welcome to ELITEA Catalog!')}
        </Typography>
        <Tabs
          value={activeTab}
          onChange={(_event, next: CatalogTab) => {
            if (next === activeTab) return;
            void navigate({ to: '/elitea-catalog', search: { tab: next }, replace: true });
          }}
        >
          <Tab
            value="agents"
            label={t('pages.eliteaCatalog.tabAgents', 'Agents')}
            data-testid="catalog-agents-tab"
          />
          <Tab
            value="skills"
            label={t('pages.eliteaCatalog.tabSkills', 'Skills')}
            data-testid="catalog-skills-tab"
          />
        </Tabs>
      </Box>
      <Box sx={{ flex: 1, minHeight: 0, overflow: 'auto' }}>
        {activeTab === 'skills' ? (
          <Box sx={{ padding: '1rem 1.5rem' }}>
            <PublicSkillsCatalog
              projectId={projectId}
              permissions={permissions}
            />
          </Box>
        ) : (
          <AgentHub />
        )}
      </Box>
    </Box>
  );
}
