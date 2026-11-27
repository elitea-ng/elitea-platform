/**
 * Where trust is offered besides the badge: the folder menu's item
 * ("Trust this folder…" or "Revoke trust"), the `sandbox_unavailable` tool
 * card's offer (only while the policy allows trust), and whether a folder
 * is trusted right now (the rules panel's "Needs trust", the Bypass line).
 */
import { Box, Button, Typography } from '@mui/material';
import { useState } from 'react';

import { t } from '@/shared/i18n';

import { trustOwner, useFolderTrust, useTrustActions } from '../model/useFolderTrust';
import { TrustDialog } from './TrustDialog';

export interface FolderTrustMenu {
  /** The folder menu's trust item; `null` outside the desktop host or for a task worktree. */
  item: { label: string; run: () => void } | null;
  /** The trust dialog, mounted while it is open. */
  dialog: React.ReactNode;
}

export function useFolderTrustMenu(workspaceId: string, workspaceName: string): FolderTrustMenu {
  const { ipc, status } = useFolderTrust(workspaceId);
  const { revoke } = useTrustActions(ipc);
  const [open, setOpen] = useState(false);
  if (status === undefined || status.inherited_from !== null) return { item: null, dialog: null };
  const item =
    status.state === 'trusted'
      ? { label: t('desktop.trust.revoke', 'Revoke trust'), run: () => revoke.mutate(trustOwner(status)) }
      : { label: t('desktop.trust.menu', 'Trust this folder…'), run: () => setOpen(true) };
  return { item, dialog: open ? <TrustDialog open workspaceName={workspaceName} status={status} onClose={() => setOpen(false)} /> : null };
}

/** `true` only while the folder (or a worktree's parent) is trusted and not paused. */
export function useFolderTrusted(workspaceId: string | undefined): boolean {
  return useFolderTrust(workspaceId).status?.state === 'trusted';
}

/** The folder carries the trust badge ("No sandbox", "Trust paused"): its trust is granted, running or paused (#3145). */
export function useFolderTrustBadged(workspaceId: string | undefined): boolean {
  const state = useFolderTrust(workspaceId).status?.state;
  return state !== undefined && state !== 'untrusted';
}

/** Under a command refused for want of a sandbox: "Trust this folder…", when the policy allows it. */
export function SandboxUnavailableOffer({ workspaceId, workspaceName }: { workspaceId: string; workspaceName: string }): React.JSX.Element | null {
  const { status } = useFolderTrust(workspaceId);
  const [open, setOpen] = useState(false);
  if (status === undefined || !status.allowed_by_policy || !status.grantable || status.state === 'trusted') return null;
  return (
    <Box data-testid="sandbox-unavailable-offer" sx={{ display: 'flex', alignItems: 'center', gap: 1, marginLeft: 3, marginTop: 0.5, flexWrap: 'wrap' }}>
      <Typography variant="bodySmall" color="text.secondary">
        {t('desktop.trust.offer', 'There is no sandbox here to run this command in.')}
      </Typography>
      <Button size="small" variant="outlined" onClick={() => setOpen(true)}>
        {t('desktop.trust.menu', 'Trust this folder…')}
      </Button>
      {open && <TrustDialog open workspaceName={workspaceName} status={status} onClose={() => setOpen(false)} />}
    </Box>
  );
}
