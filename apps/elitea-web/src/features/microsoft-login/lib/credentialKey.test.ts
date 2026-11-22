import { describe, expect, it } from 'vitest';

import { isMicrosoftCredentialType, signInTokenKey } from './credentialKey';

const UUID = '5b0f4a42-0f0f-4c1f-9c55-0a7d8f1a2b3c';
const URL_ = 'https://login.microsoftonline.com/contoso.onmicrosoft.com/oauth2/v2.0/authorize?client_id=abc';

describe('isMicrosoftCredentialType', () => {
  it('is true for the four types this feature signs in, and false for SharePoint and anything else', () => {
    for (const type of ['outlook', 'teams', 'ms_graph', 'dataverse']) expect(isMicrosoftCredentialType(type)).toBe(true);
    expect(isMicrosoftCredentialType('sharepoint')).toBe(false);
    expect(isMicrosoftCredentialType('openai')).toBe(false);
    expect(isMicrosoftCredentialType(undefined)).toBe(false);
  });
});

describe('signInTokenKey', () => {
  it('uses the authority the sign-in went to for a Custom app, not the endpoint text the form held', () => {
    // Main normalizes the stored authority (case, trailing slash, /v2.0), so the typed text can differ from what a run derives.
    expect(
      signInTokenKey({ credentialUuid: UUID, data: { oauth_discovery_endpoint: ' https://login.microsoftonline.com/Contoso.onmicrosoft.com/v2.0/ ' }, authorizationUrl: URL_ }),
    ).toBe(`${UUID}:https://login.microsoftonline.com/contoso.onmicrosoft.com`);
  });

  it('falls back to the endpoint the credential holds when the URL names no authority', () => {
    expect(
      signInTokenKey({ credentialUuid: UUID, data: { oauth_discovery_endpoint: 'https://login.microsoftonline.com/tenant-1' }, authorizationUrl: 'not a url' }),
    ).toBe(`${UUID}:https://login.microsoftonline.com/tenant-1`);
  });

  it('uses the authority the sign-in went to for a Default app, which has no endpoint of its own', () => {
    expect(signInTokenKey({ credentialUuid: UUID, data: { oauth_app_id: 'app-1' }, authorizationUrl: URL_ })).toBe(
      `${UUID}:https://login.microsoftonline.com/contoso.onmicrosoft.com`,
    );
  });

  it('ignores a leftover endpoint under a Default app', () => {
    expect(
      signInTokenKey({
        credentialUuid: UUID,
        data: { oauth_app_id: 'app-1', oauth_discovery_endpoint: 'https://login.microsoftonline.com/stale' },
        authorizationUrl: URL_,
      }),
    ).toBe(`${UUID}:https://login.microsoftonline.com/contoso.onmicrosoft.com`);
  });

  it('uses the authority of the authorization URL for Dataverse, whose tenant builds it', () => {
    expect(signInTokenKey({ credentialUuid: UUID, data: { url: 'https://org.crm.dynamics.com', tenant_id: 'contoso.onmicrosoft.com' }, authorizationUrl: URL_ })).toBe(
      `${UUID}:https://login.microsoftonline.com/contoso.onmicrosoft.com`,
    );
  });

  it('names nothing when no authority can be read', () => {
    expect(signInTokenKey({ credentialUuid: UUID, data: {}, authorizationUrl: 'not a url' })).toBeUndefined();
  });
});
