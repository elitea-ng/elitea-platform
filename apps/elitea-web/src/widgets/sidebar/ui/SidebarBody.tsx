import { Fragment, type ReactNode, useMemo } from 'react';

import { useRouterState } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import Divider from '@mui/material/Divider';
import type { Theme } from '@mui/material/styles';

import type { Project } from '@/entities/project';
import { CreateEntityButton } from '@/widgets/create-button';
import { AgentIcon } from '@/shared/ui/icons/agent-icon';
import { ApplicationsIcon } from '@/shared/ui/icons/applications-icon';
import { ArtifactsIcon } from '@/shared/ui/icons/artifacts-icon';
import { FlowIcon } from '@/shared/ui/icons/flow-icon';
import { KeyIcon } from '@/shared/ui/icons/key-icon';
import { McpIcon } from '@/shared/ui/icons/mcp-icon';
import { SkillIcon } from '@/shared/ui/icons/skill-icon';
import { ToolIcon } from '@/shared/ui/icons/tool-icon';
import ChatBubbleOutlineIcon from '@mui/icons-material/ChatBubbleOutlined';

import { computeIsSelectedProjectPublic, navSections, selectedNavItem, visibleNavSections, type NavItemValue } from '../lib/navSections';
import { ProjectSwitcher } from './ProjectSwitcher';
import { SidebarBottomLinks, SidebarFooterBar } from './SidebarFooter';
import { SidebarHeader } from './SidebarHeader';
import { SidebarNavItem } from './SidebarNavItem';

// [S2 substitution] old app's "Agents" nav icon is `components/Icons/
// ApplicationsIcon.jsx`, a hand-rolled component never routed through
// `shared/assets/svg/**` and therefore not one of S2's 116 ported icons.
// `agent-icon.tsx` (`assets/agent.svg`) is the closest ported asset that
// keeps Agents/Applications visually distinct — same documented-fallback
// precedent as `SidebarHeader`'s `LogoIcon` substitution.
const ICON_BY_VALUE: Record<NavItemValue, ReactNode> = {
  chat: <ChatBubbleOutlineIcon sx={{ width: '1rem', height: '1rem' }} />,
  agents: <AgentIcon />,
  pipelines: <FlowIcon />,
  skills: <SkillIcon />,
  toolkits: <ToolIcon />,
  mcps: <McpIcon />,
  credentials: <KeyIcon />,
  applications: <ApplicationsIcon />,
  artifacts: <ArtifactsIcon />,
};

export interface SidebarBodyProps {
  collapsed: boolean;
  onToggleCollapsed: () => void;
  permissions: ReadonlySet<string>;
  projects: readonly Project[];
  selectedProjectId: string | undefined;
  onSelectProject: (projectId: string, projectName: string) => void;
  /** `SidebarBody.jsx`'s own `onToggleAssistant` prop — present only when the deployment enabled the support assistant. */
  onToggleAssistant?: (() => void) | undefined;
}

