/**
 * The identity provider dialog's data rules, separated from its rendering.
 *
 * These are the parts worth reading and testing on their own: the key
 * derivation that has to match the server character for character, the
 * tri-state that decides whether a credential survives a save, and the
 * client-side checks. `./AdminIdentityProviderDialog.tsx` is the controls.
 *
 * Everything here is a COURTESY. `internal/identityproviders/validate.go`
 * refuses each of these independently and names the field it refused, and its
 * words are what the dialog renders when the two disagree. The point of
 * checking here is to save a round trip, never to be the gate.
 */
import { t } from '@/shared/i18n';

import type {
  AdminIdentityProvider,
  AdminIdentityProviderDraft,
  AdminIdentityProviderKind,
  AdminOidcDocument,
  AdminSamlDocument,
} from './api/adminIdentityProvidersApi';

/**
 * The server's `NormalizeKey`, reproduced so the operator sees the key their
 * display name will be stored under before they save it.
 *
 * It must stay identical to `internal/identityproviders.NormalizeKey`: lower
 * case, and any run of characters outside [a-z0-9] collapses to one underscore,
 * with leading and trailing underscores removed.
 */
export function normalizeProviderKey(name: string): string {
  return name
    .toLowerCase()
    .replaceAll(/[^a-z0-9]+/g, '_')
    .replaceAll(/^_+|_+$/g, '');
}

/**
 * The dialog's fields as ONE state object.
 *
 * Both protocols' fields live in one shape rather than two, because the dialog
 * keeps what was typed when the operator switches kind mid-edit. Two objects
 * would either discard that or need a merge at every switch.
 */
export interface IdentityProviderForm {
  readonly kind: AdminIdentityProviderKind;
  readonly displayName: string;
  readonly enabled: boolean;
  readonly secret: string;
  readonly clearSecret: boolean;
  /**
   * The login domains, as typed: separated by commas, spaces or new lines.
   * Shared by both protocols, because both documents carry the same list.
   */
  readonly loginDomains: string;
  /**
   * `adopt_scim_users`: whether this provider's first sign-in may adopt an
   * account a SCIM client provisioned. Shared by both protocols. Off by
   * default — see `internal/identityproviders/provider.go`.
   */
  readonly adoptScimUsers: boolean;

  readonly issuer: string;
  readonly clientId: string;
  readonly redirectUri: string;
  readonly scopes: string;
  readonly requireEmailVerified: boolean;

  readonly idpEntityId: string;
  readonly idpSsoUrl: string;
  readonly idpSloUrl: string;
  readonly idpCertificates: string;
  readonly spEntityId: string;
  readonly acsUrl: string;
  readonly nameIdFormat: string;
  readonly emailAttribute: string;
  readonly nameAttribute: string;
  readonly signAuthnRequests: boolean;
  readonly spCertificate: string;
  readonly clockSkewSeconds: string;
}

/**
 * How a field is written back.
 *
 * Declared beside the form shape rather than in the dialog, because two
 * components now take it and a second declaration is a second definition to
 * keep in step.
 */
export type FormUpdate = <K extends keyof IdentityProviderForm>(
  key: K,
  value: IdentityProviderForm[K],
) => void;

const EMPTY_FORM: IdentityProviderForm = {
  kind: 'oidc',
  displayName: '',
  // A NEW provider arrives disabled. An operator authors the document first and
  // turns it on after, rather than replacing a working provider halfway through
  // typing one.
  enabled: false,
  secret: '',
  clearSecret: false,
  loginDomains: '',
  adoptScimUsers: false,

  issuer: '',
  clientId: '',
  redirectUri: '',
  scopes: '',
  requireEmailVerified: false,

  idpEntityId: '',
  idpSsoUrl: '',
  idpSloUrl: '',
  idpCertificates: '',
  spEntityId: '',
  acsUrl: '',
  nameIdFormat: '',
  emailAttribute: '',
  nameAttribute: '',
  signAuthnRequests: false,
  spCertificate: '',
  clockSkewSeconds: '',
};

/**
 * What the dialog shows when it opens on a provider, or on nothing.
 *
 * `secret` is ALWAYS empty and `clearSecret` always false, whatever the
 * provider holds: the server never sends the secret, so there is nothing to
 * pre-fill with, and "untouched" must mean "leave the sealed one alone".
 */
export function initialProviderForm(
  editing: AdminIdentityProvider | undefined,
): IdentityProviderForm {
  if (editing === undefined) return EMPTY_FORM;
  return {
    ...EMPTY_FORM,
    kind: editing.kind,
    displayName: editing.display_name,
    enabled: editing.enabled,
    loginDomains: (editing.oidc?.login_domains ?? editing.saml?.login_domains ?? []).join(', '),
    adoptScimUsers: editing.oidc?.adopt_scim_users ?? editing.saml?.adopt_scim_users ?? false,
    ...oidcFormFields(editing.oidc),
    ...samlFormFields(editing.saml),
  };
}

