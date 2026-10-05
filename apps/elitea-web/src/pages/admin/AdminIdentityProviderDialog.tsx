/**
 * Create / Edit dialog for one identity provider.
 *
 * One component for both, for the reason `./AdminMcpServerDialog.tsx` gives:
 * the two differ only in whether the key and the protocol are editable, and
 * splitting them would duplicate every field, the validation and the error
 * surface.
 *
 * ## The secret is never pre-filled
 *
 * The server never sends it — a read renders a mask — so there is nothing to
 * pre-fill with, and fetching the plaintext to display it would put the
 * deployment's single sign-on credential on screen because a dialog opened.
 *
 * That makes "unchanged" the DEFAULT on edit: leaving the field untouched sends
 * no `secret` at all and the sealed one stays. Clearing it is an explicit act,
 * and the checkbox is what makes the difference visible instead of hiding it in
 * an empty text field.
 *
 * ## What this dialog does not offer, and why
 *
 * There is no response-type control, no "enable PKCE" switch, no
 * `want_assertions_signed` box and no signature-algorithm list. Each of those
 * has exactly one safe answer, and the server fixes it where an operator cannot
 * reach it (`internal/identityproviders/provider.go`). Rendering them as
 * controls would be offering a deployment the choice to be insecure.
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
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';

import { SamlFields } from './AdminIdentityProviderSamlFields';
import {
  initialProviderForm,
  normalizeProviderKey,
  providerDraft,
  providerKeyHelperText,
  validateProviderForm,
  type FormUpdate,
  type IdentityProviderForm,
} from './adminIdentityProviderForm';
import type {
  AdminIdentityProvider,
  AdminIdentityProviderDraft,
} from './api/adminIdentityProvidersApi';

export interface AdminIdentityProviderDialogProps {
  readonly open: boolean;
  /** `undefined` ⇒ create; a provider ⇒ edit it. */
  readonly editing: AdminIdentityProvider | undefined;
  /** Every key currently authored, for the duplicate check on create. */
  readonly existingKeys: ReadonlySet<string>;
  readonly isSaving: boolean;
  /** The server's own words when the last attempt was refused. */
  readonly serverError: string | undefined;
  readonly onClose: () => void;
  readonly onSubmit: (draft: AdminIdentityProviderDraft) => void;
}

/**
 * The OpenID Connect fields.
 *
 * Split out so the dialog itself stays a state machine and each protocol's
 * fields are one thing. Both were over the complexity gate as one component.
 */
function OidcFields({
  form,
  update,
}: {
  readonly form: IdentityProviderForm;
  readonly update: FormUpdate;
}) {
  return (
    <>
      <TextField
        label={t('pages.admin.identityProviders.dialog.issuer', 'Issuer URL')}
        value={form.issuer}
        onChange={(event) => {
          update('issuer', event.target.value);
        }}
        fullWidth
        required
        // The discovery URL is what an operator moving across from the pylon
        // page has in hand. The server accepts it and stores the issuer, so
        // this says so rather than refusing a paste that will work.
        helperText={t(
          'pages.admin.identityProviders.dialog.issuerHelp',
          'For example https://idp.example.com. A .well-known/openid-configuration URL is accepted and reduced to its issuer.',
        )}
        slotProps={{ htmlInput: { 'data-testid': 'identity-provider-issuer' } }}
      />
      <TextField
        label={t('pages.admin.identityProviders.dialog.clientId', 'Client ID')}
        value={form.clientId}
        onChange={(event) => {
          update('clientId', event.target.value);
        }}
        fullWidth
        required
      />
      <TextField
        label={t('pages.admin.identityProviders.dialog.redirectUri', 'Redirect URI')}
        value={form.redirectUri}
        onChange={(event) => {
          update('redirectUri', event.target.value);
        }}
        fullWidth
        required
        helperText={t(
          'pages.admin.identityProviders.dialog.redirectUriHelp',
          'The callback this deployment serves. Register the same value at the identity provider.',
        )}
      />
      <TextField
        label={t('pages.admin.identityProviders.dialog.scopes', 'Additional scopes')}
        value={form.scopes}
        onChange={(event) => {
          update('scopes', event.target.value);
        }}
        fullWidth
        helperText={t(
          'pages.admin.identityProviders.dialog.scopesHelp',
          'Space separated. openid is always requested and is not listed here.',
        )}
      />
      <FormControlLabel
        control={
          <BaseCheckbox
            checked={form.requireEmailVerified}
            onChange={(event) => {
              update('requireEmailVerified', event.target.checked);
            }}
          />
        }
        label={t(
          'pages.admin.identityProviders.dialog.requireEmailVerified',
          'Require a verified email address on a first login',
        )}
      />
    </>
  );
}

