/**
 * The caps a send went over (segment, file, file count, unpacked tree) and
 * the largest paths, from `Preview.caps` or a `handoff_too_large` rejection.
 */
import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { CapsBreakdown } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';

import { formatBytes } from '../model/format';

function capName(cap: string): string {
  switch (cap) {
    case 'max_segment_bytes':
      return t('workspace.handoff.cap.segment', 'Size of what is sent');
    case 'max_file_bytes':
      return t('workspace.handoff.cap.file', 'Size of one file');
    case 'max_files':
      return t('workspace.handoff.cap.files', 'Number of files');
    case 'max_unpacked_bytes':
      return t('workspace.handoff.cap.unpacked', 'Size of the folder when unpacked');
    case 'project_workspace_bytes_left':
      return t('workspace.handoff.cap.quota', 'Room left in this project’s cloud storage');
    default:
      return cap;
  }
}

/** Counts are plain numbers; everything else a cap measures is bytes. */
const amount = (cap: string, value: number): string => (cap === 'max_files' ? value.toLocaleString() : formatBytes(value));

export function CapsList({ caps }: { caps: CapsBreakdown }): React.JSX.Element {
  return (
    <Box data-testid="handoff-caps" sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      <Box component="ul" sx={{ margin: 0, paddingLeft: 2.5 }}>
        {caps.violations.map((violation) => (
          <li key={`${violation.cap}:${violation.path ?? ''}`}>
            <Typography variant="bodySmall">
              {t('workspace.handoff.cap.over', '{{cap}}: {{actual}} (the limit is {{limit}})', {
                cap: capName(violation.cap),
                actual: amount(violation.cap, violation.actual),
                limit: amount(violation.cap, violation.limit),
              })}
              {violation.path === null ? '' : ` · ${violation.path}`}
            </Typography>
          </li>
        ))}
      </Box>
      {caps.largest.length > 0 && (
        <Box>
          <Typography variant="labelSmall" component="h4" sx={(theme: Theme) => ({ margin: 0, color: theme.vars.palette.text.secondary })}>
            {t('workspace.handoff.cap.largest', 'Largest files')}
          </Typography>
          <Box component="ul" sx={{ margin: 0, paddingLeft: 2.5 }}>
            {caps.largest.slice(0, 8).map((file) => (
              <li key={file.path}>
                <Typography variant="bodySmall">{`${file.path} · ${formatBytes(file.bytes)}`}</Typography>
              </li>
            ))}
          </Box>
        </Box>
      )}
    </Box>
  );
}