/**
 * The OpenID Connect half of an opened provider.
 *
 * Split from initialProviderForm because that function was one expression with
 * twenty fallbacks, which the complexity gate counted — correctly. Each half is
 * now readable on its own, and a missing document yields the blank fields
 * rather than a special case at the call site.
 */
function oidcFormFields(oidc: AdminOidcDocument | undefined): Partial<IdentityProviderForm> {
  if (oidc === undefined) return {};
  return {
    issuer: oidc.issuer,
    clientId: oidc.client_id,
    redirectUri: oidc.redirect_uri,
    // The server stores `openid` first and always. Showing it back would invite
    // the operator to delete it, so only the scopes they authored are listed.
    scopes: (oidc.scopes ?? []).filter((scope) => scope !== 'openid').join(' '),
    requireEmailVerified: oidc.require_email_verified ?? false,
  };
}

/** The SAML half of an opened provider. */
function samlFormFields(saml: AdminSamlDocument | undefined): Partial<IdentityProviderForm> {
  if (saml === undefined) return {};
  const skew = saml.clock_skew_seconds ?? 0;
  return {
    idpEntityId: saml.idp_entity_id,
    idpSsoUrl: saml.idp_sso_url,
    idpSloUrl: saml.idp_slo_url ?? '',
    idpCertificates: (saml.idp_certificates ?? []).join('\n\n'),
    spEntityId: saml.sp_entity_id,
    acsUrl: saml.acs_url,
    nameIdFormat: saml.name_id_format ?? '',
    emailAttribute: saml.email_attribute ?? '',
    nameAttribute: saml.name_attribute ?? '',
    signAuthnRequests: saml.sign_authn_requests ?? false,
    spCertificate: saml.sp_certificate ?? '',
    // A zero skew renders as an EMPTY field: zero means "the default applies"
    // on the server, and a literal 0 would read as a tolerance of none that the
    // operator had chosen.
    clockSkewSeconds: skew > 0 ? String(skew) : '',
  };
}

/**
 * Turns the two secret controls into the tri-state the server reads.
 *
 * `undefined` ⇒ omit the field, leaving the sealed secret alone. `''` ⇒ clear
 * it. A value ⇒ re-seal it.
 *
 * This is the one place on this screen where a wrong answer silently and
 * permanently destroys a credential — and on this surface that credential is
 * what the deployment's single sign-on runs on. It is three lines that look
 * obviously right, which is exactly why it is a named function with its own
 * test.
 */
export function resolveProviderSecretForSave(typed: string, clear: boolean): string | undefined {
  // Clearing WINS over a typed value. The checkbox disables the field, so a
  // keystroke left over from before it was ticked must not resurrect a secret
  // the operator chose to remove.
  if (clear) return '';
  if (typed === '') return undefined;
  return typed;
}

/** Splits the certificate textarea into the entries the server stores. */
function splitCertificates(raw: string): string[] {
  // PEM blocks span many lines, so a blank line is the separator rather than a
  // newline. A newline split would cut every armoured certificate into
  // fragments, none of which parses.
  return raw
    .split(/\n\s*\n/)
    .map((entry) => entry.trim())
    .filter((entry) => entry !== '');
}

/**
 * Splits the login domains field into the entries the server stores.
 *
 * Lower case and without a leading "@", as the server stores them. The server
 * normalises the same way, so this only makes the client check exact.
 */
export function splitLoginDomains(raw: string): string[] {
  const domains = raw
    .split(/[\s,;]+/)
    .map((entry) => entry.trim().toLowerCase().replace(/^@/, ''))
    .filter((entry) => entry !== '');
  return [...new Set(domains)];
}

/**
 * A host name of two or more labels: the server's `IsLoginDomain`. Each label
 * is 1 to 63 characters of [a-z0-9-] and does not start or end with "-".
 */
const LOGIN_DOMAIN_PATTERN =
  /^(?=.{1,253}$)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;

export function isLoginDomain(value: string): boolean {
  return LOGIN_DOMAIN_PATTERN.test(value);
}

