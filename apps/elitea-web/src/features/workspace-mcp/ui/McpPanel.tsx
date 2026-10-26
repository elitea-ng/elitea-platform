/**
 * The person's local MCP servers (#1230; IPC.md, "Local MCP servers"):
 * the servers of every workspace and of this one, each with its status in
 * the workspace's session (state, tools, the end of its stderr), Restart,
 * edit and remove; and the add form. A saved server starts with the next
 * turn that may offer its tools, under the sandbox; the organisation's
 * policy decides whether any runs.
 */
import { Alert, Box, Button, Chip, CircularProgress, Stack, Typography, type Theme } from '@mui/material';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';

import type { McpFileView, McpIpc, McpScope, McpServerConfig, McpServerStatus } from '@/shared/desktop/mcpIpc';
import { toWorkspaceIpcError } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import { EMPTY_FORM, formOf, type ServerForm } from '../model/serverForm';
import { mcpListKey } from './mcpQuery';
import { ServerFormFields } from './ServerFormFields';

export interface McpPanelProps {
  ipc: McpIpc;
  workspaceId: string;
}

function stateLabel(server: McpServerConfig, status: McpServerStatus | undefined): string {
  if (server.enabled === false) return t('workspace.mcp.disabled', 'Off');
  if (status === undefined) return t('workspace.mcp.state.idle', 'Starts with the next turn');
  switch (status.state) {
    case 'ready':
      return t('workspace.mcp.state.ready', 'Running');
    case 'starting':
      return t('workspace.mcp.state.starting', 'Starting');
    case 'failed':
      return t('workspace.mcp.state.failed', 'Did not start');
    case 'stopped':
      return t('workspace.mcp.state.stopped', 'Stopped');
    case 'idle':
      return t('workspace.mcp.state.idle', 'Starts with the next turn');
  }
}

/** What the session says of a running (or failed) server: its tools, why it failed, its output. */
function ServerStatusLines({ name, status, showLog }: { name: string; status: McpServerStatus; showLog: boolean }): React.JSX.Element {
  return (
    <>
      {status.tools.length > 0 && (
        <Typography variant="bodySmall" color="text.secondary">
          {t('workspace.mcp.tools', 'Tools: {{tools}}', { tools: status.tools.join(', ') })}
        </Typography>
      )}
      {status.error !== undefined && <Alert severity="error">{status.error}</Alert>}
      {showLog && (
        <Typography
          component="pre"
          variant="bodySmall"
          data-testid={`mcp-stderr-${name}`}
          sx={{ m: 0, p: 1, maxHeight: 200, overflow: 'auto', bgcolor: 'action.hover', fontFamily: 'monospace', whiteSpace: 'pre-wrap' }}
        >
          {status.stderr_tail === '' ? t('workspace.mcp.noOutput', 'No output.') : status.stderr_tail}
        </Typography>
      )}
    </>
  );
}

interface ServerRowProps {
  scope: McpScope;
  name: string;
  server: McpServerConfig;
  status: McpServerStatus | undefined;
  busy: boolean;
  onEdit: () => void;
  onRemove: () => void;
  onRestart: () => void;
}

function ServerRow({ scope, name, server, status, busy, onEdit, onRemove, onRestart }: ServerRowProps): React.JSX.Element {
  const [showLog, setShowLog] = useState(false);
  return (
    <Box
      component="li"
      data-testid={`mcp-server-${scope}-${name}`}
      sx={(theme: Theme) => ({ display: 'flex', flexDirection: 'column', gap: 0.5, p: 1.5, border: 1, borderColor: 'divider', borderRadius: theme.vars.shape.radiusSm })}
    >
      <Stack direction="row" spacing={1} sx={{ alignItems: 'center', flexWrap: 'wrap' }}>
        <Typography variant="bodyMedium" sx={{ fontWeight: 600 }}>
          {name}
        </Typography>
        <Chip size="small" label={stateLabel(server, status)} color={status?.state === 'failed' ? 'error' : 'default'} />
        {server.network === true && <Chip size="small" variant="outlined" label={t('workspace.mcp.network', 'Network')} />}
      </Stack>
      <Typography variant="bodySmall" color="text.secondary" sx={{ fontFamily: 'monospace', overflowWrap: 'anywhere' }}>
        {[server.command, ...(server.args ?? [])].join(' ')}
      </Typography>
      {status !== undefined && <ServerStatusLines name={name} status={status} showLog={showLog} />}
      <Stack direction="row" spacing={1}>
        {status !== undefined && (
          <>
            <Button size="small" onClick={() => setShowLog((open) => !open)}>
              {showLog ? t('workspace.mcp.hideLog', 'Hide its output') : t('workspace.mcp.showLog', 'Show its output')}
            </Button>
            <Button size="small" onClick={onRestart} disabled={busy}>
              {t('workspace.mcp.restart', 'Restart')}
            </Button>
          </>
        )}
        <Button size="small" onClick={onEdit} disabled={busy}>
          {t('workspace.mcp.edit', 'Edit')}
        </Button>
        <Button size="small" color="error" onClick={onRemove} disabled={busy}>
          {t('workspace.mcp.remove', 'Remove')}
        </Button>
      </Stack>
    </Box>
  );
}

