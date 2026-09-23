import { useCallback, useMemo, useState } from 'react';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';

import { McpAuthModal, getAuthorizationReference, useMcpLogin } from '@/features/mcps';
import type { ToolkitTestAuthorizationRenderProps } from '@/features/toolkits';
import { t } from '@/shared/i18n';

/** The editor reuses its existing OAuth dialog. Retry carries only a server reference. */
export function ToolkitTestAuthorization({ projectId, challenge, onAuthorized, onSkip }: ToolkitTestAuthorizationRenderProps) {
  const [missingReference, setMissingReference] = useState(false);
  const values = useMemo(() => ({ id: String(challenge.toolkit_id), type: challenge.toolkit_type, settings: { url: challenge.server_url } }), [challenge]);
  const onSuccess = useCallback(() => {
    const reference = getAuthorizationReference(projectId, String(challenge.toolkit_id), challenge.server_url);
    if (!reference) { setMissingReference(true); return; }
    void onAuthorized(reference);
  }, [projectId, challenge, onAuthorized]);
  const authConfig = useMemo(() => ({ onLogin: (handle: (message: unknown) => void) => handle({ response_metadata: challenge }) }), [challenge]);
  const { onLogin, modalProps } = useMcpLogin({ projectId, values, onSuccess, authConfig });
  return <>
    <Button onClick={onLogin}>{t('mcps.authModal.authorize', 'Authorize')}</Button>
    <Button onClick={onSkip}>{t('features.toolkits.testToolPane.skip', 'Skip')}</Button>
    {missingReference && <Typography role="alert">{t('features.toolkits.testToolPane.missingReference', 'Authorization was not saved for this toolkit. Authorize again.')}</Typography>}
    <McpAuthModal {...modalProps} authorizationReferenceOnly onCancel={() => { modalProps.onCancel(); onSkip(); }} />
  </>;
}
