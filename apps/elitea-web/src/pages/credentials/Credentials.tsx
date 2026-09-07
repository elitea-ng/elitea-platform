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

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { PageHeader } from '@/widgets/page-header';

import { CredentialsList } from './CredentialsList';

export interface CredentialsProps {
  readonly tab: string;
  readonly projectId: string;
  readonly onSelectCredential: (id: string) => void;
  readonly onCreateNew: () => void;
}

export function Credentials({ projectId, onSelectCredential, onCreateNew }: CredentialsProps): ReactNode {
  return (
    <Box sx={containerSx}>
      <PageHeader
        title={t('credentials.page.title', 'Credentials')}
        titleComponent="h1"
        showBorder={false}
        sx={headerSx}
      />
      <CredentialsList
        projectId={projectId}
        onSelectCredential={onSelectCredential}
        onCreateNew={onCreateNew}
      />
    </Box>
  );
}

const containerSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', flexDirection: 'column', gap: theme.spacing(2), padding: theme.spacing(3) });
/** The page already pads its content column, so the header keeps no padding of its own. */
const headerSx: SxProps<Theme> = { padding: 0 };
