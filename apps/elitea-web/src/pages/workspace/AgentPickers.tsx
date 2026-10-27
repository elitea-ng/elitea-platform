/**
 * `NoAgents`: what a project without agents shows in the thread body instead
 * of an agent picker (`ComposerControls`): create a Local coding agent in it
 * (one click, under the person's own permissions), open the agent editor
 * there, or bind the folder to another project (a menu of the projects it may
 * join). Nothing is created on the server unless the person clicks.
 */
import { useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { ProjectChoice } from './useBindableProjects';
import type { LocalCodingAgent } from './useLocalCodingAgent';

interface NoAgentsProps {
  /** The "Local coding agent" offer for the bound project. */
  codingAgent: LocalCodingAgent;
  /** Open the agent editor in the bound project. */
  onCreate: () => void;
  /** The projects the folder may move to (not the bound one), and the move. */
  otherProjects: readonly ProjectChoice[];
  onMoveTo: (projectId: number) => void;
  busy: boolean;
}

/** What the empty project's notice says, by what the person may do in it. */
function noAgentsBody(state: LocalCodingAgent['state']): string {
  switch (state) {
    case 'ready':
      return t(
        'workspace.noAgents.coding',
        'Create a Local coding agent? It explores before it edits, keeps its changes small, runs the tests and explains what it changed. It is an ordinary agent of this project: you can edit it on the web.',
      );
    case 'forbidden':
      return t(
        'workspace.noAgents.forbidden',
        'You cannot create agents in this project: someone with permission to create agents in it (a project admin) can. Or bind this folder to a project that has agents.',
      );
    case 'no_model':
      return t(
        'workspace.noAgents.noModel',
        'This project has no chat model to give a Local coding agent. Add a model to the project, create an agent yourself, or bind this folder to another project.',
      );
    case 'checking':
      return t(
        'workspace.noAgents.body',
        'A session runs one of the bound project’s agents. Create an agent in this project, or bind this folder to a project that has agents.',
      );
  }
}

export function NoAgents({ codingAgent, onCreate, otherProjects, onMoveTo, busy }: NoAgentsProps): React.JSX.Element {
  const { state } = codingAgent;
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
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
        {noAgentsBody(state)}
      </Typography>
      {codingAgent.error !== undefined && (
        <Typography variant="bodySmall" role="alert" sx={(theme: Theme) => ({ color: theme.vars.palette.error.main })}>
          {t('workspace.noAgents.createFailed', 'The agent was not created: {{message}}', { message: codingAgent.error })}
        </Typography>
      )}
      <Box sx={{ display: 'flex', gap: 1, flexWrap: 'wrap' }}>
        {state === 'ready' && (
          <Button variant="contained" size="small" disabled={busy || codingAgent.creating} onClick={codingAgent.create}>
            {t('workspace.noAgents.createCoding', 'Create a Local coding agent')}
          </Button>
        )}
        {state !== 'forbidden' && (
          <Button variant={state === 'ready' ? 'outlined' : 'contained'} size="small" onClick={onCreate}>
            {t('workspace.noAgents.create', 'Create an agent')}
          </Button>
        )}
        {otherProjects.length > 0 && (
          <Button variant="outlined" size="small" disabled={busy} aria-haspopup="menu" onClick={(event) => setAnchor(event.currentTarget)}>
            {t('workspace.noAgents.otherProject', 'Use another project')}
          </Button>
        )}
        <Menu anchorEl={anchor} open={anchor !== null} onClose={() => setAnchor(null)} slotProps={{ list: { dense: true } }}>
          {otherProjects.map((project) => (
            <MenuItem
              key={project.id}
              onClick={() => {
                setAnchor(null);
                onMoveTo(project.id);
              }}
            >
              {project.name}
            </MenuItem>
          ))}
        </Menu>
      </Box>
    </Box>
  );
}
