/**
 * LedgerSignIn — a fan-out sign-in card the run's own user authorizes
 * (FU-250). It is the existing reference-only sign-in (`useMcpLogin` +
 * `McpAuthModal`, as `ToolkitTestAuthorization` uses it): Main saves the
 * grant and answers with an opaque reference, which the card's decision
 * carries. The token never reaches the browser's storage or the decision.
 *
 * Authorize first asks Main for the toolkit's consent metadata by toolkit id
 * alone, then opens the dialog with it. Without that injected login,
 * `useMcpLogin` falls back to a socket check this platform does not answer,
 * and the button did nothing.
 */
import { useCallback, useMemo, useState } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';

import type { LedgerSignInSlotProps } from '@/features/chat-messages';
import { McpAuthModal, getAuthorizationReference, readMcpAuthorizationChallenge, useMcpLogin } from '@/features/mcps';
import { t } from '@/shared/i18n';

export function LedgerSignIn({ projectId, slot }: { readonly projectId: string | undefined; readonly slot: LedgerSignInSlotProps }) {
  const { toolkitId, toolkitType, serverUrl, message, disabled, onAuthorized, onSkip } = slot;
  const [missingReference, setMissingReference] = useState(false);
  const [refusal, setRefusal] = useState<string | undefined>();
  const [loading, setLoading] = useState(false);
  const values = useMemo(() => ({ id: String(toolkitId), type: toolkitType ?? 'mcp', settings: { url: serverUrl } }), [toolkitId, toolkitType, serverUrl]);
  const onSuccess = useCallback(() => {
    const reference = projectId ? getAuthorizationReference(projectId, String(toolkitId), serverUrl) : undefined;
    if (!reference) { setMissingReference(true); return; }
    setMissingReference(false);
    onAuthorized(reference);
  }, [projectId, toolkitId, serverUrl, onAuthorized]);
  const authConfig = useMemo(() => ({
    isRunning: loading,
    onLogin: (handle: (message: unknown) => void) => {
      if (!projectId) {
        setRefusal(t('mcps.authorizationChallenge.unavailable', 'Sign-in is not available right now. Try again, or Skip.'));
        return;
      }
      setRefusal(undefined);
      setLoading(true);
      void readMcpAuthorizationChallenge(projectId, toolkitId).then((outcome) => {
        setLoading(false);
        if (outcome.kind === 'refused') { setRefusal(outcome.message); return; }
        handle({ response_metadata: { ...outcome.challenge, toolkit_id: String(toolkitId), toolkit_type: values.type } });
      });
    },
  }), [projectId, toolkitId, values.type, loading]);
  const { onLogin, modalProps } = useMcpLogin({ projectId, values, onSuccess, authConfig });
  return (
    <Box data-testid="toolkit-authorization-panel" sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
      {message && <Typography variant="bodyMedium">{message}</Typography>}
      <Box sx={{ display: 'flex', gap: '0.5rem' }}>
        <Button variant="contained" disabled={disabled || loading} onClick={onLogin}>{t('mcps.authModal.authorize', 'Authorize')}</Button>
        <Button variant="outlined" disabled={disabled} onClick={onSkip}>{t('features.chatMessages.authorization.skip', 'Skip')}</Button>
      </Box>
      {refusal && (
        <Typography role="alert" variant="bodySmall">{refusal}</Typography>
      )}
      {missingReference && (
        <Typography role="alert" variant="bodySmall">
          {t('features.toolkits.testToolPane.missingReference', 'Authorization was not saved for this toolkit. Authorize again.')}
        </Typography>
      )}
      <McpAuthModal {...modalProps} authorizationReferenceOnly />
    </Box>
  );
}
