/**
 * The controls of the E-mail editor, and the source tag beside each one.
 *
 * Split out of `./AdminEmailEditor.tsx` so the editor stays its own state
 * machine rather than a stack of fields — the same complexity-gate split
 * `AdminMcpServersEditor` made for its table and its alerts.
 *
 * ## Why every field carries a tag
 *
 * The relay is configured in TWO layers: this page's rows, and the deployment's
 * environment (SMTP_HOST and its siblings), with the rows winning field by
 * field. A blank control is therefore ambiguous on its own — it can mean "no
 * one has set this" or "the chart sets it and you have not overridden it" — and
 * an operator who reads the second as the first retypes a value the deployment
 * already has, or reads a blank host as "e-mail is off" on a deployment that
 * sends perfectly well. The tag removes the ambiguity, and the placeholder
 * shows the inherited value so the operator can see what they would be
 * replacing.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { AdminEmailSource } from './api/adminEmailApi';

/** The tag beside one control. */
export function EmailSourceTag({ source, field }: { readonly source: AdminEmailSource; readonly field: string }) {
  const label =
    source === 'database'
      ? t('pages.admin.email.source.database', 'Set here')
      : source === 'environment'
        ? t('pages.admin.email.source.environment', 'From the environment')
        : t('pages.admin.email.source.unset', 'Not set');
  return (
    <Chip
      size="small"
      variant="outlined"
      color={source === 'database' ? 'primary' : 'default'}
      label={label}
      data-testid={`admin-email-source-${field}`}
    />
  );
}

/** One labelled row: the control, then its tag. */
export function EmailField({
  field,
  source,
  children,
}: {
  readonly field: string;
  readonly source: AdminEmailSource;
  readonly children: ReactNode;
}) {
  return (
    <Box sx={{ display: 'flex', alignItems: 'flex-start', gap: '0.75rem' }}>
      <Box sx={{ flex: 1, minWidth: 0 }}>{children}</Box>
      <Box sx={{ paddingTop: '1rem' }}>
        <EmailSourceTag source={source} field={field} />
      </Box>
    </Box>
  );
}

/** The draft the fields edit. */
export interface EmailFormValues {
  readonly host: string;
  readonly port: string;
  readonly username: string;
  readonly tls: string;
  readonly from: string;
  readonly replyTo: string;
  readonly publicBaseURL: string;
}

