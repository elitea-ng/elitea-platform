/**
 * The administration scopes of a new token (ADR-0059 R1.3b).
 *
 * Shown ONLY when the server offers scopes: a person without administration
 * permissions, and a sign-in that may set none (the desktop and mobile apps,
 * a token, a break-glass or elevated session), see nothing here and create a
 * token without administration access, as before. The list is the server's
 * offer, which already leaves out every `token: never` permission. `inherit`
 * is never offered: it exists only as an upgrade backfill.
 *
 * Nothing chosen means a token with no administration access.
 */
import { memo, useCallback } from 'react';

import Box from '@mui/material/Box';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import FormControlLabel from '@mui/material/FormControlLabel';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { useTokenScopeOfferQuery } from '../../api/personal-tokens/tokenAdminScopes';

export interface TokenAdminScopesFieldProps {
  /** The chosen scopes. */
  readonly value: readonly string[];
  readonly onChange: (next: readonly string[]) => void;
  readonly disabled?: boolean;
}

export const TokenAdminScopesField = memo(function TokenAdminScopesField({
  value,
  onChange,
  disabled = false,
}: TokenAdminScopesFieldProps) {
  const { data: offer } = useTokenScopeOfferQuery();
  const toggle = useCallback(
    (name: string, checked: boolean) => {
      onChange(checked ? [...value, name].sort() : value.filter((scope) => scope !== name));
    },
    [onChange, value],
  );

  if (offer === undefined || !offer.settable || offer.scopes.length === 0) return null;

  return (
    <Box
      component="fieldset"
      sx={FIELDSET_SX}
      data-testid="token-admin-scopes"
    >
      <Typography
        component="legend"
        variant="labelMedium"
        color="text.primary"
      >
        {t('entities.token.form.adminScopesTitle', 'Administration access')}
      </Typography>
      <Typography
        variant="bodySmall"
        color="text.secondary"
        sx={HELP_SX}
      >
        {t(
          'entities.token.form.adminScopesHelp',
          'Choose the administration permissions this token may use. Choose none for a token without administration access. Choosing any asks for a recent sign-in.',
        )}
      </Typography>
      <Box sx={LIST_SX}>
        {offer.scopes.map((scope) => (
          <FormControlLabel
            key={scope.name}
            sx={OPTION_SX}
            disabled={disabled}
            control={
              <BaseCheckbox
                size="small"
                checked={value.includes(scope.name)}
                onChange={(_, checked) => toggle(scope.name, checked)}
              />
            }
            label={
              <Typography
                variant="bodySmall"
                color="text.secondary"
              >
                {scope.name}
              </Typography>
            }
          />
        ))}
      </Box>
    </Box>
  );
});

/** The form's own field width, matching the name and expiration rows. */
const FIELDSET_SX: SxProps<Theme> = { width: '17.875rem', border: 'none', margin: 0, padding: 0, minInlineSize: 0 };
const HELP_SX: SxProps<Theme> = { display: 'block', marginTop: '0.25rem', marginBottom: '0.5rem' };
const LIST_SX: SxProps<Theme> = { display: 'flex', flexDirection: 'column', maxHeight: '15rem', overflowY: 'auto' };
const OPTION_SX: SxProps<Theme> = { marginLeft: 0, marginRight: 0 };
