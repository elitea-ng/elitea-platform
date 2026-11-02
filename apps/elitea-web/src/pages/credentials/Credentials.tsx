/**
 * pages/credentials/Credentials.tsx — the `/credentials/:tab` route target.
 * Ported from `apps/elitea-ui/src/pages/Credentials/Credentials.jsx`.
 * Manifest ROUTE-022, COPY-466.
 *
 * The baseline's `tabs` array has exactly one entry
 * (`CredentialsTabs = ['all']`, `hooks/useConfigurations.js`) — every other
 * "tab" the domain has (`create-credential`, `:credential_uid`) is a
 * SEPARATE route, not a second entry in this tab strip. This port keeps
 * that single-tab reality: `tab` is accepted for future-proofing (a second
 * tab would be a route-level, not page-level, change) but does not branch
 * on it today.
 */
import type { ReactNode } from 'react';

import { CredentialsList } from './CredentialsList';

export interface CredentialsProps {
  readonly tab: string;
  readonly projectId: string;
  readonly onSelectCredential: (id: string) => void;
  readonly onCreateNew: () => void;
}

/** The page's name is the shell's title row; `CredentialsList` draws the toolbar, the grid and the Filters tab. */
export function Credentials({ projectId, onSelectCredential, onCreateNew }: CredentialsProps): ReactNode {
  return (
    <CredentialsList
      projectId={projectId}
      onSelectCredential={onSelectCredential}
      onCreateNew={onCreateNew}
    />
  );
}
