/**
 * Port of
 * apps/elitea-ui/src/[fsd]/features/mcp/ui/modal/McpLogoutModal.jsx
 * (unit A5, manifest COPY-145).
 */
import type { ReactNode } from 'react';
import { useCallback, useMemo } from 'react';

import Link from '@mui/material/Link';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import { isPrebuildMcpType, logout } from '../lib/storage';

export interface McpLogoutModalProps {
  serverUrl?: string | undefined;
  toolkitType?: string | undefined;
  /** Human-readable credential name; never render a composite storage key as a URL. */
  authorizationLabel?: string | undefined;
  open: boolean;
  onClose?: ((success?: boolean) => void) | undefined;
  onConfirm?: (() => void) | undefined;
}

export function McpLogoutModal({ serverUrl, toolkitType, authorizationLabel, open, onClose, onConfirm }: McpLogoutModalProps): ReactNode {
  const isPrebuildMcp = useMemo(() => isPrebuildMcpType(toolkitType), [toolkitType]);
  const displayName = isPrebuildMcp ? toolkitType : serverUrl;

  const handleCancel = useCallback(() => onClose?.(), [onClose]);

  const handleLogout = useCallback(() => {
    if (isPrebuildMcp) {
      logout(undefined, toolkitType);
    } else if (serverUrl) {
      logout(serverUrl);
    } else {
      return;
    }
    onConfirm?.();
    onClose?.(true);
  }, [isPrebuildMcp, toolkitType, serverUrl, onClose, onConfirm]);

  return (
    <BaseModal
      open={open}
      onClose={handleCancel}
      title={authorizationLabel ? t('mcps.logoutModal.credentialTitle', 'Toolkit authorization') : t('mcps.logoutModal.title', 'MCP Authorization')}
      data-testid="mcp-logout-modal"
      content={
        <>
          <Typography
            variant="bodyMedium"
            component="div"
            sx={{ marginBottom: '1rem' }}
          >
            {t('mcps.logoutModal.clearGrant', 'Remove the saved authorization from this browser and its other tabs. Other credentials stay authorized.')}
          </Typography>
          <Typography
            variant="headingSmall"
            component="div"
            sx={{ color: 'text.secondary' }}
          >
            {authorizationLabel ? t('mcps.logoutModal.credentialLabel', 'Credential: ') : isPrebuildMcp ? t('mcps.logoutModal.toolkitLabel', 'Toolkit: ') : t('mcps.logoutModal.serverLabel', 'Server: ')}
            <Typography
              variant="bodyMedium"
              component="span"
            >
              {authorizationLabel || isPrebuildMcp ? (
                authorizationLabel ?? displayName
              ) : (
                <Link
                  href={serverUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                >
                  {serverUrl}
                </Link>
              )}
            </Typography>
          </Typography>
          <Typography
            variant="bodyMedium"
            component="div"
            sx={{ marginTop: '1.5rem' }}
          >
            {t('mcps.logoutModal.confirmPrompt', 'Are you sure to log out?')}
          </Typography>
        </>
      }
      actions={{
        cancelText: t('mcps.logoutModal.cancel', 'Cancel'),
        confirmText: t('mcps.logoutModal.confirm', 'Log out'),
        alarm: true,
      }}
      onConfirm={handleLogout}
    />
  );
}