/**
 * SHELL-001..012 — the sidebar's scrollable body: header, project switcher,
 * create button, the 3 permission-gated nav groups, footer. Ported from
 * `[fsd]/widgets/sidebar-root/ui/SidebarBody.jsx`.
 *
 * `ChatBubbleOutlineIcon` (`@mui/icons-material`) stands in for the old
 * app's hand-rolled `ChatIcon` (`components/Icons/ChatIcon.jsx`) — not
 * among S2's 116 ported SVGs (`chat-icon.tsx` does not exist in
 * `shared/ui/icons/**`), same documented fallback pattern `SingleSelect`/
 * `BaseModal` already established for gaps in S2's port (R-I1-compliant
 * single-icon import).
 *
 * SHELL-011 ("Sidebar navigation defers API cache reset while streaming")
 * has no home here: TanStack Router's `<Link>` performs the navigation
 * directly (no manual `navigateToPage` wrapper to attach the old app's
 * `isBlockNav`/`isStreaming` deferred-reset behaviour to). The equivalent
 * blocking guarantee is `widgets/app-shell`'s `useBlocker` registration,
 * which intercepts EVERY navigation attempt (this sidebar's links
 * included) at the router level — see that widget's header for why this is
 * a strictly more robust mechanism than re-implementing a per-click check
 * here. The React-Query cache-reset half is not ported: there is no
 * per-project cache to reset yet (no landed Wave-2 unit populates one).
 *
 * `NotificationButton` is NOT here any more. It used to sit directly before
 * `<SidebarFooter>`, which put the bell at the BOTTOM of the rail; the old
 * app has it in the sticky-top header row next to the logo
 * (`SidebarBody.jsx:233`). The misplacement was deliberate and documented —
 * the unit that added it owned this file and the widget but not
 * `SidebarHeader.tsx`, so it could not put the bell where it belonged. It
 * now lives in `SidebarHeader.tsx`, which is that row.
 *
 * `sections` also gates `skills` on the selected project being the public
 * project (`../lib/navSections.ts`'s `computeIsSelectedProjectPublic`) —
 * see that file's header for why `mcps`' equivalent gate is deliberately
 * NOT wired here yet.
 */
export function SidebarBody({
  collapsed,
  onToggleCollapsed,
  permissions,
  projects,
  selectedProjectId,
  onSelectProject,
  onToggleAssistant,
}: SidebarBodyProps): ReactNode {
  const pathname = useRouterState({ select: (routerState) => routerState.location.pathname });

  const sections = useMemo(
    () =>
      visibleNavSections(navSections(), permissions, {
        isSelectedProjectPublic: computeIsSelectedProjectPublic(selectedProjectId),
      }),
    [permissions, selectedProjectId],
  );

  return (
    <Box
      role="presentation"
      sx={(theme: Theme) => ({
        height: '100%',
        display: 'flex',
        flexDirection: 'column',
        background: theme.vars.palette.background.sideBar,
        overflow: 'hidden',
      })}
    >
      <Box sx={{ position: 'sticky', top: 0, zIndex: 1, width: '100%' }}>
        <SidebarHeader
          collapsed={collapsed}
          onToggleCollapsed={onToggleCollapsed}
        />
        <SidebarDivider />
        <ProjectSwitcher
          projects={projects}
          selectedProjectId={selectedProjectId}
          onSelect={onSelectProject}
          collapsed={collapsed}
        />
        <SidebarDivider />
        <Box sx={{ padding: '0.75rem 1rem' }}>
          <CreateEntityButton
            permissions={permissions}
            collapsed={collapsed}
            projectId={selectedProjectId}
          />
        </Box>
        <SidebarDivider />
      </Box>

      <Box sx={{ flex: 1, display: 'flex', flexDirection: 'column', overflowY: 'auto', overflowX: 'hidden' }}>
        {sections.map((section, index) => {
          const selected = selectedNavItem(pathname, section.items);
          return (
            <Fragment key={index}>
              <Box sx={{ paddingBlock: '0.5rem', paddingInline: '1rem', display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
                {section.items.map((item) => (
                  <SidebarNavItem
                    key={item.value}
                    label={item.label}
                    icon={ICON_BY_VALUE[item.value]}
                    to={item.url}
                    selected={item.value === selected}
                    showLabel={!collapsed}
                  />
                ))}
              </Box>
              <SidebarDivider inset />
            </Fragment>
          );
        })}

        {/* `styles.bottomSection` — inside the scroll column, `flex: 1`, so
            Settings + Catalog sit at the BOTTOM of the nav rather than
            directly under the last group. */}
        <SidebarBottomLinks collapsed={collapsed} />
      </Box>

      <SidebarFooterBar
        collapsed={collapsed}
        onToggleAssistant={onToggleAssistant}
      />
    </Box>
  );
}

function SidebarDivider({ inset = false }: { inset?: boolean }): ReactNode {
  return (
    <Divider
      sx={(theme: Theme) => ({
        borderColor: theme.vars.palette.border.sidebarDivider,
        ...(inset ? { marginInline: '1rem' } : {}),
      })}
    />
  );
}
