/**
 * The desktop's left sidebar, workspace-first: search (the command palette),
 * "Local work" — each opened folder, expandable to the threads run in it
 * (`LocalWorkSection.tsx`) — then a compact, collapsible "Elitea" list that
 * opens the web features in the main area, and Settings at the bottom.
 *
 * Each folder carries its project binding (its menu re-binds it), so there is
 * no global project switcher over the folders; the project the Elitea pages
 * work in is a quiet caption on that section.
 */
import { useMemo, useState } from 'react';

import AccountTreeOutlinedIcon from '@mui/icons-material/AccountTreeOutlined';
import AppsOutlinedIcon from '@mui/icons-material/AppsOutlined';
import AutoAwesomeOutlinedIcon from '@mui/icons-material/AutoAwesomeOutlined';
import BuildOutlinedIcon from '@mui/icons-material/BuildOutlined';
import ChatBubbleOutlineOutlinedIcon from '@mui/icons-material/ChatBubbleOutlineOutlined';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import HelpOutlineOutlinedIcon from '@mui/icons-material/HelpOutlineOutlined';
import HubOutlinedIcon from '@mui/icons-material/HubOutlined';
import Inventory2OutlinedIcon from '@mui/icons-material/Inventory2Outlined';
import KeyOutlinedIcon from '@mui/icons-material/KeyOutlined';
import SearchIcon from '@mui/icons-material/Search';
import SettingsOutlinedIcon from '@mui/icons-material/SettingsOutlined';
import SmartToyOutlinedIcon from '@mui/icons-material/SmartToyOutlined';
import StorefrontOutlinedIcon from '@mui/icons-material/StorefrontOutlined';
import ViewSidebarOutlinedIcon from '@mui/icons-material/ViewSidebarOutlined';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import IconButton from '@mui/material/IconButton';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import type { Project } from '@/entities/project';
import { t } from '@/shared/i18n';
import { computeIsSelectedProjectPublic } from '@/widgets/sidebar';

import { eliteaItems, selectedEliteaItem, type EliteaItemValue } from '../lib/eliteaSections';
import type { ShellAction } from '../lib/shellActions';
import { SIDEBAR_TINT_OPACITY } from '../lib/sidebarTint';
import { useDesktopLayout } from '../model/desktopLayout.store';
import { useEliteaSectionOpen } from '../model/useEliteaSectionOpen';
import type { ShellLocation } from '../model/useShellLocation';
import { LocalWorkSection } from './LocalWorkSection';
import { ShellRow, SectionCaption } from './rows';
import { modKey } from './shortcutLabel';
import { TitleBarSpacer } from './TitleBarSpacer';

const ITEM_ICONS: Record<EliteaItemValue, React.JSX.Element> = {
  chat: <ChatBubbleOutlineOutlinedIcon />,
  agents: <SmartToyOutlinedIcon />,
  pipelines: <AccountTreeOutlinedIcon />,
  skills: <AutoAwesomeOutlinedIcon />,
  toolkits: <BuildOutlinedIcon />,
  mcps: <HubOutlinedIcon />,
  credentials: <KeyOutlinedIcon />,
  applications: <AppsOutlinedIcon />,
  artifacts: <Inventory2OutlinedIcon />,
  catalog: <StorefrontOutlinedIcon />,
  help: <HelpOutlineOutlinedIcon />,
};

export interface DesktopSidebarProps {
  location: ShellLocation;
  run: (action: ShellAction) => void;
  permissions: ReadonlySet<string>;
  projects: readonly Project[];
  selectedProjectId: string | undefined;
  onSelectProject: (projectId: string, projectName: string) => void;
}

