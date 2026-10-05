/**
 * Add or edit one native client (Admin › Configuration › Native clients).
 *
 * The redirect URI rules are the SERVER's, and the dialog shows them in two
 * places: once as guidance under the field, and again — per URI — when a save
 * is refused with 422, using the server's own reason for each line it
 * refused. The dialog stays open on a refusal, holding what was typed.
 */
import { useEffect, useState } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import FormControlLabel from '@mui/material/FormControlLabel';
import FormHelperText from '@mui/material/FormHelperText';
import TextField from '@mui/material/TextField';

import type { NativeClient } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import {
  initialNativeClientForm,
  nativeClientDraft,
  type NativeClientFieldErrors,
  type NativeClientForm,
} from './adminNativeClientForm';
import type { NativeClientDraft } from './api/adminNativeClientsApi';

export interface AdminNativeClientDialogProps {
  readonly open: boolean;
  /** `undefined` ⇒ register a new client; a client ⇒ edit it. */
  readonly editing: NativeClient | undefined;
  readonly isSaving: boolean;
  /** A refusal that names no field (409, 503, …), in the server's words. */
  readonly serverError: string | undefined;
  /** The 422's reasons, placed by field. */
  readonly fieldErrors: NativeClientFieldErrors;
  readonly onClose: () => void;
  readonly onSubmit: (draft: NativeClientDraft) => void;
}

type Update = <K extends keyof NativeClientForm>(key: K, value: NativeClientForm[K]) => void;

function RedirectUriField({
  form,
  update,
  errors,
}: {
  readonly form: NativeClientForm;
  readonly update: Update;
  readonly errors: readonly string[];
}) {
  return (
    <>
      <TextField
        label={t('pages.admin.nativeClients.dialog.redirectUris', 'Redirect URIs')}
        value={form.redirectUris}
        onChange={(event) => {
          update('redirectUris', event.target.value);
        }}
        multiline
        minRows={2}
        fullWidth
        required
        error={errors.length > 0}
        helperText={t(
          'pages.admin.nativeClients.dialog.redirectUrisHelp',
          'One per line. Use a private-use reverse-domain scheme such as com.example.app:/oauth/callback, or a loopback address without a port such as http://127.0.0.1/callback.',
        )}
        slotProps={{ htmlInput: { 'data-testid': 'native-client-redirect-uris' } }}
      />
      {errors.map((reason) => (
        <FormHelperText key={reason} error data-testid="native-client-redirect-uri-error">
          {reason}
        </FormHelperText>
      ))}
    </>
  );
}

export function AdminNativeClientDialog({
  open,
  editing,
  isSaving,
  serverError,
  fieldErrors,
  onClose,
  onSubmit,
}: AdminNativeClientDialogProps) {
  const isEdit = editing !== undefined;
  const [form, setForm] = useState<NativeClientForm>(() => initialNativeClientForm(undefined));

  useEffect(() => {
    if (!open) return;
    setForm(initialNativeClientForm(editing));
  }, [open, editing]);

  const update: Update = (key, value) => {
    setForm((previous) => ({ ...previous, [key]: value }));
  };

  // Disabling from this dialog revokes devices exactly as the row's switch
  // does, so it says so here too rather than only in the switch's confirm.
  const disablesLiveClient = isEdit && editing.enabled && !form.enabled;

  return (
    <Dialog open={open} onClose={onClose} maxWidth="sm" fullWidth data-testid="native-client-dialog">
      <DialogTitle>
        {isEdit
          ? t('pages.admin.nativeClients.dialog.editTitle', 'Edit native client')
          : t('pages.admin.nativeClients.dialog.createTitle', 'Register native client')}
      </DialogTitle>
      {/* `&&`: MUI zeroes the padding-top of a DialogContent that follows a
          DialogTitle with a more specific rule than a plain `sx`, which cut
          the first field's outlined label in half. */}
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: '1rem', '&&': { pt: '0.75rem' } }}>
        {serverError !== undefined ? (
          <Alert severity="error" data-testid="native-client-dialog-error">
            {serverError}
          </Alert>
        ) : null}
        {fieldErrors.other.map((reason) => (
          <Alert key={reason} severity="error">
            {reason}
          </Alert>
        ))}

        <TextField
          label={t('pages.admin.nativeClients.dialog.clientId', 'Client ID')}
          value={form.clientId}
          onChange={(event) => {
            update('clientId', event.target.value);
          }}
          // The id is the row's key and every device's foreign key: renaming
          // it would orphan every signed-in device, so it is fixed on edit.
          disabled={isEdit}
          fullWidth
          required
          error={fieldErrors.clientId !== undefined}
          helperText={
            fieldErrors.clientId ??
            t(
              'pages.admin.nativeClients.dialog.clientIdHelp',
              'The client_id the app sends. 3 to 64 lower-case letters, digits, dots, underscores or hyphens.',
            )
          }
          slotProps={{ htmlInput: { 'data-testid': 'native-client-id' } }}
        />
        <TextField
          label={t('pages.admin.nativeClients.dialog.displayName', 'Display name')}
          value={form.displayName}
          onChange={(event) => {
            update('displayName', event.target.value);
          }}
          fullWidth
          required
          error={fieldErrors.displayName !== undefined}
          helperText={
            fieldErrors.displayName ??
            t(
              'pages.admin.nativeClients.dialog.displayNameHelp',
              'Shown to users on the sign-in consent page and in their device list.',
            )
          }
          slotProps={{ htmlInput: { 'data-testid': 'native-client-display-name' } }}
        />
        <RedirectUriField form={form} update={update} errors={fieldErrors.redirectUris} />
        <TextField
          label={t('pages.admin.nativeClients.dialog.minClientVersion', 'Minimum app version')}
          value={form.minClientVersion}
          onChange={(event) => {
            update('minClientVersion', event.target.value);
          }}
          fullWidth
          error={fieldErrors.minClientVersion !== undefined}
          helperText={
            fieldErrors.minClientVersion ??
            t(
              'pages.admin.nativeClients.dialog.minClientVersionHelp',
              'Optional. Older versions of this app are asked to upgrade. It can only raise the deployment-wide minimum, never lower it.',
            )
          }
          slotProps={{ htmlInput: { 'data-testid': 'native-client-min-version' } }}
        />
        <FormControlLabel
          control={
            <BaseCheckbox
              checked={form.enabled}
              onChange={(event) => {
                update('enabled', event.target.checked);
              }}
            />
          }
          label={t('pages.admin.nativeClients.dialog.enabled', 'Users may sign in with this app')}
        />
        {disablesLiveClient ? (
          <Alert severity="warning" data-testid="native-client-dialog-disable-warning">
            {t(
              'pages.admin.nativeClients.dialog.disableWarning',
              'Saving signs out every device of this app. Enabling it again does not restore them.',
            )}
          </Alert>
        ) : null}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose} disabled={isSaving} sx={{ textTransform: 'none' }}>
          {t('pages.admin.nativeClients.dialog.cancel', 'Cancel')}
        </Button>
        <Button
          variant="elitea"
          color="primary"
          onClick={() => {
            onSubmit(nativeClientDraft(form));
          }}
          disabled={isSaving}
          sx={{ textTransform: 'none' }}
          data-testid="native-client-save"
        >
          {t('pages.admin.nativeClients.dialog.save', 'Save')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