/** The secret controls, which mean the same thing for both protocols. */
function SecretFields({
  form,
  update,
  isEdit,
  hasSealedSecret,
}: {
  readonly form: IdentityProviderForm;
  readonly update: FormUpdate;
  readonly isEdit: boolean;
  readonly hasSealedSecret: boolean;
}) {
  const label =
    form.kind === 'oidc'
      ? t('pages.admin.identityProviders.dialog.clientSecret', 'Client secret')
      : t('pages.admin.identityProviders.dialog.privateKey', 'Service provider private key');
  return (
    <>
      <TextField
        label={label}
        type="password"
        value={form.secret}
        onChange={(event) => {
          update('secret', event.target.value);
        }}
        disabled={form.clearSecret}
        fullWidth
        autoComplete="new-password"
        helperText={
          isEdit && hasSealedSecret
            ? t(
                'pages.admin.identityProviders.dialog.secretKeep',
                'A secret is sealed in the platform vault. Leave this empty to keep it.',
              )
            : t(
                'pages.admin.identityProviders.dialog.secretNew',
                'Sealed in the platform vault. No endpoint ever returns it.',
              )
        }
        slotProps={{ htmlInput: { 'data-testid': 'identity-provider-secret' } }}
      />
      {isEdit && hasSealedSecret ? (
        <FormControlLabel
          control={
            <BaseCheckbox
              checked={form.clearSecret}
              onChange={(event) => {
                update('clearSecret', event.target.checked);
              }}
            />
          }
          label={t(
            'pages.admin.identityProviders.dialog.clearSecret',
            'Remove the stored secret (a public client using PKCE alone)',
          )}
        />
      ) : null}
    </>
  );
}

