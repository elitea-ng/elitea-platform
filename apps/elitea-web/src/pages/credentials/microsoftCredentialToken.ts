/**
 * pages/credentials/microsoftCredentialToken.ts — where the credential form finds the
 * access token of a Microsoft sign-in.
 *
 * `features/microsoft-login` remembers which key a sign-in's token was stored under, and
 * `features/mcps` holds the token itself (where runs read it, with its expiry). This file
 * is the one place the two meet, because features may not import each other.
 */
import { isMicrosoftCredentialType, readSession } from '@/features/microsoft-login';
import { getAccessToken } from '@/features/mcps';

/** The token a Microsoft credential's check carries, or `undefined` when none is held (or it ended). */
function readSignInToken(credentialUuid: string | undefined): string | undefined {
  const key = readSession(credentialUuid)?.tokenKey;
  if (key === undefined || key === '') return undefined;
  return getAccessToken(key) ?? undefined;
}

/**
 * The reader the connection check uses for this credential type, or `undefined` for a type
 * that does not sign in with Microsoft (its check is unchanged).
 */
export function microsoftTokenReader(
  credentialType: string | undefined,
  credentialUuid: string | undefined,
): (() => string | undefined) | undefined {
  if (!isMicrosoftCredentialType(credentialType)) return undefined;
  return () => readSignInToken(credentialUuid);
}
