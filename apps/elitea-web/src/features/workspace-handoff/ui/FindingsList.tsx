/**
 * The secret findings that block a send: file, line and rule (never the
 * matched text), with one "Leave on my laptop" per file. There is no
 * override: a file is left behind or edited.
 */
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';

import type { SecretFinding } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';

export interface FindingsListProps {
  findings: readonly SecretFinding[];
  disabled: boolean;
  onLeave: (path: string) => void;
}

export function FindingsList({ findings, disabled, onLeave }: FindingsListProps): React.JSX.Element {
  const paths = [...new Set(findings.map((finding) => finding.path))];
  return (
    <Box data-testid="handoff-findings" sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      {paths.map((path) => (
        <Box key={path} sx={{ display: 'flex', alignItems: 'flex-start', gap: 1 }}>
          <Box sx={{ flex: 1, minWidth: 0 }}>
            <Typography variant="labelSmall" sx={{ wordBreak: 'break-all' }}>
              {path}
            </Typography>
            {findings
              .filter((finding) => finding.path === path)
              .map((finding) => (
                <Typography key={`${String(finding.line)}:${finding.rule}`} variant="bodySmall" component="div">
                  {`${path}:${String(finding.line)} · ${finding.rule}`}
                </Typography>
              ))}
          </Box>
          <Button size="small" disabled={disabled} onClick={() => onLeave(path)} aria-label={t('workspace.handoff.leaveNamed', 'Leave {{path}} on my laptop', { path })}>
            {t('workspace.handoff.leave', 'Leave on my laptop')}
          </Button>
        </Box>
      ))}
    </Box>
  );
}
