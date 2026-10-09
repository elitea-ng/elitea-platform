/**
 * The project, agent, version and conversation selectors of a workspace
 * session. The project is the folder's binding (changing it re-binds the
 * folder on the host); the agent list is the bound project's (see
 * `useAgentSelection`); the version list follows the chosen agent.
 *
 * A project without agents gets an empty state instead of empty selects:
 * create an agent in it, or bind the folder to another project. Nothing is
 * created on the server from here.
 */
import type { Ref } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import MenuItem from '@mui/material/MenuItem';
import type { Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
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
  onCreateAgent: () => void;
  /** The box holding the agent select (the composer's `/agent` focuses it). */
  agentRef?: Ref<HTMLDivElement>;
}

function ProjectPicker({
  projectId,
  projects,
  busy,
  open,
  onOpenChange,
  onChange,
}: {
  projectId: number;
  projects: readonly ProjectChoice[];
  busy: boolean;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onChange: (projectId: number) => void;
}): React.JSX.Element {
  const listed = projects.some((p) => p.id === projectId);
  return (
    <TextField
      select
      size="small"
      sx={{ minWidth: '12rem' }}
      label={t('workspace.project', 'Project')}
      value={listed ? projectId : ''}
      disabled={busy}
      onChange={(event) => {
        const next = Number(event.target.value);
        if (next !== projectId) onChange(next);
      }}
      slotProps={{ select: { open, onOpen: () => onOpenChange(true), onClose: () => onOpenChange(false) } }}
    >
      {projects.map((project) => (
        <MenuItem key={project.id} value={project.id}>
          {project.name}
        </MenuItem>
      ))}
    </TextField>
  );
}

function NoAgents({ onCreate, onOtherProject, busy }: { onCreate: () => void; onOtherProject: () => void; busy: boolean }): React.JSX.Element {
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
      <Typography variant="bodySmall" sx={{ color: (theme: Theme) => theme.vars.palette.text.secondary }}>
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

export function AgentPickers({ selection, projectId, projects, busy, onChangeProject, onCreateAgent, agentRef }: AgentPickersProps): React.JSX.Element {
  const [projectMenuOpen, setProjectMenuOpen] = useState(false);
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }}>
      <Box sx={{ display: 'flex', gap: 2, flexWrap: 'wrap' }}>
        <ProjectPicker
          projectId={projectId}
          projects={projects}
          busy={busy}
          open={projectMenuOpen}
          onOpenChange={setProjectMenuOpen}
          onChange={onChangeProject}
        />
        {!selection.empty && (
          <>
            <Box ref={agentRef} sx={{ display: 'contents' }}>
              <TextField
                select
                size="small"
                sx={{ minWidth: '14rem' }}
                label={t('workspace.agent', 'Agent')}
                value={selection.agents.some((a) => a.id === selection.agentId) ? selection.agentId : ''}
                onChange={(event) => selection.selectAgent(event.target.value)}
              >
                {selection.agents.map((agent) => (
                  <MenuItem key={agent.id} value={agent.id}>
                    {agent.name}
                  </MenuItem>
                ))}
              </TextField>
            </Box>
            <TextField
              select
              size="small"
              sx={{ minWidth: '10rem' }}
              label={t('workspace.version', 'Version')}
              value={selection.versions.some((v) => String(v.id) === selection.versionId) ? selection.versionId : ''}
              disabled={selection.versions.length === 0}
              onChange={(event) => selection.selectVersion(event.target.value)}
            >
              {selection.versions.map((version) => (
                <MenuItem key={version.id} value={String(version.id)}>
                  {version.name}
                </MenuItem>
              ))}
            </TextField>
            <TextField
              select
              size="small"
              sx={{ minWidth: '14rem' }}
              label={t('workspace.conversation', 'Conversation')}
              value={selection.conversationId}
              onChange={(event) => selection.selectConversation(event.target.value)}
            >
              <MenuItem value="">{t('workspace.newConversation', 'New conversation')}</MenuItem>
              {selection.conversations.map((conversation) => (
                <MenuItem key={String(conversation.id)} value={String(conversation.id)}>
                  {conversation.name !== '' ? conversation.name : String(conversation.id)}
                </MenuItem>
              ))}
            </TextField>
          </>
        )}
      </Box>
      {selection.empty && <NoAgents busy={busy} onCreate={onCreateAgent} onOtherProject={() => setProjectMenuOpen(true)} />}
    </Box>
  );
}
