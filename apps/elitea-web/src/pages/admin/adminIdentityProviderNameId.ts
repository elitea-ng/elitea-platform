/**
 * The SAML NameID select's options, split from `./adminIdentityProviderForm.ts`
 * (which is at the 400-line gate).
 */
import { t } from '@/shared/i18n';

/**
 * The NameID formats the dialog offers, as the URNs
 * `internal/identityproviders/validate.go` accepts byte for byte.
 *
 * The server accepts three more (entity, X509SubjectName,
 * WindowsDomainQualifiedName). None is a person identifier a sign-in can use,
 * so they are not offered; a row that already holds one keeps it, see
 * `nameIdFormatOptions`.
 */
export const SAML_NAME_ID_FORMATS = [
  'urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified',
  'urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress',
  'urn:oasis:names:tc:SAML:2.0:nameid-format:persistent',
  'urn:oasis:names:tc:SAML:2.0:nameid-format:transient',
] as const;

export interface NameIdFormatOption {
  readonly value: string;
  readonly label: string;
}

/**
 * The NameID select's options.
 *
 * The first is EMPTY: the stored default, which requests no particular format.
 * A stored value outside the list is appended verbatim, so opening and saving
 * such a row does not silently change what it requests.
 */
export function nameIdFormatOptions(current: string): readonly NameIdFormatOption[] {
  const labels: Record<(typeof SAML_NAME_ID_FORMATS)[number], string> = {
    'urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified': t(
      'pages.admin.identityProviders.nameIdFormat.unspecified',
      'Unspecified',
    ),
    'urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress': t(
      'pages.admin.identityProviders.nameIdFormat.emailAddress',
      'Email address',
    ),
    'urn:oasis:names:tc:SAML:2.0:nameid-format:persistent': t(
      'pages.admin.identityProviders.nameIdFormat.persistent',
      'Persistent',
    ),
    'urn:oasis:names:tc:SAML:2.0:nameid-format:transient': t(
      'pages.admin.identityProviders.nameIdFormat.transient',
      'Transient',
    ),
  };
  const options: NameIdFormatOption[] = [
    {
      value: '',
      label: t(
        'pages.admin.identityProviders.nameIdFormat.none',
        'Not requested (identity provider default)',
      ),
    },
    ...SAML_NAME_ID_FORMATS.map((value) => ({ value, label: labels[value] })),
  ];
  const trimmed = current.trim();
  if (trimmed !== '' && !options.some((option) => option.value === trimmed)) {
    options.push({ value: trimmed, label: trimmed });
  }
  return options;
}
