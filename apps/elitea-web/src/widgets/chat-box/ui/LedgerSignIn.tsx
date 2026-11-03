/**
 * LedgerSignIn — a fan-out sign-in card the run's own user authorizes
 * (FU-250). It is the existing reference-only sign-in (`useMcpLogin` +
 * `McpAuthModal`, as `ToolkitTestAuthorization` uses it): Main saves the
 * grant and answers with an opaque reference, which the card's decision
 * carries. The token never reaches the browser's storage or the decision.
 */
import { useCallback, useMemo, useState } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';

import type { LedgerSignInSlotProps } from '@/features/chat-messages';
import { McpAuthModal, getAuthorizationReference, useMcpLogin } from '@/features/mcps';
import { t } from '@/shared/i18n';

export function LedgerSignIn({ projectId, slot }: { readonly projectId: string | undefined; readonly slot: LedgerSignInSlotProps }) {
  const { toolkitId, toolkitType, serverUrl, message, disabled, onAuthorized, onSkip } = slot;
  const [missingReference, setMissingReference] = useState(false);
  const values = useMemo(() => ({ id: String(toolkitId), type: toolkitType ?? 'mcp', settings: { url: serverUrl } }), [toolkitId, toolkitType, serverUrl]);
  const onSuccess = useCallback(() => {
    const reference = projectId ? getAuthorizationReference(projectId, String(toolkitId), serverUrl) : undefined;
    if (!reference) { setMissingReference(true); return; }
    setMissingReference(false);
    onAuthorized(reference);
  }, [projectId, toolkitId, serverUrl, onAuthorized]);
  const { onLogin, modalProps } = useMcpLogin({ projectId, values, onSuccess });
  return (
    <Box data-testid="toolkit-authorization-panel" sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
      {message && <Typography variant="bodyMedium">{message}</Typography>}
      <Box sx={{ display: 'flex', gap: '0.5rem' }}>
        <Button variant="contained" disabled={disabled} onClick={onLogin}>{t('mcps.authModal.authorize', 'Authorize')}</Button>
        <Button variant="outlined" disabled={disabled} onClick={onSkip}>{t('features.chatMessages.authorization.skip', 'Skip')}</Button>
      </Box>
      {missingReference && (
        <Typography role="alert" variant="bodySmall">
          {t('features.toolkits.testToolPane.missingReference', 'Authorization was not saved for this toolkit. Authorize again.')}
        </Typography>
      )}
      <McpAuthModal {...modalProps} authorizationReferenceOnly />
    </Box>
  );
}
