/**
 * TokenRow — sub-components for the personal tokens table.
 *
 * Combines ExpiryCell, DeleteTokenConfirm, and ActionsCell from the original
 * TokensTable into a cohesive module.
 */
import { memo, useCallback, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import SvgIcon from '@mui/material/SvgIcon';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';
import { tokenExpiryStatus } from '@/entities/token';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';
import type { PersonalAccessToken } from '@/entities/token';
import { OpenEyeIcon } from '@/shared/ui/icons/open-eye-icon';
import { AttentionIcon } from '@/shared/ui/icons/attention-icon';
import { RemoveIcon } from '@/shared/ui/icons/remove-icon';
import { SuccessIcon } from '@/shared/ui/icons/success-icon';
import { StatusChip } from '@/shared/ui/status-chip';
import { tokenRowStyles } from './TokenRow.styles';

/* ── expiry cell ───────────────────────────────────────────────────────── */

export const ExpiryCell = memo(function ExpiryCell({
  expires,
}: {
  expires: string | null;
}) {
  const now = Date.now();
  const status = tokenExpiryStatus(expires, now);

  const statusLabel = useMemo(() => {
    if (status === 'never') return t('entities.token.expiry.never', 'Never');
    if (status === 'safe') return t('entities.token.expiry.safe', 'Safe');
    if (status === 'warning') return t('entities.token.expiry.warning', 'Warning');
    return t('entities.token.expiry.expired', 'Expired');
  }, [status]);

  // The legacy ExpiryInDays: a success icon while the token is valid, an attention icon for the last 7 days,
  // a remove icon in the disabled colour once it has expired; the word muted only then.
  if (status === 'warning') {
    return (
      <StatusChip
        tone="warning"
        icon={<AttentionIcon />}
        label={statusLabel}
      />
    );
  }
  if (status === 'expired') {
    return (
      <StatusChip
        tone="neutral"
        icon={<RemoveIcon />}
        label={statusLabel}
        muted
      />
    );
  }
  return (
    <StatusChip
      tone="success"
      icon={<SuccessIcon />}
      label={statusLabel}
    />
  );
});

/* ── delete confirmation ───────────────────────────────────────────────── */

function DeleteTokenConfirm({
  open,
  tokenName,
  onClose,
  onConfirm,
  isLoading,
}: {
  open: boolean;
  tokenName: string;
  onClose: () => void;
  onConfirm: () => void;
  isLoading: boolean;
}) {
  const handleConfirmed = useCallback(() => {
    onClose();
    onConfirm();
  }, [onClose, onConfirm]);

  return (
    <DeleteEntityModal
      open={open}
      onClose={onClose}
      onConfirm={handleConfirmed}
      name={tokenName}
      confirming={isLoading}
      copy={{
        title: t('entities.token.delete.confirmTitle', 'Delete token'),
        textContent: t(
          'entities.token.delete.confirmText',
          'Are you sure you want to delete ',
        ),
        confirmText: t('entities.token.delete.confirm', 'Delete'),
      }}
    />
  );
}

/* ── actions cell ──────────────────────────────────────────────────────── */

export const ActionsCell = memo(function ActionsCell({
  token,
  onDelete,
  onPreview,
  showPreview,
}: {
  token: PersonalAccessToken;
  onDelete: (uuid: string) => Promise<void>;
  onPreview: (token: PersonalAccessToken) => void;
  showPreview: boolean;
}) {
  const [isDeleting, setIsDeleting] = useState(false);
  const [deleteModalOpen, setDeleteModalOpen] = useState(false);
  const s = tokenRowStyles.actions();

  const handleDeleteClick = useCallback(() => {
    setDeleteModalOpen(true);
  }, []);

  const handleDeleteConfirm = useCallback(async () => {
    setIsDeleting(true);
    try {
      await onDelete(token.uuid);
    } catch {
      // Error handled by react-query — no toast in shared/ui
    } finally {
      setIsDeleting(false);
      setDeleteModalOpen(false);
    }
  }, [onDelete, token.uuid]);

  const handlePreview = useCallback(() => {
    onPreview(token);
  }, [onPreview, token]);

  return (
    <>
      <Box sx={s.container}>
        {showPreview && (
          <Tooltip title={t('entities.token.table.previewTooltip', 'Preview settings')}>
            <IconButton size="small" onClick={handlePreview}>
              <SvgIcon
                component={OpenEyeIcon}
                inheritViewBox
                sx={{ width: '0.875rem', height: '0.875rem' }}
              />
            </IconButton>
          </Tooltip>
        )}
        <Tooltip title={t('entities.token.table.deleteTooltip', 'Delete token')}>
          <IconButton
            size="small"
            onClick={handleDeleteClick}
            disabled={isDeleting}
            sx={s.deleteButton}
          >
            <SvgIcon
              component={RemoveIcon}
              inheritViewBox
              sx={{ width: '0.875rem', height: '0.875rem' }}
            />
          </IconButton>
        </Tooltip>
      </Box>
      <DeleteTokenConfirm
        open={deleteModalOpen}
        tokenName={token.name}
        onClose={() => setDeleteModalOpen(false)}
        onConfirm={() => void handleDeleteConfirm()}
        isLoading={isDeleting}
      />
    </>
  );
});
