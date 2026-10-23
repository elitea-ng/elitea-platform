/**
 * The SAML 2.0 fields of the identity provider dialog.
 *
 * Its own file because the dialog was over the 400-line gate with both
 * protocols' fields inline, and SAML carries twice as many as OpenID Connect —
 * an identity provider's endpoints and trust anchors, and this deployment's own
 * service-provider identity.
 *
 * ## What is NOT here, and why
 *
 * No `want_assertions_signed` switch, no signature-algorithm list, no
 * "skip audience check". Each has exactly one safe answer and the server fixes
 * it where an operator cannot reach it: an assertion must be signed, the digest
 * must be SHA-256 or better, and Audience, Destination and InResponseTo are
 * checked on every response. Offering those as controls would be offering a
 * deployment the choice to be insecure.
 */
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import FormControlLabel from '@mui/material/FormControlLabel';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';

import { nameIdFormatOptions } from './adminIdentityProviderNameId';
import type { FormUpdate, IdentityProviderForm } from './adminIdentityProviderForm';

/** The SAML 2.0 fields. */
export function SamlFields({
  form,
  update,
}: {
  readonly form: IdentityProviderForm;
  readonly update: FormUpdate;
}) {
  return (
    <>
      <TextField
        label={t('pages.admin.identityProviders.dialog.idpEntityId', 'Identity provider entity ID')}
        value={form.idpEntityId}
        onChange={(event) => {
          update('idpEntityId', event.target.value);
        }}
        fullWidth
        required
      />
      <TextField
        label={t('pages.admin.identityProviders.dialog.idpSsoUrl', 'Single sign-on URL')}
        value={form.idpSsoUrl}
        onChange={(event) => {
          update('idpSsoUrl', event.target.value);
        }}
        fullWidth
        required
      />
      <TextField
        label={t('pages.admin.identityProviders.dialog.idpSloUrl', 'Single logout URL')}
        value={form.idpSloUrl}
        onChange={(event) => {
          update('idpSloUrl', event.target.value);
        }}
        fullWidth
        helperText={t(
          'pages.admin.identityProviders.dialog.idpSloUrlHelp',
          'Optional. Empty means a logout clears the local session only.',
        )}
      />
      <TextField
        label={t(
          'pages.admin.identityProviders.dialog.idpCertificates',
          'Identity provider signing certificates',
        )}
        value={form.idpCertificates}
        onChange={(event) => {
          update('idpCertificates', event.target.value);
        }}
        fullWidth
        required
        multiline
        minRows={4}
        helperText={t(
          'pages.admin.identityProviders.dialog.idpCertificatesHelp',
          'PEM or the bare base64 body from the provider metadata. Separate several with a blank line. Every one is parsed; an unreadable certificate is refused rather than skipped.',
        )}
        slotProps={{ htmlInput: { 'data-testid': 'identity-provider-idp-certificates' } }}
      />
      <TextField
        label={t('pages.admin.identityProviders.dialog.spEntityId', 'Service provider entity ID')}
        value={form.spEntityId}
        onChange={(event) => {
          update('spEntityId', event.target.value);
        }}
        fullWidth
        required
        helperText={t(
          'pages.admin.identityProviders.dialog.spEntityIdHelp',
          'This deployment’s identifier. An assertion must name it as its audience.',
        )}
      />
      <TextField
        label={t('pages.admin.identityProviders.dialog.acsUrl', 'Assertion consumer service URL')}
        value={form.acsUrl}
        onChange={(event) => {
          update('acsUrl', event.target.value);
        }}
        fullWidth
        required
        // The path is fixed by the router, so naming it here saves the operator
        // guessing it — and the metadata pointer beside it is what they upload
        // at the identity provider, built from this same row. It is served with
        // request signing off as well; only the certificate depends on it.
        helperText={t(
          'pages.admin.identityProviders.dialog.acsUrlHelp',
          'This deployment serves it at /auth/saml/acs. Service provider metadata is served at /auth/saml/metadata once this provider is enabled, with or without request signing. It carries a certificate only when requests are signed.',
        )}
      />
      <TextField
        select
        label={t('pages.admin.identityProviders.dialog.nameIdFormat', 'NameID format')}
        value={form.nameIdFormat}
        onChange={(event) => {
          update('nameIdFormat', event.target.value);
        }}
        fullWidth
        helperText={t(
          'pages.admin.identityProviders.dialog.nameIdFormatHelp',
          'The NameID format this deployment requests and publishes in its metadata. Email address lets the NameID serve as the sign-in address. Persistent is a stable opaque identifier. Transient changes at every sign-in, so set an email attribute with it.',
        )}
        // The empty option is the stored default, so it renders its label
        // rather than an empty box under the field label.
        slotProps={{
          select: { displayEmpty: true },
          inputLabel: { shrink: true },
          htmlInput: { 'data-testid': 'identity-provider-name-id-format' },
        }}
      >
        {nameIdFormatOptions(form.nameIdFormat).map((option) => (
          <MenuItem key={option.value} value={option.value}>
            {option.label}
          </MenuItem>
        ))}
      </TextField>
      <TextField
        label={t('pages.admin.identityProviders.dialog.emailAttribute', 'Email attribute')}
        value={form.emailAttribute}
        onChange={(event) => {
          update('emailAttribute', event.target.value);
        }}
        fullWidth
        helperText={t(
          'pages.admin.identityProviders.dialog.attributeHelp',
          'Optional. Empty falls back to the common SAML attribute names, and to the NameID when it is an address.',
        )}
      />
      <TextField
        label={t('pages.admin.identityProviders.dialog.nameAttribute', 'Display name attribute')}
        value={form.nameAttribute}
        onChange={(event) => {
          update('nameAttribute', event.target.value);
        }}
        fullWidth
      />
      <TextField
        label={t('pages.admin.identityProviders.dialog.clockSkew', 'Clock skew (seconds)')}
        value={form.clockSkewSeconds}
        onChange={(event) => {
          update('clockSkewSeconds', event.target.value);
        }}
        fullWidth
        helperText={t(
          'pages.admin.identityProviders.dialog.clockSkewHelp',
          'Tolerance applied to assertion time conditions. Empty for the default. At most 300 seconds.',
        )}
      />
      <FormControlLabel
        control={
          <BaseCheckbox
            checked={form.signAuthnRequests}
            onChange={(event) => {
              update('signAuthnRequests', event.target.checked);
            }}
          />
        }
        label={t(
          'pages.admin.identityProviders.dialog.signAuthnRequests',
          'Sign authentication requests',
        )}
      />
      {form.signAuthnRequests ? (
        <TextField
          label={t(
            'pages.admin.identityProviders.dialog.spCertificate',
            'Service provider certificate',
          )}
          value={form.spCertificate}
          onChange={(event) => {
            update('spCertificate', event.target.value);
          }}
          fullWidth
          multiline
          minRows={3}
          helperText={t(
            'pages.admin.identityProviders.dialog.spCertificateHelp',
            'The public certificate matching the private key below. It is published in this deployment’s metadata.',
          )}
        />
      ) : null}
    </>
  );
}