function ProjectCaption({
  projects,
  selectedProjectId,
  onSelectProject,
}: Pick<DesktopSidebarProps, 'projects' | 'selectedProjectId' | 'onSelectProject'>): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const current = projects.find((p) => String(p.id) === selectedProjectId);
  return (
    <>
      <ButtonBase
        aria-haspopup="menu"
        aria-label={t('desktop.shell.eliteaProject', 'Project: {{name}}', { name: current?.name ?? '' })}
        onClick={(event) => setAnchor(event.currentTarget)}
        sx={(theme: Theme) => ({ borderRadius: theme.vars.shape.radiusSm, paddingX: 0.5, color: theme.vars.palette.text.metrics })}
      >
        <Typography variant="bodySmall" component="span" sx={{ maxWidth: '8rem', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          {current?.name ?? ''}
        </Typography>
        <ExpandMoreIcon sx={{ width: '0.875rem', height: '0.875rem' }} />
      </ButtonBase>
      <Menu anchorEl={anchor} open={anchor !== null} onClose={() => setAnchor(null)} slotProps={{ list: { dense: true } }}>
        {projects
          .filter((project) => !project.suspended)
          .map((project) => (
            <MenuItem
              key={project.id}
              selected={String(project.id) === selectedProjectId}
              onClick={() => {
                setAnchor(null);
                onSelectProject(String(project.id), project.name);
              }}
            >
              {project.name}
            </MenuItem>
          ))}
      </Menu>
    </>
  );
}

export function DesktopSidebar(props: DesktopSidebarProps): React.JSX.Element {
  const { location, run, permissions, selectedProjectId } = props;
  const items = useMemo(
    () => eliteaItems(permissions, computeIsSelectedProjectPublic(selectedProjectId)),
    [permissions, selectedProjectId],
  );
  const selected = selectedEliteaItem(location.pathname, items);
  const vibrancy = useDesktopLayout((state) => state.platform.vibrancy);
  const [eliteaOpen, toggleElitea] = useEliteaSectionOpen();

  return (
    <Box
      component="aside"
      data-testid="desktop-sidebar"
      sx={(theme: Theme) => ({
        width: '16rem',
        flexShrink: 0,
        height: '100vh',
        display: 'flex',
        flexDirection: 'column',
        boxSizing: 'border-box',
        borderRight: `1px solid ${theme.vars.palette.divider}`,
        position: 'relative',
        isolation: 'isolate',
        // Over the window's native material: a tint of the secondary surface strong
        // enough for AA text on any backdrop (`lib/sidebarTint.ts`); else that surface.
        '&::before': {
          content: '""',
          position: 'absolute',
          inset: 0,
          zIndex: -1,
          background: theme.vars.palette.background.secondary,
          opacity: vibrancy ? SIDEBAR_TINT_OPACITY : 1,
        },
      })}
    >
      <TitleBarSpacer
        leading
        trailing={
          <Tooltip title={`${t('desktop.shell.hideSidebar', 'Hide sidebar')} (${modKey()}\\)`}>
            <IconButton size="small" aria-label={t('desktop.shell.hideSidebar', 'Hide sidebar')} onClick={() => run({ type: 'toggle_sidebar' })}>
              <ViewSidebarOutlinedIcon fontSize="inherit" />
            </IconButton>
          </Tooltip>
        }
      />
      <Box sx={{ paddingX: 1, paddingBottom: 0.5 }}>
        <ButtonBase
          onClick={() => run({ type: 'command_palette' })}
          sx={(theme: Theme) => ({
            width: '100%',
            justifyContent: 'flex-start',
            gap: 1,
            paddingX: 1,
            paddingY: 0.5,
            borderRadius: theme.vars.shape.radiusSm,
            border: `1px solid ${theme.vars.palette.divider}`,
            color: theme.vars.palette.text.metrics,
            '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
          })}
        >
          <SearchIcon sx={{ width: '1rem', height: '1rem' }} />
          <Typography variant="labelSmall" component="span" sx={{ flex: 1, textAlign: 'left' }}>
            {t('desktop.shell.search', 'Search')}
          </Typography>
          <Typography variant="bodySmall" component="kbd" sx={{ fontFamily: 'inherit' }}>
            {`${modKey()}K`}
          </Typography>
        </ButtonBase>
      </Box>
      <Box sx={{ flex: 1, minHeight: 0, overflowY: 'auto', paddingX: 1 }}>
        <LocalWorkSection location={location} run={run} projects={props.projects} />
        <Box component="nav" aria-label={t('desktop.shell.elitea', 'Elitea')}>
          <SectionCaption
            expanded={eliteaOpen}
            onToggle={toggleElitea}
            action={<ProjectCaption projects={props.projects} selectedProjectId={selectedProjectId} onSelectProject={props.onSelectProject} />}
          >
            {t('desktop.shell.elitea', 'Elitea')}
          </SectionCaption>
          <Box component="ul" sx={{ margin: 0, padding: 0, listStyle: 'none' }}>
            {(eliteaOpen ? items : items.filter((item) => item.value === selected)).map((item) => (
              <li key={item.value}>
                <ShellRow
                  testId={`shell-elitea-${item.value}`}
                  icon={ITEM_ICONS[item.value]}
                  label={item.label}
                  selected={selected === item.value}
                  onClick={() => run({ type: 'go', to: item.url })}
                />
              </li>
            ))}
          </Box>
        </Box>
      </Box>
      <Box sx={(theme: Theme) => ({ paddingX: 1, paddingY: 0.5, borderTop: `1px solid ${theme.vars.palette.divider}` })}>
        <ShellRow
          icon={<SettingsOutlinedIcon />}
          label={t('desktop.shell.settings', 'Settings')}
          selected={location.pathname.startsWith('/settings')}
          onClick={() => run({ type: 'settings' })}
        />
      </Box>
    </Box>
  );
}