/** The workspace's file first: its servers replace the person's of the same name there. */
const SCOPES: readonly McpScope[] = ['workspace', 'user'];

interface Editing {
  form: ServerForm;
  existing: boolean;
}

export function McpPanel({ ipc, workspaceId }: McpPanelProps): React.JSX.Element {
  const queryClient = useQueryClient();
  const key = mcpListKey(workspaceId);
  const list = useQuery({ queryKey: key, queryFn: () => ipc.list(workspaceId) });
  const [editing, setEditing] = useState<Editing | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const done = async (): Promise<void> => {
    setFailure(null);
    await queryClient.invalidateQueries({ queryKey: key });
  };
  const onError = (error: unknown): void => setFailure(toWorkspaceIpcError(error).message);
  const save = useMutation({
    mutationFn: ({ scope, name, server }: { scope: McpScope; name: string; server: McpServerConfig }) => ipc.save(scope, name, server, workspaceId),
    onSuccess: async () => {
      setEditing(null);
      await done();
    },
    onError,
  });
  const remove = useMutation({ mutationFn: ({ scope, name }: { scope: McpScope; name: string }) => ipc.remove(scope, name, workspaceId), onSuccess: done, onError });
  const restart = useMutation({ mutationFn: (name: string) => ipc.restart(workspaceId, name), onSuccess: done, onError });
  const busy = save.isPending || remove.isPending || restart.isPending;

  if (list.data === undefined) {
    return list.error === null ? <CircularProgress size={24} aria-label={t('workspace.mcp.loading', 'Loading MCP servers')} /> : <Alert severity="error">{toWorkspaceIpcError(list.error).message}</Alert>;
  }
  const view = list.data;
  const statusOf = (scope: McpScope, name: string): McpServerStatus | undefined => view.status.find((status) => status.name === name && status.scope === scope);
  const section = (scope: McpScope, file: McpFileView | undefined): React.JSX.Element | null =>
    file === undefined ? null : (
      <Stack spacing={1} component="section" key={scope} aria-label={scope === 'user' ? t('workspace.mcp.scopeUser', 'Every workspace') : t('workspace.mcp.scopeWorkspace', 'This workspace')}>
        <Typography variant="labelMedium">{scope === 'user' ? t('workspace.mcp.scopeUser', 'Every workspace') : t('workspace.mcp.scopeWorkspace', 'This workspace')}</Typography>
        {file.error !== undefined && <Alert severity="error">{t('workspace.mcp.fileRefused', 'These settings are not used: {{message}}', { message: file.error })}</Alert>}
        {Object.keys(file.servers).length === 0 ? (
          <Typography variant="bodySmall" color="text.secondary">
            {t('workspace.mcp.none', 'No servers.')}
          </Typography>
        ) : (
          <Stack component="ul" spacing={1} sx={{ listStyle: 'none', p: 0, m: 0 }}>
            {Object.entries(file.servers).map(([name, server]) => (
              <ServerRow
                key={name}
                scope={scope}
                name={name}
                server={server}
                status={statusOf(scope, name)}
                busy={busy}
                onEdit={() => setEditing({ form: formOf(name, scope, server), existing: true })}
                onRemove={() => remove.mutate({ scope, name })}
                onRestart={() => restart.mutate(name)}
              />
            ))}
          </Stack>
        )}
      </Stack>
    );
  return (
    <Stack spacing={3}>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'workspace.mcp.help',
          'Your own MCP servers run on this computer, each under the same sandbox as the agent’s commands: they cannot read your sign-in or other folders’ data, and have no network unless you allow it. Their tools are asked the first time they are used. A repository’s .mcp.json is only a proposal: nothing runs from it unless you add it.',
        )}
      </Typography>
      {!view.allowed && (
        <Alert severity="info" data-testid="mcp-not-allowed">
          {t('workspace.mcp.notAllowed', 'Your organisation’s policy does not allow local MCP servers, so none of these run.')}
        </Alert>
      )}
      {failure !== null && <Alert severity="error">{failure}</Alert>}
      {SCOPES.map((scope) => section(scope, scope === 'user' ? view.user : view.workspace))}
      {editing === null ? (
        <Box>
          <Button size="small" variant="outlined" onClick={() => setEditing({ form: EMPTY_FORM, existing: false })}>
            {t('workspace.mcp.add', 'Add a server')}
          </Button>
        </Box>
      ) : (
        <ServerFormFields initial={editing.form} editing={editing.existing} busy={busy} onSave={(scope, name, server) => save.mutate({ scope, name, server })} onCancel={() => setEditing(null)} />
      )}
    </Stack>
  );
}
