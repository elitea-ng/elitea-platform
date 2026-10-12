/**
 * The agent asked to do something that needs the user's say-so (run a command,
 * touch paths). One request at a time; the rest wait in the turn's queue.
 *
 * Deny is the focused button and Escape denies: the safe answer is the one
 * the keyboard reaches without thought. The backdrop does not dismiss, since
 * an accidental click must not answer for the user.
 */
import { useEffect, useRef } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import type { ApprovalDecision, ApprovalRequestPayload } from '@/shared/desktop/workspaceIpc';

export interface ApprovalDialogProps {
  /** The request being asked; `undefined` closes the dialog. */
  request: ApprovalRequestPayload | undefined;
  /** How many more are waiting behind this one. */
  queued: number;
  onRespond: (requestId: string, decision: ApprovalDecision) => void;
}

const monoSx = (theme: Theme) => ({
  margin: 0,
  padding: 1,
  overflowX: 'auto',
  whiteSpace: 'pre-wrap',
  wordBreak: 'break-all',
  fontFamily: theme.typography.fontFamilyMono,
  border: `1px solid ${theme.vars.palette.divider}`,
  borderRadius: theme.vars.shape.radiusMd,
});

/** Shell-quote an argv element only when it needs it, so the line reads like what would be typed. */
function quoteArg(arg: string): string {
  return /^[\w@%+=:,./-]+$/.test(arg) ? arg : `'${arg.replaceAll("'", "'\\''")}'`;
}

export function ApprovalDialog({ request, queued, onRespond }: ApprovalDialogProps): React.JSX.Element | null {
  const denyRef = useRef<HTMLButtonElement>(null);
  const requestId = request?.request_id;
  // Deny takes focus each time a new request is shown (focus-trap order would otherwise start at the first button).
  useEffect(() => {
    if (requestId !== undefined) denyRef.current?.focus();
  }, [requestId]);
  if (request === undefined) return null;
  const respond = (decision: ApprovalDecision): void => onRespond(request.request_id, decision);
  return (
    <Dialog
      open
      maxWidth="sm"
      fullWidth
      aria-labelledby="workspace-approval-title"
      aria-describedby="workspace-approval-detail"
      // The focus trap claims focus after mount; Deny takes it once the dialog has finished opening.
      slotProps={{ transition: { onEntered: () => denyRef.current?.focus() } }}
      onClose={(_event, reason) => {
        if (reason === 'escapeKeyDown') respond('deny');
      }}
    >
      <DialogTitle id="workspace-approval-title">{request.title}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }}>
        <Typography id="workspace-approval-detail" variant="bodyMedium">
          {request.detail}
        </Typography>
        {request.command !== undefined && request.command.length > 0 && (
          <Box component="pre" data-testid="approval-command" aria-label={t('workspace.approval.command', 'Command')} sx={monoSx}>
            {request.command.map(quoteArg).join(' ')}
          </Box>
        )}
        {request.paths !== undefined && request.paths.length > 0 && (
          <Box component="ul" data-testid="approval-paths" aria-label={t('workspace.approval.paths', 'Paths')} sx={(theme: Theme) => ({ ...monoSx(theme), listStyle: 'none' })}>
            {request.paths.map((path) => (
              <li key={path}>{path}</li>
            ))}
          </Box>
        )}
        <Typography variant="bodySmall" sx={{ color: (theme: Theme) => theme.vars.palette.text.secondary }}>
          {t('workspace.approval.reason', 'Why: {{reason}}', { reason: request.reason })}
        </Typography>
        {queued > 0 && (
          <Typography variant="bodySmall" component="output">
            {t('workspace.approval.queued', '{{n}} more waiting', { n: queued })}
          </Typography>
        )}
      </DialogContent>
      <DialogActions>
        <Button ref={denyRef} color="error" onClick={() => respond('deny')}>
          {t('workspace.approval.deny', 'Deny')}
        </Button>
        <Button variant="outlined" onClick={() => respond('allow_once')}>
          {t('workspace.approval.allowOnce', 'Allow once')}
        </Button>
        {request.can_remember && (
          <Button variant="contained" onClick={() => respond('allow_always')}>
            {t('workspace.approval.allowAlways', 'Always allow')}
          </Button>
        )}
      </DialogActions>
    </Dialog>
  );
}
