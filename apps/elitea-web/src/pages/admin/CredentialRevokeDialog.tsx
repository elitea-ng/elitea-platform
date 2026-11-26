/**
 * The revoke confirmation of Admin › Credential inventory (ADR-0044 decision
 * 5, #1501), in the one dangerous-action dialog (ADR-0059 R3.8b, issue 2614).
 * The reason the administrator gives is recorded on the security audit event,
 * and a refusal (the credential was already gone, or is flagged only) stays
 * in the dialog next to the row it was about.
 */
import type { ReactNode } from 'react';

import { credentialReviewErrorMessage } from '@/features/agent-lifecycle';
import type { CredentialInventoryCredential } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { AdminDangerousActionDialog } from './AdminDangerousActionDialog';
import { ADMIN_ITEM } from './adminItemIds';
import { useAdminWritable } from './adminWriteAccess';
import { useRevokeCredential } from './api/adminCredentialReviewApi';

export interface CredentialRevokeDialogProps {
  readonly row: CredentialInventoryCredential | undefined;
  readonly onClose: () => void;
}

export function CredentialRevokeDialog({ row, onClose }: CredentialRevokeDialogProps): ReactNode {
  const access = useAdminWritable(ADMIN_ITEM.credentialInventory);
  const revoke = access.guard(useRevokeCredential());
  if (row === undefined) return null;
  const close = (): void => {
    revoke.reset();
    onClose();
  };
  return (
    <AdminDangerousActionDialog
      open
      action="POST /api/v2/admin/credential_inventory/revoke"
      title={t('pages.admin.credentialInventory.revokeTitle', 'Revoke credential')}
      change={t(
        'pages.admin.credentialInventory.revokeBody',
        'Revoke {{name}}? Agents and tools that use it stop working with it. The revocation is audited.',
        { name: row.name },
      )}
      reach={{ count: undefined, unit: 'projects' }}
      target={row.name}
      confirmLabel={t('pages.admin.credentialInventory.revoke', 'Revoke')}
      pending={!access.writable || revoke.isPending}
      error={revoke.error === null ? undefined : credentialReviewErrorMessage(revoke.error)}
      onClose={close}
      onConfirm={(reason) =>
        revoke.mutate({ kind: row.kind, credentialId: row.credential_id, reason }, { onSuccess: close })
      }
    />
  );
}
