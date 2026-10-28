/**
 * Settings › MCP servers (desktop build only, #1230): pick a folder, then
 * its servers and your own (every workspace) in the MCP panel.
 */
import { MenuItem, Stack, TextField, Typography } from '@mui/material';
import { useQuery } from '@tanstack/react-query';
import { useMemo, useState } from 'react';

import { useWorkspaceIpc } from '@/features/workspace';
import { McpPanel } from '@/features/workspace-mcp';
import { tauriMcpIpc, type McpIpc } from '@/shared/desktop/mcpIpc';
import { openedFolders } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

export default function McpPage({ ipc }: { ipc?: McpIpc }): React.JSX.Element {
  const mcp = useMemo(() => ipc ?? tauriMcpIpc(), [ipc]);
  const workspaceIpc = useWorkspaceIpc();
  const list = useQuery({ queryKey: ['workspace', 'list'], queryFn: () => workspaceIpc?.list() ?? Promise.resolve([]), enabled: workspaceIpc !== undefined });
  const [picked, setPicked] = useState<string | null>(null);
  const workspaces = openedFolders(list.data ?? []);
  const workspaceId = picked ?? workspaces[0]?.id ?? null;
  return (
    <Stack spacing={2} sx={{ p: 3, maxWidth: 860 }}>
      <Typography component="h1" variant="headingMedium">
        {t('workspace.mcp.pageTitle', 'MCP servers')}
      </Typography>
      {mcp === undefined || workspaceIpc === undefined ? (
        <Typography>{t('workspace.mcp.noHost', 'Local MCP servers are run by the desktop app.')}</Typography>
      ) : workspaces.length === 0 ? (
        <Typography>{t('workspace.mcp.noWorkspaces', 'Open a folder first: servers run in a workspace’s session, and yours apply to every one.')}</Typography>
      ) : (
        <>
          <TextField select size="small" label={t('workspace.mcp.folder', 'Folder')} value={workspaceId ?? ''} onChange={(event) => setPicked(event.target.value)} sx={{ maxWidth: 420 }}>
            {workspaces.map((workspace) => (
              <MenuItem key={workspace.id} value={workspace.id}>
                {workspace.name}
              </MenuItem>
            ))}
          </TextField>
          {workspaceId !== null && <McpPanel key={workspaceId} ipc={mcp} workspaceId={workspaceId} />}
        </>
      )}
    </Stack>
  );
}
