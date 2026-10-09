/**
 * The agent, version and conversation selectors of a workspace session. The
 * agent list is the bound project's (see `useAgentSelection`); the version
 * list follows the chosen agent.
 */
import Box from '@mui/material/Box';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';

import type { AgentSelection } from './useAgentSelection';

export function AgentPickers({ selection }: { selection: AgentSelection }): React.JSX.Element {
  return (
    <Box sx={{ display: 'flex', gap: 2, flexWrap: 'wrap' }}>
      <TextField
        select
        size="small"
        sx={{ minWidth: '14rem' }}
        label={t('workspace.agent', 'Agent')}
        value={selection.agentId}
        onChange={(event) => selection.selectAgent(event.target.value)}
      >
        {selection.agents.map((agent) => (
          <MenuItem key={agent.id} value={agent.id}>
            {agent.name}
          </MenuItem>
        ))}
      </TextField>
      <TextField
        select
        size="small"
        sx={{ minWidth: '10rem' }}
        label={t('workspace.version', 'Version')}
        value={selection.versionId}
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
    </Box>
  );
}
