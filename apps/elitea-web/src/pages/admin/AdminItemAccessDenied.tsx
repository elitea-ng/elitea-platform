/**
 * The access-denied page for one console item (ADR-0059 decision 6, the
 * state table; R3.2, issue 2532): what a typed URL shows when the capability
 * document does not open the item it names.
 *
 * It says three things: the item, the roles that grant it, and whom to ask.
 *
 *  - The roles come from `GET /admin/console/items/{item_id}` (R3.1a), the
 *    server's own answer, never a client-side map. They are named by role,
 *    never by person.
 *  - Whom to ask is the deployment's IT support line (ADR-0052,
 *    `client_policy.support_contact` of the discovery document), shown as
 *    written with at most one link made from it; without one, a fixed text.
 *
 * Any signed-in user can open `/admin`, and a list of administrators by name
 * is a target list for phishing, so this page names no administrator for any
 * caller. Hiding is not enforcement: the server refuses the item's routes on
 * its own account, this page only says so before the first refusal.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Link from '@mui/material/Link';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { normalizeSupportContact, supportContactLink } from '@elitea/client-core/support';

import { useGetAdminConsoleItem } from '@/shared/api/generated/admin/admin';
import { useGetClientDiscovery } from '@/shared/api/generated/discovery/discovery';
import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

export interface AdminItemAccessDeniedProps {
  /** The server's item id. */
  readonly itemId: string;
  /** The item's label, as the navigation shows it. */
  readonly label: string;
}

const secondarySx = (theme: Theme) => ({ color: theme.vars.palette.text.secondary, margin: 0 });

function roleName(role: { readonly name: string; readonly display_name?: string | undefined }): string {
  return role.display_name ?? role.name;
}

function GrantingRoles({ itemId }: { readonly itemId: string }): ReactNode {
  const query = useGetAdminConsoleItem(itemId, {
    query: { retry: false, select: (response) => (response.status === 200 ? response.data.granting_roles : undefined) },
  });
  if (query.isPending) {
    return (
      <Typography component="p" variant="bodyMedium" sx={secondarySx}>
        {t('pages.admin.itemDenied.rolesLoading', 'Looking up the roles that open it…')}
      </Typography>
    );
  }
  const roles = query.data;
  if (roles === undefined) {
    return (
      <Typography component="p" variant="bodyMedium" sx={secondarySx} data-testid="admin-item-denied-roles-error">
        {t('pages.admin.itemDenied.rolesError', 'The roles that open it could not be read.')}
      </Typography>
    );
  }
  if (roles.length === 0) {
    return (
      <Typography component="p" variant="bodyMedium" sx={secondarySx} data-testid="admin-item-denied-no-role">
        {t('pages.admin.itemDenied.noRole', 'No role on this deployment opens it.')}
      </Typography>
    );
  }
  return (
    <Box>
      <Typography component="p" variant="bodyMedium" sx={secondarySx}>
        {t('pages.admin.itemDenied.rolesIntro', 'These roles open it:')}
      </Typography>
      <Box component="ul" data-testid="admin-item-denied-roles" sx={{ margin: '0.25rem 0 0', paddingInlineStart: '1.25rem' }}>
        {roles.map((role) => (
          <Typography component="li" variant="bodyMedium" key={role.name} data-role={role.name}>
            {roleName(role)}
          </Typography>
        ))}
      </Box>
    </Box>
  );
}

export interface AdminSupportContactProps {
  /** The sentence before the contact; it ends with a colon. */
  readonly lead?: string;
  /** The sentence when the deployment names no support contact. */
  readonly fallback?: string;
}

/**
 * Whom to ask: the deployment's IT support line, or a fixed sentence when it
 * names none. Never an administrator's name.
 */
export function AdminSupportContact({ lead, fallback }: AdminSupportContactProps = {}): ReactNode {
  const query = useGetClientDiscovery({
    query: {
      retry: false,
      staleTime: 60_000,
      select: (response) => (response.status === 200 ? response.data.client_policy.support_contact : undefined),
    },
  });
  if (query.isPending) return null;
  const contact = normalizeSupportContact(query.data);
  if (contact === null) {
    return (
      <Typography component="p" variant="bodyMedium" sx={secondarySx} data-testid="admin-item-denied-contact">
        {fallback ??
          t(
            'pages.admin.itemDenied.askFallback',
            'To ask for one of these roles, contact the people who manage this deployment.',
          )}
      </Typography>
    );
  }
  const { text, href } = supportContactLink(contact);
  return (
    <Typography component="p" variant="bodyMedium" sx={secondarySx} data-testid="admin-item-denied-contact">
      {lead ?? t('pages.admin.itemDenied.askSupport', 'To ask for one of these roles, contact your IT support:')}{' '}
      {href === null ? (
        <span>{text}</span>
      ) : (
        <Link href={href} target="_blank" rel="noopener noreferrer">
          {text}
        </Link>
      )}
    </Typography>
  );
}

export function AdminItemAccessDenied({ itemId, label }: AdminItemAccessDeniedProps): ReactNode {
  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }} data-testid="admin-item-denied" data-item-id={itemId}>
      <Typography variant="headingLarge" component="h1" sx={{ margin: 0 }}>
        {t('pages.admin.itemDenied.title', 'You cannot open {{item}}', { item: label })}
      </Typography>
      <Typography component="p" variant="bodyMedium" sx={secondarySx}>
        {t(
          'pages.admin.itemDenied.explanation',
          'Your administration roles do not include {{item}}. The server refuses its pages and changes for you.',
          { item: label },
        )}
      </Typography>
      <GrantingRoles itemId={itemId} />
      <AdminSupportContact />
    </DrawerPage>
  );
}
