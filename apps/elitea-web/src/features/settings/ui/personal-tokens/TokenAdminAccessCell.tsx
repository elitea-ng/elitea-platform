/**
 * The administration access of a token in the list (ADR-0059 R1.3b).
 *
 * - `scoped`: the scopes, one per line.
 * - `inherit`: a warning that the token uses all of its owner's
 *   administration permissions, with the date it ends once the server sends
 *   one (the sunset date of ADR-0059 decision 2), and no date before.
 * - `none` (and a backend that sends nothing): "None".
 */
import { memo } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { currentLocale, t } from '@/shared/i18n';
import type { PersonalAccessToken } from '@/entities/token';

export interface TokenAdminAccessCellProps {
  readonly token: PersonalAccessToken;
}

/** The end date (YYYY-MM-DD, a calendar date) in the reader's locale; the raw value if it does not parse. */
function endDateLabel(date: string): string {
  const parsed = new Date(`${date}T00:00:00Z`);
  if (Number.isNaN(parsed.getTime())) return date;
  return new Intl.DateTimeFormat(currentLocale(), { dateStyle: 'medium', timeZone: 'UTC' }).format(parsed);
}

export const TokenAdminAccessCell = memo(function TokenAdminAccessCell({ token }: TokenAdminAccessCellProps) {
  if (token.admin_access === 'inherit') {
    return (
      <Typography
        variant="bodyMedium"
        color="warning.main"
        data-testid="token-admin-inherit"
      >
        {token.inherit_ends_on === undefined
          ? t('entities.token.table.adminInherit', 'All your administration permissions')
          : t('entities.token.table.adminInheritEnds', 'All your administration permissions, until {{date}}', {
              date: endDateLabel(token.inherit_ends_on),
            })}
      </Typography>
    );
  }
  if (token.admin_access === 'scoped' && (token.admin_scopes?.length ?? 0) > 0) {
    return (
      <Box data-testid="token-admin-scopes-cell">
        {token.admin_scopes?.map((scope) => (
          <Typography
            key={scope}
            variant="bodySmall"
            color="text.secondary"
            component="div"
          >
            {scope}
          </Typography>
        ))}
      </Box>
    );
  }
  return (
    <Typography
      variant="bodyMedium"
      color="text.secondary"
    >
      {t('entities.token.table.adminNone', 'None')}
    </Typography>
  );
});