/** Builds the draft the save mutation sends. */
export function providerDraft(
  form: IdentityProviderForm,
  key: string,
): AdminIdentityProviderDraft {
  const secret = resolveProviderSecretForSave(form.secret, form.clearSecret);
  if (form.kind === 'oidc') {
    return {
      key,
      kind: 'oidc',
      displayName: form.displayName.trim(),
      enabled: form.enabled,
      secret,
      oidc: {
        issuer: form.issuer.trim(),
        client_id: form.clientId.trim(),
        redirect_uri: form.redirectUri.trim(),
        scopes: form.scopes.split(/\s+/).filter((scope) => scope !== ''),
        require_email_verified: form.requireEmailVerified,
        login_domains: splitLoginDomains(form.loginDomains),
        adopt_scim_users: form.adoptScimUsers,
      },
    };
  }
  const skew = form.clockSkewSeconds.trim();
  return {
    key,
    kind: 'saml',
    displayName: form.displayName.trim(),
    enabled: form.enabled,
    secret,
    saml: {
      idp_entity_id: form.idpEntityId.trim(),
      idp_sso_url: form.idpSsoUrl.trim(),
      idp_slo_url: form.idpSloUrl.trim(),
      idp_certificates: splitCertificates(form.idpCertificates),
      sp_entity_id: form.spEntityId.trim(),
      acs_url: form.acsUrl.trim(),
      name_id_format: form.nameIdFormat.trim(),
      email_attribute: form.emailAttribute.trim(),
      name_attribute: form.nameAttribute.trim(),
      sign_authn_requests: form.signAuthnRequests,
      sp_certificate: form.spCertificate.trim(),
      clock_skew_seconds: skew === '' ? 0 : Number(skew),
      login_domains: splitLoginDomains(form.loginDomains),
      adopt_scim_users: form.adoptScimUsers,
    },
  };
}

/**
 * The dialog's client-side checks, as one pure function.
 *
 * Returns the message to show, or `undefined` when the draft is acceptable.
 */
export function validateProviderForm(
  form: IdentityProviderForm,
  isEdit: boolean,
  existingKeys: ReadonlySet<string>,
): string | undefined {
  const name = form.displayName.trim();
  if (name === '') {
    return t('pages.admin.identityProviders.dialog.error.nameRequired', 'Display name is required.');
  }
  const key = normalizeProviderKey(name);
  if (key === '') {
    return t(
      'pages.admin.identityProviders.dialog.error.keyEmpty',
      'That name has no usable key. Include a letter or a digit.',
    );
  }
  if (!isEdit && existingKeys.has(key)) {
    return t(
      'pages.admin.identityProviders.dialog.error.duplicate',
      'An identity provider already uses that key.',
    );
  }
  const badDomain = splitLoginDomains(form.loginDomains).find((domain) => !isLoginDomain(domain));
  if (badDomain !== undefined) {
    return t(
      'pages.admin.identityProviders.dialog.error.loginDomain',
      '“{{domain}}” is not a domain name such as example.com.',
      { domain: badDomain },
    );
  }
  return form.kind === 'oidc' ? validateOidcForm(form) : validateSamlForm(form);
}

function validateOidcForm(form: IdentityProviderForm): string | undefined {
  if (form.issuer.trim() === '') {
    return t('pages.admin.identityProviders.dialog.error.issuer', 'An issuer URL is required.');
  }
  if (form.clientId.trim() === '') {
    return t(
      'pages.admin.identityProviders.dialog.error.clientId',
      'A client identifier is required.',
    );
  }
  if (form.redirectUri.trim() === '') {
    return t(
      'pages.admin.identityProviders.dialog.error.redirectUri',
      'A redirect URI is required.',
    );
  }
  return undefined;
}

function validateSamlForm(form: IdentityProviderForm): string | undefined {
  if (form.idpEntityId.trim() === '' || form.idpSsoUrl.trim() === '') {
    return t(
      'pages.admin.identityProviders.dialog.error.idp',
      'The identity provider entity ID and single sign-on URL are required.',
    );
  }
  if (splitCertificates(form.idpCertificates).length === 0) {
    return t(
      'pages.admin.identityProviders.dialog.error.certificates',
      'At least one identity provider signing certificate is required.',
    );
  }
  if (form.spEntityId.trim() === '' || form.acsUrl.trim() === '') {
    return t(
      'pages.admin.identityProviders.dialog.error.sp',
      'The service provider entity ID and assertion consumer service URL are required.',
    );
  }
  const skew = form.clockSkewSeconds.trim();
  if (skew !== '' && (!Number.isInteger(Number(skew)) || Number(skew) < 0 || Number(skew) > 300)) {
    return t(
      'pages.admin.identityProviders.dialog.error.skew',
      'Clock skew must be a whole number of seconds between 0 and 300, or empty for the default.',
    );
  }
  return undefined;
}

/** The key the provider is, or will be, stored under. */
export function providerKeyHelperText(fixedKey: string | undefined, displayName: string): string {
  if (fixedKey !== undefined) {
    return t(
      'pages.admin.identityProviders.dialog.keyFixed',
      'Provider key: {{key}} (cannot change)',
      { key: fixedKey },
    );
  }
  return t('pages.admin.identityProviders.dialog.keyDerived', 'Provider key: {{key}}', {
    key: normalizeProviderKey(displayName.trim()) || '—',
  });
}
