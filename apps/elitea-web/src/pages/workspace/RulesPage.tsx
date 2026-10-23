/**
 * Settings › Approval rules (desktop build only, #1216): pick a folder, then
 * its rules and your own (every workspace) in the rules panel.
 */
import { MenuItem, Stack, TextField, Typography } from '@mui/material';
import { useQuery } from '@tanstack/react-query';
import { useMemo, useState } from 'react';

import { useWorkspaceIpc } from '@/features/workspace';
import { RulesPanel } from '@/features/workspace-rules';
import { tauriRulesIpc, type RulesIpc } from '@/shared/desktop/rulesIpc';
import { t } from '@/shared/i18n';

export default function RulesPage({ ipc }: { ipc?: RulesIpc }): React.JSX.Element {
  const rules = useMemo(() => ipc ?? tauriRulesIpc(), [ipc]);
  const workspaceIpc = useWorkspaceIpc();
  const list = useQuery({ queryKey: ['workspace', 'list'], queryFn: () => workspaceIpc?.list() ?? Promise.resolve([]), enabled: workspaceIpc !== undefined });
  const [picked, setPicked] = useState<string | null>(null);
  const workspaces = list.data ?? [];
  const workspaceId = picked ?? workspaces[0]?.id ?? null;
  return (
    <Stack spacing={2} sx={{ p: 3, maxWidth: 860 }}>
      <Typography component="h1" variant="headingMedium">
        {t('workspace.rules.pageTitle', 'Approval rules')}
      </Typography>
      {rules === undefined || workspaceIpc === undefined ? (
        <Typography>{t('workspace.rules.noHost', 'Approval rules are kept by the desktop app.')}</Typography>
      ) : workspaces.length === 0 ? (
        <Typography>{t('workspace.rules.noWorkspaces', 'Open a folder first: rules belong to a workspace, and yours apply to every one.')}</Typography>
      ) : (
        <>
          <TextField select size="small" label={t('workspace.rules.folder', 'Folder')} value={workspaceId ?? ''} onChange={(event) => setPicked(event.target.value)} sx={{ maxWidth: 420 }}>
            {workspaces.map((workspace) => (
              <MenuItem key={workspace.id} value={workspace.id}>
                {workspace.name}
              </MenuItem>
            ))}
          </TextField>
          {workspaceId !== null && <RulesPanel ipc={rules} workspaceId={workspaceId} />}
        </>
      )}
    </Stack>
  );
}
