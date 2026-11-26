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
 *
 * The LLM gateway editor (`llm_proxy`) moved the same way under the six-area
 * navigation (R3.5a, issue 2535): its tabs are the pages of Models and spend,
 * and its row opens the first of them. With the `admin_console_areas` switch
 * off the editor still mounts in Configuration, so the section moves only
 * when the switch is known to be on.
 */
import { Link } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { useAdminConsole } from './api/adminConsoleApi';
import { IDENTITY_PROVIDERS_MANAGED_SURFACE } from './api/adminIdentityProvidersApi';
import { LLM_PROXY_MANAGED_SURFACE } from './api/adminLlmProxyApi';
import { MICROSOFT_APPS_MANAGED_SURFACE } from './api/adminMicrosoftAppsApi';
import { NATIVE_CLIENTS_MANAGED_SURFACE } from './api/adminNativeClientsApi';
import { ADMIN_ITEM } from './adminItemIds';
import { useAdminItemAccess } from './adminWriteAccess';

type MovedPath = '/identity-providers' | '/client-applications' | '/models/providers';

const MOVED_SURFACES: Readonly<Record<string, MovedPath>> = {
  [IDENTITY_PROVIDERS_MANAGED_SURFACE]: '/identity-providers',
  [NATIVE_CLIENTS_MANAGED_SURFACE]: '/client-applications',
  [MICROSOFT_APPS_MANAGED_SURFACE]: '/client-applications',
};

type Section = { readonly managed_surface?: string } | undefined;

/** The page a section moved to, when it moved; `areas` is the `admin_console_areas` switch. */
function movedPath(section: Section, areas: boolean | undefined): MovedPath | undefined {
  const surface = section?.managed_surface;
  if (surface === undefined) return undefined;
  if (surface === LLM_PROXY_MANAGED_SURFACE) return areas === true ? '/models/providers' : undefined;
  return MOVED_SURFACES[surface];
}

/** The page a section moved to, when it moved. */
export function useMovedSectionPath(): (section: Section) => MovedPath | undefined {
  const { areasEnabled } = useAdminConsole();
  return (section) => movedPath(section, areasEnabled);
}

const MOVED_ITEMS: Readonly<Record<MovedPath, string>> = {
  '/identity-providers': ADMIN_ITEM.identityProviders,
  '/client-applications': ADMIN_ITEM.clientApplications,
  '/models/providers': ADMIN_ITEM.providersModels,
};

/**
 * Whether the caller's document opens the page a section moved to. Unknown
 * (still loading, or the document failed) reads as open: the page itself
 * then answers, and a failure never reads as no access.
 */
export function useMovedSectionReach(): (path: MovedPath | undefined) => boolean {
  const identity = useAdminItemAccess(MOVED_ITEMS['/identity-providers']);
  const clients = useAdminItemAccess(MOVED_ITEMS['/client-applications']);
  const providers = useAdminItemAccess(MOVED_ITEMS['/models/providers']);
  return (path) => {
    if (path === '/identity-providers') return identity !== 'none';
    if (path === '/models/providers') return providers !== 'none';
    return clients !== 'none';
  };
}

function pageName(path: MovedPath): string {
  if (path === '/identity-providers') return t('pages.admin.nav.identityProviders', 'Identity providers');
  if (path === '/models/providers') return t('pages.admin.console.item.providers-models', 'Providers and models');
  return t('pages.admin.nav.clientApplications', 'Client applications');
}

/** The card a moved section shows in place of its editor. */
export function ConfigurationMovedSection({ path }: { readonly path: MovedPath }) {
  const page = pageName(path);
  const reaches = useMovedSectionReach();
  return (
    <Box
      data-testid="admin-configuration-moved"
      sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem', maxWidth: '36rem' }}
    >
      <Typography variant="bodyMedium" color="text.secondary">
        {t('pages.admin.configuration.moved.body', 'These settings have a page of their own: {{page}}.', { page })}
      </Typography>
      {reaches(path) ? null : (
        <Typography variant="bodySmall" color="text.secondary" data-testid="admin-configuration-moved-closed">
          {t('pages.admin.configuration.moved.closed', 'Your roles do not open {{page}}.', { page })}
        </Typography>
      )}
      <Box hidden={!reaches(path)}>
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
 * a moved section went to when the caller's roles open it, otherwise the
 * row's own selection (which shows the card).
 */
export function movedSectionRow(
  path: MovedPath | undefined,
  onSelect: () => void,
  reaches: (path: MovedPath | undefined) => boolean,
): { readonly component: typeof Link; readonly to: string } | { readonly onClick: () => void } {
  return path === undefined || !reaches(path) ? { onClick: onSelect } : { component: Link, to: path };
}
