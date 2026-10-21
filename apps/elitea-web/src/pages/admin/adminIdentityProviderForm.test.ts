/**
 * The SAML NameID select: it offers the URNs the server accepts, keeps an
 * empty default, keeps a stored value outside the list, and the draft carries
 * the chosen value.
 */
import { describe, expect, it } from 'vitest';

import { initialProviderForm, providerDraft } from './adminIdentityProviderForm';
import { nameIdFormatOptions, SAML_NAME_ID_FORMATS } from './adminIdentityProviderNameId';

describe('SAML NameID format', () => {
  it('offers the empty default first, then the four person formats', () => {
    const values = nameIdFormatOptions('').map((option) => option.value);
    expect(values).toEqual(['', ...SAML_NAME_ID_FORMATS]);
    expect(values).toContain('urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress');
  });

  it('keeps a stored format the list does not offer', () => {
    const stored = 'urn:oasis:names:tc:SAML:2.0:nameid-format:entity';
    const values = nameIdFormatOptions(stored).map((option) => option.value);
    expect(values.at(-1)).toBe(stored);
    expect(values).toHaveLength(SAML_NAME_ID_FORMATS.length + 2);
  });

  it('sends the chosen format in the SAML draft', () => {
    const form = {
      ...initialProviderForm(undefined),
      kind: 'saml' as const,
      displayName: 'Corp',
      nameIdFormat: 'urn:oasis:names:tc:SAML:2.0:nameid-format:persistent',
    };
    const draft = providerDraft(form, 'corp');
    expect(draft.saml?.name_id_format).toBe('urn:oasis:names:tc:SAML:2.0:nameid-format:persistent');
  });
});
