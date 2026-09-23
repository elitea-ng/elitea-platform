/**
 * ui/CredentialMismatchFooter.tsx — rendered under `CredentialsSelect` when
 * the current value's `elitea_title` matches nothing in the loaded options.
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/credentials/ui/credentials-select/CredentialMismatchFooter.jsx`.
 * Manifest COPY-111.
 */
import type { ReactNode } from 'react';

import FormControl from '@mui/material/FormControl';
import FormHelperText from '@mui/material/FormHelperText';

import { t } from '@/shared/i18n';

import { CredentialWarningBanner } from '@/entities/credential';

export interface CredentialMismatchFooterProps {
  readonly mismatchedPrivateCredential: boolean;
  readonly credentialId?: string | null;
  readonly credentialType?: string;
  readonly section?: string;
  /** Forwarded to `CredentialWarningBanner` — see that component's doc comment for why this is caller-supplied. Only read when `mismatchedPrivateCredential` is true. */
  readonly createHref?: string;
}

export function CredentialMismatchFooter({
  mismatchedPrivateCredential,
  credentialId,
  credentialType,
  section,
  createHref,
}: CredentialMismatchFooterProps): ReactNode {
  if (mismatchedPrivateCredential) {
    return (
      <CredentialWarningBanner
        {...(credentialId !== undefined && credentialId !== null ? { credentialId } : {})}
        {...(credentialType !== undefined ? { credentialType } : {})}
        {...(section !== undefined ? { section } : {})}
        createHref={createHref ?? ''}
      />
    );
  }

  return (
    <FormControl
      error
      fullWidth
    >
      <FormHelperText>
        {t('credentials.mismatchFooter.text', 'Your configuration does not match any available configurations.')}
      </FormHelperText>
    </FormControl>
  );
}
