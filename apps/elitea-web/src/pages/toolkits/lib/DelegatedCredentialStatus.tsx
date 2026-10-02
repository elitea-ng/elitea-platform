import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { McpLogoutButton, useMcpTokenChange } from '@/features/mcps';
import { t } from '@/shared/i18n';

import type { CredentialPickerRow } from './useCredentialRows';

/** Match the runtime's credential UUID plus discovery endpoint. Never use a row id or title. */
export function delegatedCredentialKey(credential: CredentialPickerRow): string | undefined {
  const endpoint = credential.data['oauth_discovery_endpoint'];
  if (!credential.uuid || typeof endpoint !== 'string' || endpoint.trim() === '') return undefined;
  return `${credential.uuid}:${endpoint}`;
}

/** Shared by every credential field. Toolkit names and transport types do not determine authorization. */
export function DelegatedCredentialStatus({ credential }: { readonly credential: CredentialPickerRow }): ReactNode {
  const tokenKey = delegatedCredentialKey(credential);
  const { hasStoredAuthorization } = useMcpTokenChange(tokenKey);
  if (!tokenKey) return null;
  return (
    <Box data-testid="delegated-credential-status" sx={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', mt: 1 }}>
      <Typography variant="bodySmall">
        {hasStoredAuthorization
          ? t('pages.toolkits.oauth.savedAuthorization', 'Saved authorization')
          : t('pages.toolkits.oauth.authorizationOnUse', 'Authorize this credential when you use the toolkit.')}
      </Typography>
      {/* Keep logout available even without a local grant: another tab can still hold one. */}
      <McpLogoutButton serverUrl={tokenKey} authorizationLabel={credential.displayLabel} />
    </Box>
  );
}
