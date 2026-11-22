/**
 * pages/credentials/MicrosoftCredentialSignIn.tsx — the Microsoft sign-in block of the
 * credential form for Outlook, Teams, Microsoft Graph and Dataverse (ADR-0057, S1d).
 *
 * It composes the three pieces no single feature may import from another: the flow and
 * panel (`features/microsoft-login`), the store runs read the access token from
 * (`features/mcps`), and the form's own save and connection check.
 *
 *  - Login saves the credential first, then signs in, then stores the token under
 *    `{credential uuid}:{authority}` and runs the connection check with it.
 *  - Sign out forgets the token, so runs and the check stop using it.
 *  - A check Main answers with 401 `requires_authorization` while a token is held means
 *    the token was refused: it is dropped and the panel asks for a new sign-in.
 */
import type { ReactNode } from 'react';
import { useCallback, useEffect, useRef } from 'react';

import { forgetAccessToken, setAccessToken, useMcpTokenChange } from '@/features/mcps';
import { MicrosoftLoginPanel, isMicrosoftCredentialType, useMicrosoftLogin } from '@/features/microsoft-login';
import type { SignedIn } from '@/features/microsoft-login';

import type { useCredentialFormController } from './useCredentialFormController';

export interface MicrosoftCredentialSignInProps {
  readonly controller: ReturnType<typeof useCredentialFormController>;
  readonly projectId: string;
  /** The person may update credentials in this project; sign-in needs that permission. */
  readonly canUpdate: boolean;
}

function SignInBlock({ controller, projectId, canUpdate }: MicrosoftCredentialSignInProps): ReactNode {
  const { data, credentialUuid, saveForSignIn, testConnection, resetTest, signInRefused } = controller;
  // The check that follows a sign-in is the form's own; read through a ref so the hook's
  // callback stays stable while the form re-renders.
  const checkRef = useRef(testConnection);
  checkRef.current = testConnection;

  const onSignedIn = useCallback(
    ({ tokenKey, redeemed }: SignedIn) => {
      if (tokenKey !== '') {
        setAccessToken(tokenKey, redeemed.access_token, redeemed.expires_in, null, null, null, { project_id: projectId });
      }
      resetTest();
      checkRef.current();
    },
    [projectId, resetTest],
  );

  const login = useMicrosoftLogin({
    projectId: Number(projectId),
    credentialUuid,
    ensureSaved: saveForSignIn,
    credentialData: data,
    onSignedIn,
  });

  const tokenKey = login.session?.tokenKey === '' ? undefined : login.session?.tokenKey;
  const { isLoggedIn } = useMcpTokenChange(tokenKey);

  // A refused token is no use to a run either: drop it so nothing keeps sending it.
  useEffect(() => {
    if (signInRefused && tokenKey !== undefined) forgetAccessToken(tokenKey);
  }, [signInRefused, tokenKey]);

  const signOut = useCallback(() => {
    if (tokenKey !== undefined) forgetAccessToken(tokenKey);
    login.clear();
    resetTest();
  }, [tokenKey, login, resetTest]);

  return (
    <MicrosoftLoginPanel
      login={login}
      signedIn={isLoggedIn && !signInRefused}
      needsLogin={signInRefused}
      saved={credentialUuid !== undefined}
      canLogin={canUpdate}
      onSignOut={signOut}
    />
  );
}

/** Renders nothing for a credential type that does not sign in with Microsoft. A wrapper so the hooks above run for the four types only. */
export function MicrosoftCredentialSignIn(props: MicrosoftCredentialSignInProps): ReactNode {
  if (!isMicrosoftCredentialType(props.controller.effectiveType)) return null;
  return <SignInBlock {...props} />;
}