export function AdminIdentityProviderDialog({
  open,
  editing,
  existingKeys,
  isSaving,
  serverError,
  onClose,
  onSubmit,
}: AdminIdentityProviderDialogProps) {
  const isEdit = editing !== undefined;
  const [form, setForm] = useState<IdentityProviderForm>(() => initialProviderForm(undefined));
  const [localError, setLocalError] = useState('');

  useEffect(() => {
    if (!open) return;
    setForm(initialProviderForm(editing));
    setLocalError('');
  }, [open, editing]);

  const update: FormUpdate = (key, value) => {
    setForm((previous) => ({ ...previous, [key]: value }));
  };

  const handleSubmit = (): void => {
    const problem = validateProviderForm(form, isEdit, existingKeys);
    if (problem !== undefined) {
      setLocalError(problem);
      return;
    }
    setLocalError('');
    // On edit the key is the stored one, never re-derived from the display
    // name: renaming a provider must not orphan its sealed secret, whose vault
    // name is derived from the key.
    onSubmit(providerDraft(form, editing?.key ?? normalizeProviderKey(form.displayName.trim())));
  };

  return (
    <Dialog open={open} onClose={onClose} maxWidth="sm" fullWidth data-testid="identity-provider-dialog">
      <DialogTitle>
        {isEdit
          ? t('pages.admin.identityProviders.dialog.editTitle', 'Edit identity provider')
          : t('pages.admin.identityProviders.dialog.createTitle', 'Add identity provider')}
      </DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: '1rem', pt: '0.5rem' }}>
        {serverError !== undefined ? <Alert severity="error">{serverError}</Alert> : null}
        {localError !== '' ? <Alert severity="warning">{localError}</Alert> : null}

        <TextField
          label={t('pages.admin.identityProviders.dialog.displayName', 'Display name')}
          value={form.displayName}
          onChange={(event) => {
            update('displayName', event.target.value);
          }}
          fullWidth
          required
          helperText={providerKeyHelperText(editing?.key, form.displayName)}
          slotProps={{ htmlInput: { 'data-testid': 'identity-provider-display-name' } }}
        />

        <TextField
          select
          label={t('pages.admin.identityProviders.dialog.kind', 'Protocol')}
          value={form.kind}
          onChange={(event) => {
            update('kind', event.target.value as IdentityProviderForm['kind']);
          }}
          // The protocol is FIXED on edit. Changing it would leave the previous
          // protocol's secret sealed under a name the new document never reads,
          // and the server refuses the save for that reason — so the control
          // says so instead of offering a choice that will be rejected.
          disabled={isEdit}
          fullWidth
          helperText={
            isEdit
              ? t(
                  'pages.admin.identityProviders.dialog.kindFixed',
                  'The protocol cannot change. Add a separate provider for the other one.',
                )
              : undefined
          }
        >
          <MenuItem value="oidc">
            {t('pages.admin.identityProviders.kind.oidc', 'OpenID Connect')}
          </MenuItem>
          <MenuItem value="saml">
            {t('pages.admin.identityProviders.kind.saml', 'SAML 2.0')}
          </MenuItem>
        </TextField>

        <TextField
          label={t('pages.admin.identityProviders.dialog.loginDomains', 'Login domains')}
          value={form.loginDomains}
          onChange={(event) => {
            update('loginDomains', event.target.value);
          }}
          fullWidth
          placeholder={t(
            'pages.admin.identityProviders.dialog.loginDomainsPlaceholder',
            'example.com, example.org',
          )}
          helperText={t(
            'pages.admin.identityProviders.dialog.loginDomainsHelp',
            'Optional. Separate domains with commas. On the sign-in page, a person who types a work email in one of these domains goes to this provider.',
          )}
          slotProps={{ htmlInput: { 'data-testid': 'identity-provider-login-domains' } }}
        />

        <FormControlLabel
          control={
            <BaseCheckbox
              checked={form.adoptScimUsers}
              onChange={(event) => {
                update('adoptScimUsers', event.target.checked);
              }}
              slotProps={{ input: { 'aria-describedby': 'identity-provider-adopt-scim-help' } }}
            />
          }
          label={t(
            'pages.admin.identityProviders.dialog.adoptScimUsers',
            'Users provisioned by SCIM may sign in through this provider',
          )}
        />
        <FormHelperText id="identity-provider-adopt-scim-help">
          {t(
            'pages.admin.identityProviders.dialog.adoptScimUsersHelp',
            'Turn this on only for the provider paired with your SCIM client (for example Entra ID). When it is off, a first sign-in through this provider cannot take over an account SCIM created.',
          )}
        </FormHelperText>

        {form.kind === 'oidc' ? (
          <OidcFields form={form} update={update} />
        ) : (
          <SamlFields form={form} update={update} />
        )}

        <SecretFields
          form={form}
          update={update}
          isEdit={isEdit}
          hasSealedSecret={editing?.secret !== undefined && editing.secret !== ''}
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
          label={t(
            'pages.admin.identityProviders.dialog.enabled',
            'Use this provider for logins',
          )}
        />
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose} disabled={isSaving} sx={{ textTransform: 'none' }}>
          {t('pages.admin.identityProviders.dialog.cancel', 'Cancel')}
        </Button>
        <Button
          variant="contained"
          onClick={handleSubmit}
          disabled={isSaving}
          sx={{ textTransform: 'none' }}
          data-testid="identity-provider-save"
        >
          {t('pages.admin.identityProviders.dialog.save', 'Save')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
