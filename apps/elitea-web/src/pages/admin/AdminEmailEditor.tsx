/**
 * Admin › E-mail — the outbound relay editor (gap G7).
 *
 * ## What this screen is
 *
 * The place a deployment's SMTP relay is configured. Before it, the transport
 * was read from the process environment at boot, so an operator with no access
 * to the chart could not turn e-mail on at all — and `POST /admin/user_invite`
 * answered `invitation_delivered: false` on every such install. The settings
 * now live in the database, the resolver merges them over the environment on
 * every send, and a save therefore reaches the NEXT message rather than the
 * next deployment.
 *
 * ## Two layers, shown as two things
 *
 * The form edits the layer this page owns. Beside each control is a tag saying
 * which layer actually decides that field, and the placeholder shows the value
 * that would be inherited. See `./AdminEmailFields.tsx` for why a blank control
 * on its own is ambiguous and what an operator does wrong when it is.
 *
 * ## The password is write-only
 *
 * No endpoint returns it: it is sealed in the platform vault's hidden bucket.
 * The form reports whether one is set and offers to replace or remove it, and
 * sends the field ONLY when the operator touched it — see the tri-state note in
 * `./api/adminEmailApi.ts`.
 *
 * ## The test button verifies the SETTINGS
 *
 * It sends through the same resolution as every other message, so pressing it
 * after a save answers the question the operator actually has: does what I just
 * typed work? A relay that refuses shows the relay's own sentence.
 *
 * The Configuration page reaches this component through a server-declared
 * `managed_surface`, never through a hardcoded section id — see
 * `./Configuration.tsx`.
 *
 * ## Authorisation
 *
 * Every route this page calls is gated server-side on `runtime.plugins`
 * (`internal/api/router.go`) — the same permission the Configuration page it
 * replaces already required. `window.admin_ui_config.permissions` hides the nav
 * item and never gates anything: see `./adminUiConfig`.
 */
