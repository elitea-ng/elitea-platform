/**
 * The outcome of a Microsoft app Test: one line per check, each translated here from
 * the server's stable id and detail code (never free text from Entra). The test cannot
 * prove consent: a delegated flow cannot be tested without a person, so the first
 * sign-in does that and reports Entra's error code back to the administrator.
 */
import CheckCircleOutlinedIcon from '@mui/icons-material/CheckCircleOutlined';
import ErrorOutlinedIcon from '@mui/icons-material/ErrorOutlined';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import type { MicrosoftAppCheck, MicrosoftAppTestResult } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

function checkName(id: string): string {
  switch (id) {
    case 'client_id':
      return t('pages.admin.microsoftApps.check.clientId', 'Client ID is a valid GUID');
    case 'authority':
      return t('pages.admin.microsoftApps.check.authority', 'The authority publishes Microsoft Entra metadata');
    case 'redirect_uri':
      return t('pages.admin.microsoftApps.check.redirect', 'The redirect URI is one Entra accepts and matches this deployment');
    case 'secret':
      return t('pages.admin.microsoftApps.check.secret', 'A client secret is stored');
    case 'secret_expiry':
      return t('pages.admin.microsoftApps.check.expiry', 'The client secret has not expired');
    default:
      return id;
  }
}

function checkDetail(check: MicrosoftAppCheck): string | undefined {
  if (check.detail === undefined || check.detail === '') return undefined;
  switch (check.detail) {
    case 'https_required':
      return t('pages.admin.microsoftApps.detail.httpsRequired', 'Set DEPLOYMENT_URL to the public https address.');
    case 'host_mismatch':
      return t('pages.admin.microsoftApps.detail.hostMismatch', 'You are on a different host than DEPLOYMENT_URL. Register the redirect URI of DEPLOYMENT_URL.');
    case 'unreachable':
      return t('pages.admin.microsoftApps.detail.unreachable', 'The platform could not reach login.microsoftonline.com.');
    case 'not_set':
      return t('pages.admin.microsoftApps.detail.notSet', 'Enter the client secret.');
    case 'expired':
      return t('pages.admin.microsoftApps.detail.expired', 'Create a new secret in Entra.');
    case 'expiring':
      return t('pages.admin.microsoftApps.detail.expiring', 'It expires within 30 days.');
    default:
      return check.detail;
  }
}

export function MicrosoftAppTestPanel({ appName, result }: { readonly appName: string; readonly result: MicrosoftAppTestResult }) {
  return (
    <Alert severity={result.ok ? 'success' : 'warning'} data-testid="microsoft-app-test-result">
      <Typography variant="bodyMedium">
        {result.ok
          ? t('pages.admin.microsoftApps.test.passed', '{{name}}: every check passed.', { name: appName })
          : t('pages.admin.microsoftApps.test.failed', '{{name}}: some checks need attention.', { name: appName })}
      </Typography>
      <Box component="ul" sx={{ m: 0, pl: '1.25rem' }}>
        {result.checks.map((check) => (
          <li key={check.id} data-testid={`microsoft-app-check-${check.id}`}>
            {check.ok ? (
              <CheckCircleOutlinedIcon fontSize="inherit" titleAccess={t('pages.admin.microsoftApps.check.ok', 'Passed')} />
            ) : (
              <ErrorOutlinedIcon fontSize="inherit" titleAccess={t('pages.admin.microsoftApps.check.failed', 'Needs attention')} />
            )}{' '}
            {checkName(check.id)}
            {checkDetail(check) === undefined ? '' : `: ${checkDetail(check)}`}
          </li>
        ))}
      </Box>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.microsoftApps.test.note',
          'Consent cannot be tested without a person. The first sign-in proves it and shows Entra’s error code if it fails.',
        )}
      </Typography>
    </Alert>
  );
}
