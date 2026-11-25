/**
 * The Configuration sections whose editor moved to a page of its own
 * (ADR-0059 decision 6, "the four identity and client editors leave the
 * Configuration page"; R3.6, issue 2537).
 *
 * The server still declares the `identity_providers`, `native_clients` and
 * `microsoft_apps` managed surfaces until R4.9 (issue 2623), so the sections
 * keep their row in the Configuration list. The row is a link to the page,
 * and a section opened by default shows a card that points there: one editor
 * per surface, in one place.
 */
import { Link } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { IDENTITY_PROVIDERS_MANAGED_SURFACE } from './api/adminIdentityProvidersApi';
import { MICROSOFT_APPS_MANAGED_SURFACE } from './api/adminMicrosoftAppsApi';
import { NATIVE_CLIENTS_MANAGED_SURFACE } from './api/adminNativeClientsApi';

type MovedPath = '/identity-providers' | '/client-applications';

const MOVED_SURFACES: Readonly<Record<string, MovedPath>> = {
  [IDENTITY_PROVIDERS_MANAGED_SURFACE]: '/identity-providers',
  [NATIVE_CLIENTS_MANAGED_SURFACE]: '/client-applications',
  [MICROSOFT_APPS_MANAGED_SURFACE]: '/client-applications',
};

/** The page a section moved to, when it moved. */
export function movedSectionPath(
  section: { readonly managed_surface?: string } | undefined,
): MovedPath | undefined {
  const surface = section?.managed_surface;
  return surface === undefined ? undefined : MOVED_SURFACES[surface];
}

function pageName(path: MovedPath): string {
  return path === '/identity-providers'
    ? t('pages.admin.nav.identityProviders', 'Identity providers')
    : t('pages.admin.nav.clientApplications', 'Client applications');
}

/** The card a moved section shows in place of its editor. */
export function ConfigurationMovedSection({ path }: { readonly path: MovedPath }) {
  const page = pageName(path);
  return (
    <Box
      data-testid="admin-configuration-moved"
      sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem', maxWidth: '36rem' }}
    >
      <Typography variant="bodyMedium" color="text.secondary">
        {t('pages.admin.configuration.moved.body', 'These settings have a page of their own: {{page}}.', { page })}
      </Typography>
      <Box>
        <Button
          component={Link}
          to={path}
          size="small"
          variant="elitea"
          color="primary"
          data-testid="admin-configuration-moved-link"
        >
          {t('pages.admin.configuration.moved.open', 'Open {{page}}', { page })}
        </Button>
      </Box>
    </Box>
  );
}


/**
 * The props of a section's row in the Configuration list: a link to the page
 * a moved section went to, otherwise the row's own selection.
 */
export function movedSectionRow(
  section: { readonly managed_surface?: string },
  onSelect: () => void,
): { readonly component: typeof Link; readonly to: string } | { readonly onClick: () => void } {
  const path = movedSectionPath(section);
  return path === undefined ? { onClick: onSelect } : { component: Link, to: path };
}
