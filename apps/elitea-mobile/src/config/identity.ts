import Constants from 'expo-constants';

/**
 * Runtime view of the identifiers defined in `app.config.ts` (the only place
 * they are written down). Import from here; never hardcode them.
 */
export interface AppIdentity {
  displayName: string;
  /** The short product name (the brand's `shortName`). */
  shortName: string;
  slug: string;
  bundleId: string;
  connectSchemes: readonly string[];
  oauthRedirectScheme: string;
  oauthRedirectPath: string;
  webUrl: string;
  privacyUrl: string;
  supportUrl: string;
  /** Universal links and App Links host, or null (the custom scheme works alone). */
  linkDomain: string | null;
  /** App Group shared with the widget extension (src/features/surfaces/). */
  appGroup: string;
  widgetBundleId: string;
  /** The brand's mark as SVG markup (`<BrandMark>`). */
  markSvg: string;
}

function loadIdentity(): AppIdentity {
  const identity = Constants.expoConfig?.extra?.identity as AppIdentity | undefined;
  if (!identity) {
    throw new Error('app.config.ts is missing extra.identity');
  }
  return identity;
}

export const identity: AppIdentity = loadIdentity();

/** RFC 8252 redirect URI, e.g. `ai.elitea.app:/oauth/callback`. */
export const oauthRedirectUri = `${identity.oauthRedirectScheme}:${identity.oauthRedirectPath}`;
