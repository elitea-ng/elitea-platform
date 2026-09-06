/**
 * pages/credentials/EditCredential.tsx — the `/credentials/:tab/:credential_uid`
 * route target (also reused, via `mode.configurationMode`, for
 * `/settings/edit-configuration/:credential_uid`). Ported from
 * `apps/elitea-ui/src/pages/Credentials/EditCredential.jsx`. Manifest
 * ROUTE-025, ROUTE-065.
 *
 * Thin route-target wrapper: all real logic lives in `CredentialForm.tsx`.
 */
import type { ReactNode } from 'react';

import type { CredentialFormContext } from './CredentialForm';
import { CredentialForm } from './CredentialForm';

export interface EditCredentialProps {
  readonly context: CredentialFormContext;
  readonly credentialUid: string;
  readonly configurationMode?: boolean;
  /** See `useCredentialFormController.ts`'s `onSaved` doc comment. */
  readonly onSaved: (savedId?: string) => void;
  readonly onDiscarded: () => void;
}

export function EditCredential({ context, credentialUid, configurationMode = false, onSaved, onDiscarded }: EditCredentialProps): ReactNode {
  return (
    <CredentialForm
      context={context}
      mode={{ kind: 'edit', configId: credentialUid, configurationMode }}
      onSaved={onSaved}
      onDiscarded={onDiscarded}
    />
  );
}
