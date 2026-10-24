/**
 * Settings › Hooks (desktop build only, #1237): pick a folder, then
 * its hooks and your own (every workspace) in the hooks panel.
 */
import { MenuItem, Stack, TextField, Typography } from '@mui/material';
import { useQuery } from '@tanstack/react-query';
import { useMemo, useState } from 'react';

import { useWorkspaceIpc } from '@/features/workspace';
import { HooksPanel } from '@/features/workspace-hooks';
import { tauriHooksIpc, type HooksIpc } from '@/shared/desktop/hooksIpc';
import { t } from '@/shared/i18n';

export default function HooksPage({ ipc }: { ipc?: HooksIpc }): React.JSX.Element {
  const hooks = useMemo(() => ipc ?? tauriHooksIpc(), [ipc]);
  const workspaceIpc = useWorkspaceIpc();
  const list = useQuery({ queryKey: ['workspace', 'list'], queryFn: () => workspaceIpc?.list() ?? Promise.resolve([]), enabled: workspaceIpc !== undefined });
  const [picked, setPicked] = useState<string | null>(null);
  const workspaces = list.data ?? [];
  const workspaceId = picked ?? workspaces[0]?.id ?? null;
  return (
    <Stack spacing={2} sx={{ p: 3, maxWidth: 860 }}>
      <Typography component="h1" variant="headingMedium">
        {t('workspace.hooks.pageTitle', 'Hooks')}
      </Typography>
      {hooks === undefined || workspaceIpc === undefined ? (
        <Typography>{t('workspace.hooks.noHost', 'Hooks are run by the desktop app.')}</Typography>
      ) : workspaces.length === 0 ? (
        <Typography>{t('workspace.hooks.noWorkspaces', 'Open a folder first: hooks run in a workspace’s session, and yours apply to every one.')}</Typography>
      ) : (
        <>
          <TextField select size="small" label={t('workspace.hooks.folder', 'Folder')} value={workspaceId ?? ''} onChange={(event) => setPicked(event.target.value)} sx={{ maxWidth: 420 }}>
            {workspaces.map((workspace) => (
              <MenuItem key={workspace.id} value={workspace.id}>
                {workspace.name}
              </MenuItem>
            ))}
          </TextField>
          {workspaceId !== null && <HooksPanel key={workspaceId} ipc={hooks} workspaceId={workspaceId} />}
        </>
      )}
    </Stack>
  );
}
