/**
 * Settings › Trusted folders (ADR-0061 decision 5i): every folder trusted on
 * this computer (`trust_list`), with when it was trusted, the platform, its
 * state and the last command it ran without a sandbox, and Revoke. Revoking
 * takes effect at once: the folder's running commands are stopped and the
 * next one runs sandboxed.
 */
import { Alert, Button, CircularProgress, Stack, Table, TableBody, TableCell, TableHead, TableRow, Typography } from '@mui/material';
import { useQuery } from '@tanstack/react-query';

import type { TrustEntry } from '@/shared/desktop/trustIpc';
import { t } from '@/shared/i18n';

import { pausedReason, platformName, whenText } from '../model/trustCopy';
import { TRUST_LIST_KEY, useTrustActions, useTrustEvents, useTrustIpc } from '../model/useFolderTrust';

const stateText = (entry: TrustEntry): string =>
  entry.state === 'suspended' ? pausedReason(entry) : t('desktop.trust.badge', 'No sandbox');

export function TrustedFoldersList(): React.JSX.Element {
  const ipc = useTrustIpc();
  useTrustEvents(ipc);
  const { revoke } = useTrustActions(ipc);
  const list = useQuery({ queryKey: TRUST_LIST_KEY, queryFn: () => ipc?.list() ?? Promise.resolve([]), enabled: ipc !== undefined });
  const entries = (list.data ?? []).filter((entry) => entry.state !== 'untrusted' && entry.inherited_from === null);
  if (ipc === undefined) return <Typography>{t('desktop.trust.noHost', 'Trusted folders are kept by the desktop app.')}</Typography>;
  if (list.isPending) return <CircularProgress size={24} aria-label={t('desktop.trust.loading', 'Loading trusted folders')} />;
  return (
    <Stack spacing={2}>
      {list.error !== null && <Alert severity="error">{list.error.message}</Alert>}
      {revoke.error !== null && <Alert severity="error">{revoke.error.message}</Alert>}
      {entries.length === 0 ? (
        <Typography data-testid="trusted-folders-empty">{t('desktop.trust.none', 'No folder is trusted: every command runs in the sandbox, or is asked.')}</Typography>
      ) : (
        <Table size="small" data-testid="trusted-folders">
          <TableHead>
            <TableRow>
              <TableCell>{t('desktop.trust.colFolder', 'Folder')}</TableCell>
              <TableCell>{t('desktop.trust.colGranted', 'Trusted')}</TableCell>
              <TableCell>{t('desktop.trust.colPlatform', 'Platform')}</TableCell>
              <TableCell>{t('desktop.trust.colState', 'State')}</TableCell>
              <TableCell>{t('desktop.trust.colLast', 'Last command without a sandbox')}</TableCell>
              <TableCell />
            </TableRow>
          </TableHead>
          <TableBody>
            {entries.map((entry) => (
              <TableRow key={entry.workspace_id} data-testid={`trusted-folder-${entry.workspace_id}`}>
                <TableCell title={entry.path}>{entry.name}</TableCell>
                <TableCell>{whenText(entry.granted_at)}</TableCell>
                <TableCell>{platformName(entry.platform)}</TableCell>
                <TableCell>{stateText(entry)}</TableCell>
                <TableCell>{whenText(entry.last_host_command_at)}</TableCell>
                <TableCell align="right">
                  <Button size="small" color="error" disabled={revoke.isPending} onClick={() => revoke.mutate(entry.workspace_id)}>
                    {t('desktop.trust.revoke', 'Revoke trust')}
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Stack>
  );
}
