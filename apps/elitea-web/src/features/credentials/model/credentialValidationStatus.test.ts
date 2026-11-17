import { describe, expect, it } from 'vitest';

import { isCredentialRefusalReason } from './credentialValidationStatus';

describe('isCredentialRefusalReason', () => {
  it('refuses a credential the provider rejected', () => {
    expect(isCredentialRefusalReason('auth_failed')).toBe(true);
  });

  // #2143: an egress-policy refusal and a timeout both answer `unreachable`;
  // neither is a verdict on the credential, so neither may block Save.
  it.each(['unreachable', 'unsupported_type', 'ok', undefined])('does not treat %s as a verdict on the credential', (reason) => {
    expect(isCredentialRefusalReason(reason)).toBe(false);
  });
});
