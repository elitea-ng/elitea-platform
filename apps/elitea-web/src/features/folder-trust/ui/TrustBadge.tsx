/**
 * The badge a trusted folder carries (ADR-0061 decision 5i): "No sandbox"
 * while its commands run outside the sandbox, "Trust paused" (with the
 * reason) while the organisation's policy or a file changed outside the app
 * holds it, and "Environment fallback" when the host could not read the
 * login-shell environment. An untrusted folder shows nothing. The `chip`
 * variant opens a popover with the details and Revoke; the `glyph` variant
 * (inside a sidebar row, which is itself a button) is text only, and the
 * row's menu carries the actions. A worktree shows its parent's state.
 */
import { Alert, Box, Button, Chip, Popover, Stack, Typography } from '@mui/material';
import type { Theme } from '@mui/material/styles';
import { useState } from 'react';

import type { TrustStatus } from '@/shared/desktop/trustIpc';
import { t } from '@/shared/i18n';

import { pausedReason, whenText } from '../model/trustCopy';
import { trustOwner, useFolderTrust, useTrustActions } from '../model/useFolderTrust';
import { TrustDialog } from './TrustDialog';

interface BadgeLook {
  label: string;
  /** The longer line: the pause's reason, the fallback's meaning. */
  detail: string;
  paused: boolean;
}

/** The badge for a status, or `null` when the folder is not trusted. */
function badgeLook(status: TrustStatus | undefined): BadgeLook | null {
  if (status === undefined || status.state === 'untrusted') return null;
  if (status.state === 'suspended') return { label: t('desktop.trust.badgePaused', 'Trust paused'), detail: pausedReason(status), paused: true };
  const noSandbox = t('desktop.trust.badge', 'No sandbox');
  if (!status.env_fallback) return { label: noSandbox, detail: t('desktop.trust.badgeDetail', 'Commands in this folder run as you, without a sandbox.'), paused: false };
  return {
    label: `${noSandbox} · ${t('desktop.trust.badgeEnvFallback', 'Environment fallback')}`,
    detail: t('desktop.trust.envFallbackDetail', 'Your login shell’s environment could not be read, so commands get a reduced one with your PATH.'),
    paused: false,
  };
}

export interface TrustBadgeProps {
  workspaceId: string;
  /** The folder's name, for the dialog that confirms trust again; absent: "this folder". */
  workspaceName?: string | undefined;
  variant?: 'chip' | 'glyph';
}

function TrustPopoverBody({ status, look, workspaceName, onDone }: { status: TrustStatus; look: BadgeLook; workspaceName: string; onDone: () => void }): React.JSX.Element {
  const { ipc } = useFolderTrust(status.workspace_id);
  const { revoke } = useTrustActions(ipc);
  const [confirming, setConfirming] = useState(false);
  return (
    <Stack spacing={1} sx={{ p: 2, maxWidth: '22rem' }} data-testid="trust-popover">
      <Typography variant="bodyMedium" sx={{ fontWeight: 600 }}>
        {look.label}
      </Typography>
      <Typography variant="bodySmall">{look.detail}</Typography>
      {status.inherited_from !== null && <Typography variant="bodySmall">{t('desktop.trust.followsParent', 'This task worktree follows the trust of its folder.')}</Typography>}
      <Typography variant="bodySmall" color="text.secondary">
        {t('desktop.trust.grantedAt', 'Trusted: {{when}}', { when: whenText(status.granted_at) })}
        <br />
        {t('desktop.trust.lastHostCommand', 'Last command without a sandbox: {{when}}', { when: whenText(status.last_host_command_at) })}
      </Typography>
      {revoke.error !== null && <Alert severity="error">{revoke.error.message}</Alert>}
      <Box sx={{ display: 'flex', gap: 1, justifyContent: 'flex-end' }}>
        {look.paused && status.allowed_by_policy && status.grantable && (
          <Button size="small" onClick={() => setConfirming(true)}>
            {t('desktop.trust.resume', 'Confirm again…')}
          </Button>
        )}
        <Button size="small" color="error" variant="outlined" disabled={revoke.isPending} onClick={() => revoke.mutate(trustOwner(status), { onSuccess: onDone })} data-testid="trust-revoke">
          {t('desktop.trust.revoke', 'Revoke trust')}
        </Button>
      </Box>
      {confirming && <TrustDialog open workspaceName={workspaceName} status={status} onClose={() => (setConfirming(false), onDone())} />}
    </Stack>
  );
}

const glyphSx = (theme: Theme) => ({
  ...theme.typography.labelSmall,
  color: theme.vars.palette.warning.main,
  marginRight: 0.5,
  whiteSpace: 'nowrap' as const,
});

export function TrustBadge({ workspaceId, workspaceName, variant = 'chip' }: TrustBadgeProps): React.JSX.Element | null {
  const { status } = useFolderTrust(workspaceId);
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const look = badgeLook(status);
  if (look === null || status === undefined) return null;
  if (variant === 'glyph') {
    return (
      <Box component="span" data-testid="trust-badge" title={look.detail} sx={glyphSx}>
        {look.label}
      </Box>
    );
  }
  return (
    <>
      <Chip
        size="small"
        variant="outlined"
        color="warning"
        label={look.label}
        title={look.detail}
        data-testid="trust-badge"
        aria-haspopup="dialog"
        onClick={(event: React.MouseEvent<HTMLElement>) => setAnchor(event.currentTarget)}
      />
      <Popover open={anchor !== null} anchorEl={anchor} onClose={() => setAnchor(null)} anchorOrigin={{ vertical: 'bottom', horizontal: 'left' }}>
        <TrustPopoverBody status={status} look={look} workspaceName={workspaceName ?? t('desktop.trust.thisFolder', 'this folder')} onDone={() => setAnchor(null)} />
      </Popover>
    </>
  );
}
