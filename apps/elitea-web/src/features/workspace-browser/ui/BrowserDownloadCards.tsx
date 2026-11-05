/**
 * The thread's browser downloads as transcript cards (ADR-0048 decision 2):
 * each file the pane saved to `~/Downloads/Elitea/<thread>/`, its state,
 * and Show in Finder once it is there.
 */
import DownloadOutlinedIcon from '@mui/icons-material/DownloadOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { useBrowserPane, type BrowserDownload } from '../model/browserPane.store';
import { useBrowserHost } from '../model/ipcContext';

function sizeLabel(size: number | null): string {
  if (size === null) return '';
  if (size < 1024) return `${String(size)} B`;
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`;
  return `${(size / (1024 * 1024)).toFixed(1)} MB`;
}

function stateLabel(download: BrowserDownload): string {
  switch (download.state) {
    case 'running':
      return t('workspace.browser.downloadRunning', 'Downloading…');
    case 'done':
      return sizeLabel(download.size);
    case 'failed':
      return t('workspace.browser.downloadFailed', 'Download failed');
  }
}

export function BrowserDownloadCards({ threadId }: { readonly threadId: string }): React.JSX.Element | null {
  const { downloads } = useBrowserPane(threadId);
  const { ipc } = useBrowserHost();
  if (ipc === undefined || threadId === '' || downloads.length === 0) return null;
  return (
    <Box data-testid="browser-downloads" sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      {downloads.map((download) => (
        <Box
          key={download.name}
          data-testid="browser-download-card"
          sx={(theme: Theme) => ({ display: 'flex', alignItems: 'center', gap: 1, paddingX: 1.5, paddingY: 1, borderRadius: '8px', border: `1px solid ${theme.vars.palette.divider}` })}
        >
          <DownloadOutlinedIcon fontSize="small" aria-hidden />
          <Box sx={{ flex: 1, minWidth: 0 }}>
            <Typography variant="bodySmall" noWrap title={download.name}>
              {t('workspace.browser.downloaded', 'Browser download: {{name}}', { name: download.name })}
            </Typography>
            <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
              {stateLabel(download)}
            </Typography>
          </Box>
          {download.state === 'done' && (
            <Button size="small" onClick={() => void ipc.revealDownload(threadId, download.name).catch(() => undefined)} data-testid="browser-download-reveal">
              {t('workspace.browser.revealDownload', 'Show in Finder')}
            </Button>
          )}
        </Box>
      ))}
    </Box>
  );
}
