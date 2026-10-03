/**
 * The identity provider listing table.
 *
 * Its own file because the editor was over the 400-line gate, and because a
 * row's rendering — the mask, the live/not-in-use chip, the actions — is the
 * part most likely to change independently of the page's state machine.
 */
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { AdminIdentityProvider } from './api/adminIdentityProvidersApi';
import { monoFontFamily } from '@/shared/brand/typeScale';

function protocolLabel(kind: AdminIdentityProvider['kind']): string {
  return kind === 'oidc'
    ? t('pages.admin.identityProviders.kind.oidc', 'OpenID Connect')
    : t('pages.admin.identityProviders.kind.saml', 'SAML 2.0');
}

/** The endpoint an operator identifies a provider by, whichever protocol it is. */
function providerEndpoint(provider: AdminIdentityProvider): string {
  return provider.kind === 'oidc'
    ? (provider.oidc?.issuer ?? '')
    : (provider.saml?.idp_sso_url ?? '');
}

/** The provider table. */
export function ProviderTable({
  providers,
  onEdit,
  onRemove,
}: {
  readonly providers: readonly AdminIdentityProvider[];
  readonly onEdit: (provider: AdminIdentityProvider) => void;
  readonly onRemove: (provider: AdminIdentityProvider) => void;
}) {
  return (
    <TableContainer>
      {/* Named, so the table has an accessible name for a screen reader and a
          stable handle for the journey and visual specs. */}
      <Table
        size="small"
        aria-label={t('pages.admin.identityProviders.tableLabel', 'Identity providers')}
        data-testid="admin-identity-providers-table"
      >
        <TableHead>
          <TableRow>
            <TableCell>{t('pages.admin.identityProviders.column.name', 'Name')}</TableCell>
            <TableCell>{t('pages.admin.identityProviders.column.protocol', 'Protocol')}</TableCell>
            <TableCell>{t('pages.admin.identityProviders.column.endpoint', 'Endpoint')}</TableCell>
            <TableCell>{t('pages.admin.identityProviders.column.secret', 'Secret')}</TableCell>
            <TableCell>{t('pages.admin.identityProviders.column.status', 'Status')}</TableCell>
            <TableCell align="right">
              {t('pages.admin.identityProviders.column.actions', 'Actions')}
            </TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {providers.map((provider) => (
            <TableRow key={provider.key} hover>
              <TableCell>
                <Typography variant="bodyMedium">{provider.display_name}</Typography>
                <Typography variant="bodySmall" color="text.secondary">
                  {provider.key}
                </Typography>
              </TableCell>
              <TableCell>{protocolLabel(provider.kind)}</TableCell>
              <TableCell sx={{ wordBreak: 'break-all' }}>{providerEndpoint(provider)}</TableCell>
              <TableCell>
                {provider.secret !== undefined && provider.secret !== '' ? (
                  // The mask, exactly as the server sent it. There is no reveal
                  // control because there is nothing to reveal.
                  <Typography variant="bodySmall" sx={{ fontFamily: monoFontFamily }}>{provider.secret}</Typography>
                ) : (
                  <Typography variant="bodyMedium" color="text.secondary">
                    {t('pages.admin.identityProviders.noSecret', 'None')}
                  </Typography>
                )}
              </TableCell>
              <TableCell>
                <Chip
                  size="small"
                  label={
                    provider.enabled
                      ? t('pages.admin.identityProviders.enabled', 'Live')
                      : t('pages.admin.identityProviders.disabled', 'Not in use')
                  }
                  color={provider.enabled ? 'success' : 'default'}
                  variant="outlined"
                />
              </TableCell>
              <TableCell align="right">
                <Button
            variant="elitea" color="tertiary"
                  size="small"
                  onClick={() => {
                    onEdit(provider);
                  }}
                  sx={{ textTransform: 'none' }}
                >
                  {t('pages.admin.identityProviders.edit', 'Edit')}
                </Button>
                <Button
            variant="elitea" color="alarm"
                  size="small"
                                    onClick={() => {
                    onRemove(provider);
                  }}
                  sx={{ textTransform: 'none' }}
                >
                  {t('pages.admin.identityProviders.remove', 'Remove')}
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  );
}

