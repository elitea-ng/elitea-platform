import { type ReactNode, useMemo } from 'react';

import { useNavigate, useRouterState } from '@tanstack/react-router';

import AddIcon from '@mui/icons-material/Add';
import Box from '@mui/material/Box';
import Divider from '@mui/material/Divider';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';

import type { Project } from '@/entities/project';
import { t } from '@/shared/i18n';
import { NAV_ICONS, NavRows, useStartChat } from '@/widgets/chat-rail';

import { computeIsSelectedProjectPublic, navSections, selectedNavItem, visibleNavSections, type NavItem } from '../lib/navSections';
import { useCreateGate } from '../model/useCreateGate';
import { ProjectSwitcher } from './ProjectSwitcher';
import { SidebarChats } from './SidebarChats';
import { SidebarAccount, SidebarFooterBar } from './SidebarFooter';
import { SidebarHeader } from './SidebarHeader';
import { SidebarNavItem } from './SidebarNavItem';

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

/** The rows the rail shows: every item the person may open (SHELL-010, Skills off in the public project). */
function useNavItems(permissions: ReadonlySet<string>, projectId: string | undefined): readonly NavItem[] {
  return useMemo(
    () => visibleNavSections(navSections(), permissions, { isSelectedProjectPublic: computeIsSelectedProjectPublic(projectId) }).flatMap((section) => section.items),
    [permissions, projectId],
  );
}

/**
 * SHELL-001..012 — the web rail, laid out as the desktop sidebar is: the
 * header (logo, bell, collapse), the project switcher, then the Chats with
 * their list (`SidebarChats.tsx`), the place rows — Applications, Artifacts
 * and the "Workshop" (agents, pipelines, skills, toolkits, MCPs) — each with
 * its "+" where the person may create one (`useCreateGate`), and at the
 * bottom the account menu (Settings, Credentials, the Catalog, help, the
 * version and Sign out, as on the desktop), over the Support Bot bar.
 *
 * Collapsed (64px) the rail keeps the icons — one per place, and one "New
 * chat" — and leaves the chat list out.
 *
 * Navigation is plain `<Link>`s; `widgets/app-shell`'s `useBlocker`
 * intercepts each one (an unsaved editor asks first).
 */
export function SidebarBody({ collapsed, onToggleCollapsed, permissions, projects, selectedProjectId, onSelectProject, onToggleAssistant }: SidebarBodyProps): ReactNode {
  const pathname = useRouterState({ select: (routerState) => routerState.location.pathname });
  const items = useNavItems(permissions, selectedProjectId);
  const mayCreate = useCreateGate(permissions, selectedProjectId);
  const hasCredentials = items.some((item) => item.value === 'credentials');
  const selected = selectedNavItem(pathname, items);

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
        <SidebarHeader collapsed={collapsed} onToggleCollapsed={onToggleCollapsed} />
        <SidebarDivider />
        <ProjectSwitcher projects={projects} selectedProjectId={selectedProjectId} onSelect={onSelectProject} collapsed={collapsed} />
        <SidebarDivider />
      </Box>

      <Box sx={{ flex: 1, display: 'flex', flexDirection: 'column', overflowY: 'auto', overflowX: 'hidden' }}>
        {collapsed ? (
          <CollapsedRows items={items.filter((item) => item.value !== 'credentials')} selected={selected} canCreateChat={mayCreate('chat')} />
        ) : (
          <Box component="nav" aria-label={t('widgets.sidebar.places', 'Places')} sx={{ paddingInline: '0.5rem' }}>
            {items.some((item) => item.value === 'chat') && <SidebarChats permissions={permissions} projectId={selectedProjectId} pathname={pathname} canCreateChat={mayCreate('chat')} />}
            <Box component="ul" sx={{ margin: 0, padding: 0, paddingTop: 1.5, listStyle: 'none' }}>
              <PlaceRows items={items} selected={selected} mayCreate={mayCreate} />
            </Box>
          </Box>
        )}
      </Box>

      <SidebarAccount collapsed={collapsed} credentials={hasCredentials} />
      <SidebarFooterBar collapsed={collapsed} onToggleAssistant={onToggleAssistant} />
    </Box>
  );
}

/** The place rows (all but the Chats and Credentials), each "+" going to its create page. */
function PlaceRows({ items, selected, mayCreate }: { readonly items: readonly NavItem[]; readonly selected: string | undefined; readonly mayCreate: (value: string) => boolean }): ReactNode {
  const navigate = useNavigate();
  const rows = items
    .filter((item) => item.value !== 'chat' && item.value !== 'credentials')
    .map((item) => ({
      value: item.value,
      label: item.label,
      url: item.url,
      create:
        item.create !== undefined && mayCreate(item.value)
          ? { label: item.create.label, testId: `shell-create-${item.value}`, onCreate: () => void navigate({ to: item.create?.url ?? item.url }) }
          : undefined,
    }));
  return <NavRows items={rows} selected={selected} />;
}

/** The collapsed rail: "New chat", then one icon per place. */
function CollapsedRows({ items, selected, canCreateChat }: { readonly items: readonly NavItem[]; readonly selected: string | undefined; readonly canCreateChat: boolean }): ReactNode {
  const newChat = useStartChat();
  const label = t('widgets.sidebar.create.chat', 'New chat');
  return (
    <Box sx={{ paddingBlock: '0.5rem', paddingInline: '1rem', display: 'flex', flexDirection: 'column', alignItems: 'center', gap: '0.5rem' }}>
      {canCreateChat && (
        <Tooltip title={label} placement="right">
          <IconButton size="small" aria-label={label} data-testid="sidebar-new-chat" onClick={newChat}>
            <AddIcon fontSize="small" />
          </IconButton>
        </Tooltip>
      )}
      {items.map((item) => (
        <SidebarNavItem key={item.value} label={item.label} icon={NAV_ICONS[item.value]} to={item.url} selected={item.value === selected} showLabel={false} />
      ))}
    </Box>
  );
}

function SidebarDivider(): ReactNode {
  return <Divider sx={(theme: Theme) => ({ borderColor: theme.vars.palette.border.sidebarDivider })} />;
}
