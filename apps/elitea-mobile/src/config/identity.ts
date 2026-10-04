import Constants from 'expo-constants';

/**
 * Runtime view of the identifiers defined in `app.config.ts` (the only place
 * they are written down). Import from here; never hardcode them.
 */
export interface AppIdentity {
  displayName: string;
  slug: string;
  bundleId: string;
  connectSchemes: readonly string[];
  oauthRedirectScheme: string;
  oauthRedirectPath: string;
  webUrl: string;
  privacyUrl: string;
  supportUrl: string;
}

function loadIdentity(): AppIdentity {
  const identity = Constants.expoConfig?.extra?.identity as AppIdentity | undefined;
  if (!identity) {
    throw new Error('app.config.ts is missing extra.identity');
  }
  return identity;
}

export const identity: AppIdentity = loadIdentity();

/** RFC 8252 redirect URI, e.g. `ca.zefir.agent:/oauth/callback`. */
export const oauthRedirectUri = `${identity.oauthRedirectScheme}:${identity.oauthRedirectPath}`;