import { useEffect, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import LinearProgress from '@mui/material/LinearProgress';
import Snackbar from '@mui/material/Snackbar';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { AdminEmailFields, type EmailFormValues } from './AdminEmailFields';
import { configFailureReason } from './api/adminConfigurationApi';
import {
  useAdminEmailSettings,
  useSaveAdminEmailSettings,
  useSendAdminEmailTest,
  type AdminEmailSettings,
  type AdminEmailTlsMode,
} from './api/adminEmailApi';

/** The stored layer, as the form edits it. A port of 0 is "not stated". */
function toFormValues(settings: AdminEmailSettings): EmailFormValues {
  return {
    host: settings.host,
    port: settings.port === 0 ? '' : String(settings.port),
    username: settings.username,
    tls: settings.tls,
    from: settings.from,
    replyTo: settings.reply_to,
    publicBaseURL: settings.public_base_url,
  };
}

/** The effective layer as strings, for the placeholders. */
function toPlaceholders(settings: AdminEmailSettings): Record<string, string> {
  return {
    host: settings.host,
    port: settings.port === 0 ? '' : String(settings.port),
    username: settings.username,
    tls: settings.tls,
    from: settings.from,
    reply_to: settings.reply_to,
    public_base_url: settings.public_base_url,
  };
}

export function AdminEmailEditor() {
  const state = useAdminEmailSettings();
  const save = useSaveAdminEmailSettings();
  const test = useSendAdminEmailTest();

  const [values, setValues] = useState<EmailFormValues | undefined>(undefined);
  // `undefined` means the operator has not touched the password, which is what
  // makes the save leave the sealed one alone.
  const [password, setPassword] = useState<string | undefined>(undefined);
  const [testTo, setTestTo] = useState('');
  const [notice, setNotice] = useState<string | undefined>(undefined);

  // The form is seeded from the server's document ONCE per load, and never
  // again while it is dirty: re-seeding on every refetch would throw away what
  // the operator is typing.
  useEffect(() => {
    if (state.data !== undefined && values === undefined) {
      setValues(toFormValues(state.data.settings));
    }
  }, [state.data, values]);

  if (state.isLoading || values === undefined) {
    return <LinearProgress data-testid="admin-email-loading" />;
  }
  if (state.data === undefined) {
    return (
      <Alert severity="warning" data-testid="admin-email-error">
        {configFailureReason(state.error) ??
          t('pages.admin.email.error.load', 'Failed to load the outbound e-mail settings.')}
      </Alert>
    );
  }

  const change = (field: keyof EmailFormValues, value: string) =>
    setValues((current) => (current === undefined ? current : { ...current, [field]: value }));

  const submit = () => {
    const port = values.port.trim();
    save.mutate(
      {
        host: values.host.trim(),
        // An unparsable port is sent as 0 — "not stated" — rather than as NaN,
        // which JSON.stringify turns into `null` and the server rejects with a
        // decode error that names no field.
        port: port === '' || Number.isNaN(Number(port)) ? 0 : Number(port),
        username: values.username.trim(),
        tls: values.tls as AdminEmailTlsMode | '',
        from: values.from.trim(),
        replyTo: values.replyTo.trim(),
        publicBaseURL: values.publicBaseURL.trim(),
        password,
      },
      {
        onSuccess: (saved) => {
          // Re-seed from what the SERVER stored, never from the request: a
          // write that was normalised or partially refused must look different
          // from one that landed exactly as typed.
          setValues(toFormValues(saved.settings));
          setPassword(undefined);
          setNotice(t('pages.admin.email.saved', 'The outbound e-mail settings were saved.'));
        },
      },
    );
  };

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      {/* Whether a message can be sent RIGHT NOW is the one fact an operator
          comes to this page for, so it is stated before the form rather than
          left to be inferred from eight blank controls. */}
      {state.data.configured ? (
        <Alert severity="success" data-testid="admin-email-configured">
          {t('pages.admin.email.configured', 'Outbound e-mail is configured. Invitations are delivered.')}
        </Alert>
      ) : (
        <Alert severity="warning" data-testid="admin-email-not-configured">
          {state.data.reason ??
            t(
              'pages.admin.email.notConfigured',
              'Outbound e-mail is not configured, so no invitation is delivered.',
            )}
        </Alert>
      )}

      {save.error != null ? (
        <Alert severity="error" data-testid="admin-email-save-error">
          {configFailureReason(save.error) ??
            t('pages.admin.email.error.save', 'Failed to save the outbound e-mail settings.')}
        </Alert>
      ) : null}

      <AdminEmailFields
        values={values}
        effective={toPlaceholders(state.data.effective)}
        sources={state.data.sources}
        passwordSet={state.data.password_set}
        passwordSource={state.data.password_source}
        password={password}
        onChange={change}
        onPasswordChange={setPassword}
        onClearPassword={() => setPassword('')}
      />

      <Box>
        <Button
          variant="elitea"
          color="primary"
          onClick={submit}
          loading={save.isPending}
          data-testid="admin-email-save"
        >
          {t('pages.admin.email.save', 'Save')}
        </Button>
      </Box>

      <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem', maxWidth: '44rem' }}>
        <Typography variant="h6" component="h2">
          {t('pages.admin.email.test.title', 'Send a test e-mail')}
        </Typography>
        <Typography variant="bodyMedium" color="text.secondary">
          {t(
            'pages.admin.email.test.body',
            'The test message goes through exactly the settings above, so it says whether they work.',
          )}
        </Typography>
        {test.error != null ? (
          <Alert severity="error" data-testid="admin-email-test-error">
            {configFailureReason(test.error) ??
              t('pages.admin.email.error.test', 'The test message could not be sent.')}
          </Alert>
        ) : null}
        <Box sx={{ display: 'flex', gap: '0.75rem', alignItems: 'flex-start' }}>
          <TextField
            size="small"
            label={t('pages.admin.email.test.to', 'Send to')}
            value={testTo}
            onChange={(event) => setTestTo(event.target.value)}
            sx={{ flex: 1 }}
            slotProps={{ htmlInput: { 'data-testid': 'admin-email-test-to' } }}
          />
          <Button
            variant="outlined"
            color="primary"
            disabled={testTo.trim() === ''}
            loading={test.isPending}
            data-testid="admin-email-test-send"
            onClick={() =>
              test.mutate(testTo.trim(), {
                onSuccess: () =>
                  setNotice(t('pages.admin.email.test.sent', 'The test message was accepted by the relay.')),
              })
            }
          >
            {t('pages.admin.email.test.send', 'Send test e-mail')}
          </Button>
        </Box>
      </Box>

      <Snackbar
        open={notice !== undefined}
        autoHideDuration={4000}
        onClose={() => setNotice(undefined)}
        message={notice ?? ''}
        data-testid="admin-email-notice"
      />
    </Box>
  );
}
