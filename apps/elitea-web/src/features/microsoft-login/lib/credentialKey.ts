/**
 * Which credential types sign in with Microsoft here, and the key a run finds the
 * resulting access token under.
 *
 * A run looks a token up by the credential's uuid and its authority (`{uuid}:{authority}`,
 * the same shape SharePoint's delegated sign-in uses; a token issued to another client or
 * authority is ignored). Which authority depends on the credential's mode:
 *
 *  - Custom app: the credential's own `oauth_discovery_endpoint`, as the person typed it;
 *  - Default app, and Dataverse (which has no discovery endpoint, its authority is built
 *    from `tenant_id`): the authority the sign-in actually went to, read off the
 *    authorization URL Main built.
 */

/** The credential types whose sign-in this feature serves. SharePoint keeps its own flow. */
const MICROSOFT_CREDENTIAL_TYPES: ReadonlySet<string> = new Set(['outlook', 'teams', 'ms_graph', 'dataverse']);

export function isMicrosoftCredentialType(type: string | undefined): boolean {
  return type !== undefined && MICROSOFT_CREDENTIAL_TYPES.has(type);
}

function text(value: unknown): string {
  return typeof value === 'string' ? value.trim() : '';
}

/** `https://login.microsoftonline.com/{tenant}` out of `.../{tenant}/oauth2/v2.0/authorize?...`. */
function authorityOf(authorizationUrl: string): string | undefined {
  try {
    const url = new URL(authorizationUrl);
    const tenant = url.pathname.split('/').find((segment) => segment !== '');
    return tenant === undefined ? undefined : `${url.origin}/${tenant}`;
  } catch {
    return undefined;
  }
}

export interface SignInTokenKeyParams {
  readonly credentialUuid: string;
  /** The credential's form values (`oauth_app_id`, `oauth_discovery_endpoint`). */
  readonly data: Readonly<Record<string, unknown>>;
  readonly authorizationUrl: string;
}

/** The key the access token of this sign-in is stored under, or `undefined` when no authority can be named. */
export function signInTokenKey({ credentialUuid, data, authorizationUrl }: SignInTokenKeyParams): string | undefined {
  const endpoint = text(data['oauth_discovery_endpoint']);
  const defaultApp = text(data['oauth_app_id']) !== '';
  const authority = !defaultApp && endpoint !== '' ? endpoint : authorityOf(authorizationUrl);
  return authority === undefined ? undefined : `${credentialUuid}:${authority}`;
}