export function AdminEmailFields({
  values,
  effective,
  sources,
  passwordSet,
  password,
  onChange,
  onPasswordChange,
  onClearPassword,
}: {
  readonly values: EmailFormValues;
  /** The merged document, for the placeholders that show what is inherited. */
  readonly effective: Record<string, string>;
  readonly sources: Readonly<Record<string, AdminEmailSource>>;
  readonly passwordSet: boolean;
  /** `undefined` while the operator has not touched the password. */
  readonly password: string | undefined;
  readonly onChange: (field: keyof EmailFormValues, value: string) => void;
  readonly onPasswordChange: (value: string) => void;
  readonly onClearPassword: () => void;
}) {
  const sourceOf = (field: string): AdminEmailSource => sources[field] ?? 'unset';

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem', maxWidth: '44rem' }}>
      <EmailField field="host" source={sourceOf('host')}>
        <TextField
          fullWidth
          size="small"
          label={t('pages.admin.email.field.host', 'SMTP host')}
          value={values.host}
          placeholder={effective['host'] ?? ''}
          onChange={(event) => onChange('host', event.target.value)}
          slotProps={{ htmlInput: { 'data-testid': 'admin-email-host' } }}
        />
      </EmailField>

      <EmailField field="port" source={sourceOf('port')}>
        <TextField
          fullWidth
          size="small"
          label={t('pages.admin.email.field.port', 'Port')}
          value={values.port}
          placeholder={effective['port'] ?? ''}
          helperText={t(
            'pages.admin.email.field.portHelp',
            'Leave blank for the default of the TLS mode: 587 for STARTTLS, 465 for TLS.',
          )}
          onChange={(event) => onChange('port', event.target.value)}
          slotProps={{ htmlInput: { 'data-testid': 'admin-email-port', inputMode: 'numeric' } }}
        />
      </EmailField>

      <EmailField field="tls" source={sourceOf('tls')}>
        <TextField
          select
          fullWidth
          size="small"
          label={t('pages.admin.email.field.tls', 'TLS mode')}
          value={values.tls}
          onChange={(event) => onChange('tls', event.target.value)}
          slotProps={{ htmlInput: { 'data-testid': 'admin-email-tls' } }}
        >
          <MenuItem value="">{t('pages.admin.email.tls.inherit', 'Inherit')}</MenuItem>
          <MenuItem value="starttls">{t('pages.admin.email.tls.starttls', 'STARTTLS (port 587)')}</MenuItem>
          <MenuItem value="tls">{t('pages.admin.email.tls.implicit', 'TLS (port 465)')}</MenuItem>
          <MenuItem value="none">{t('pages.admin.email.tls.none', 'None (in-cluster relay only)')}</MenuItem>
        </TextField>
      </EmailField>

      <EmailField field="username" source={sourceOf('username')}>
        <TextField
          fullWidth
          size="small"
          label={t('pages.admin.email.field.username', 'User name')}
          value={values.username}
          placeholder={effective['username'] ?? ''}
          helperText={t(
            'pages.admin.email.field.usernameHelp',
            'Leave the user name and the password both empty to submit without authentication.',
          )}
          onChange={(event) => onChange('username', event.target.value)}
          slotProps={{ htmlInput: { 'data-testid': 'admin-email-username' } }}
        />
      </EmailField>

      {/* The password is WRITE-ONLY. No endpoint returns it, so there is
          nothing to echo and a "reveal" control would be a lie. What the
          operator gets instead is whether one is set, and a way to clear it. */}
      <EmailField field="password" source={sourceOf('password')}>
        <TextField
          fullWidth
          size="small"
          type="password"
          autoComplete="new-password"
          label={t('pages.admin.email.field.password', 'Password')}
          value={password ?? ''}
          placeholder={
            passwordSet
              ? t('pages.admin.email.field.passwordSet', 'A password is stored. Type to replace it.')
              : t('pages.admin.email.field.passwordUnset', 'No password stored.')
          }
          helperText={
            password === ''
              ? t('pages.admin.email.field.passwordClearing', 'Saving now REMOVES the stored password.')
              : t(
                  'pages.admin.email.field.passwordHelp',
                  'Sealed in the platform vault and never shown again. Leave untouched to keep the stored one.',
                )
          }
          onChange={(event) => onPasswordChange(event.target.value)}
          slotProps={{ htmlInput: { 'data-testid': 'admin-email-password' } }}
        />
        {passwordSet && password === undefined ? (
          <Typography
            component="button"
            type="button"
            variant="bodySmall"
            color="error"
            onClick={onClearPassword}
            data-testid="admin-email-clear-password"
            sx={{ background: 'none', border: 0, cursor: 'pointer', padding: 0, marginTop: '0.25rem' }}
          >
            {t('pages.admin.email.field.passwordClear', 'Remove the stored password')}
          </Typography>
        ) : null}
      </EmailField>

      <EmailField field="from" source={sourceOf('from')}>
        <TextField
          fullWidth
          size="small"
          label={t('pages.admin.email.field.from', 'From')}
          value={values.from}
          placeholder={effective['from'] ?? t('pages.admin.email.field.fromExample', 'noreply@example.com')}
          onChange={(event) => onChange('from', event.target.value)}
          slotProps={{ htmlInput: { 'data-testid': 'admin-email-from' } }}
        />
      </EmailField>

      <EmailField field="reply_to" source={sourceOf('reply_to')}>
        <TextField
          fullWidth
          size="small"
          label={t('pages.admin.email.field.replyTo', 'Reply-to')}
          value={values.replyTo}
          placeholder={effective['reply_to'] ?? ''}
          onChange={(event) => onChange('replyTo', event.target.value)}
          slotProps={{ htmlInput: { 'data-testid': 'admin-email-reply-to' } }}
        />
      </EmailField>

      <EmailField field="public_base_url" source={sourceOf('public_base_url')}>
        <TextField
          fullWidth
          size="small"
          label={t('pages.admin.email.field.publicBaseUrl', 'Public base URL')}
          value={values.publicBaseURL}
          placeholder={
            effective['public_base_url'] ??
            t('pages.admin.email.field.publicBaseUrlExample', 'https://elitea.example.com')
          }
          helperText={t(
            'pages.admin.email.field.publicBaseUrlHelp',
            'The origin every link and the logo in a message are built from. An invitation with the wrong origin is an invitation nobody can accept.',
          )}
          onChange={(event) => onChange('publicBaseURL', event.target.value)}
          slotProps={{ htmlInput: { 'data-testid': 'admin-email-public-base-url' } }}
        />
      </EmailField>
    </Box>
  );
}
