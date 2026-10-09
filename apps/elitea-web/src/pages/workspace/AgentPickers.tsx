/**
 * The thread header's selectors: the folder's project (changing it re-binds
 * the folder on the host), the agent (the bound project's, see
 * `useAgentSelection`) and its version. Compact, borderless "value ▾"
 * controls, the way a native app's toolbar shows them; each keeps its name
 * for assistive technology.
 *
 * Which conversation the turn goes to is the THREAD, chosen in the sidebar;
 * there is no conversation selector here.
 *
 * A project without agents gets `NoAgents` (rendered by the page in the
 * thread body) instead of empty selects: create an agent in it, or bind the
 * folder to another project. Nothing is created on the server from here.
 */
import type { ReactNode, Ref } from 'react';

import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { ProjectChoice } from '@/features/workspace';
import { t } from '@/shared/i18n';

import type { AgentSelection } from './useAgentSelection';

export interface AgentPickersProps {
  selection: AgentSelection;
  projectId: number;
  projects: readonly ProjectChoice[];
  /** A turn runs: the folder cannot move to another project now. */
  busy: boolean;
  onChangeProject: (projectId: number) => void;
  /** The project menu is opened from elsewhere too (`NoAgents`' "Use another project"). */
  projectMenuOpen: boolean;
  onProjectMenuOpenChange: (open: boolean) => void;
  /** The box holding the agent select (the composer's `/agent` focuses it). */
  agentRef?: Ref<HTMLDivElement>;
}

interface HeaderSelectProps {
  label: string;
  value: string;
  disabled?: boolean;
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
  onChange: (value: string) => void;
  children: ReactNode;
  maxWidth: string;
}

function HeaderSelect({ label, value, disabled = false, open, onOpenChange, onChange, children, maxWidth }: HeaderSelectProps): React.JSX.Element {
  const controlled = open === undefined || onOpenChange === undefined ? {} : { open, onOpen: () => onOpenChange(true), onClose: () => onOpenChange(false) };
  return (
    <Select
      variant="standard"
      disableUnderline
      size="small"
      value={value}
      disabled={disabled}
      displayEmpty
      onChange={(event) => onChange(String(event.target.value))}
      IconComponent={ExpandMoreIcon}
      inputProps={{ 'aria-label': label }}
      {...controlled}
      sx={(theme: Theme) => ({
        maxWidth,
        borderRadius: theme.vars.shape.radiusSm,
        paddingLeft: 1,
        ...theme.typography.labelSmall,
        flexShrink: 0,
        color: theme.vars.palette.text.secondary,
        '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
      })}
    >
      {children}
    </Select>
  );
}

function Separator(): React.JSX.Element {
  return (
    <Typography aria-hidden variant="bodySmall" component="span" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
      /
    </Typography>
  );
}

export function NoAgents({ onCreate, onOtherProject, busy }: { onCreate: () => void; onOtherProject: () => void; busy: boolean }): React.JSX.Element {
  return (
    <Box
      data-testid="workspace-no-agents"
      sx={(theme: Theme) => ({
        display: 'flex',
        flexDirection: 'column',
        gap: 1,
        padding: 2,
        border: `1px dashed ${theme.vars.palette.divider}`,
        borderRadius: theme.vars.shape.radiusMd,
      })}
    >
      <Typography variant="headingSmall">{t('workspace.noAgents.title', 'This project has no agents')}</Typography>
      <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary })}>
        {t(
          'workspace.noAgents.body',
          'A session runs one of the bound project’s agents. Create an agent in this project, or bind this folder to a project that has agents.',
        )}
      </Typography>
      <Box sx={{ display: 'flex', gap: 1, flexWrap: 'wrap' }}>
        <Button variant="contained" size="small" onClick={onCreate}>
          {t('workspace.noAgents.create', 'Create an agent')}
        </Button>
        <Button variant="outlined" size="small" disabled={busy} onClick={onOtherProject}>
          {t('workspace.noAgents.otherProject', 'Use another project')}
        </Button>
      </Box>
    </Box>
  );
}

export function AgentPickers({
  selection,
  projectId,
  projects,
  busy,
  onChangeProject,
  projectMenuOpen,
  onProjectMenuOpenChange,
  agentRef,
}: AgentPickersProps): React.JSX.Element {
  const listed = projects.some((p) => p.id === projectId);
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5, minWidth: 0 }}>
      <HeaderSelect
        label={t('workspace.project', 'Project')}
        value={listed ? String(projectId) : ''}
        disabled={busy}
        open={projectMenuOpen}
        onOpenChange={onProjectMenuOpenChange}
        maxWidth="12rem"
        onChange={(value) => {
          const next = Number(value);
          if (next !== projectId) onChangeProject(next);
        }}
      >
        {projects.map((project) => (
          <MenuItem key={project.id} value={String(project.id)}>
            {project.name}
          </MenuItem>
        ))}
      </HeaderSelect>
      {!selection.empty && (
        <>
          <Separator />
          <Box ref={agentRef} sx={{ display: 'contents' }}>
            <HeaderSelect
              label={t('workspace.agent', 'Agent')}
              value={selection.agents.some((a) => a.id === selection.agentId) ? selection.agentId : ''}
              maxWidth="14rem"
              onChange={(value) => selection.selectAgent(value)}
            >
              {selection.agents.map((agent) => (
                <MenuItem key={agent.id} value={agent.id}>
                  {agent.name}
                </MenuItem>
              ))}
            </HeaderSelect>
          </Box>
          <HeaderSelect
            label={t('workspace.version', 'Version')}
            value={selection.versions.some((v) => String(v.id) === selection.versionId) ? selection.versionId : ''}
            disabled={selection.versions.length === 0}
            maxWidth="9rem"
            onChange={(value) => selection.selectVersion(value)}
          >
            {selection.versions.map((version) => (
              <MenuItem key={version.id} value={String(version.id)}>
                {version.name}
              </MenuItem>
            ))}
          </HeaderSelect>
        </>
      )}
    </Box>
  );
}
