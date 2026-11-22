/**
 * The two calls of the web sign-in (ADR-0057 decision 6): start a sign-in for a saved
 * credential, and redeem the handoff the callback page gave the popup.
 *
 * Both go through the generated client. The browser never talks to Entra for a token:
 * the code exchange runs in Main, and what comes back here is an access token and its
 * expiry, never a refresh token or a client secret.
 */
import { redeemMicrosoftLogin, startMicrosoftLogin } from '@/shared/api/generated/configurations/configurations';
import type { MicrosoftLoginRedeemed, MicrosoftLoginStart } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';

export type { MicrosoftLoginRedeemed, MicrosoftLoginStart };

interface StartSignInParams {
  readonly projectId: number;
  /** The uuid of the saved credential. */
  readonly credentialId: string;
}

/** Start a web sign-in: the popup redeems with the session, so it sends no client challenge. */
export async function startSignIn({ projectId, credentialId }: StartSignInParams): Promise<MicrosoftLoginStart> {
  const response = await startMicrosoftLogin({ project_id: projectId, credential_id: credentialId, client_kind: 'web' });
  return unwrapBody(response) as MicrosoftLoginStart;
}

interface RedeemSignInParams {
  readonly projectId: number;
  readonly handoff: string;
  readonly state: string;
}

export async function redeemSignIn({ projectId, handoff, state }: RedeemSignInParams): Promise<MicrosoftLoginRedeemed> {
  const response = await redeemMicrosoftLogin({ project_id: projectId, handoff, state });
  return unwrapBody(response) as MicrosoftLoginRedeemed;
}

/**
 * The stable `code` of a refused call, read off the error the transport threw
 * (`{error, code}` body), or `undefined` for anything else (network, a 5xx without a body).
 */
export function refusalCode(caught: unknown): string | undefined {
  if (typeof caught !== 'object' || caught === null || !('failure' in caught)) return undefined;
  const failure = (caught as { readonly failure?: unknown }).failure;
  if (typeof failure !== 'object' || failure === null) return undefined;
  const body = (failure as { readonly body?: unknown }).body;
  if (typeof body !== 'object' || body === null) return undefined;
  const code = (body as { readonly code?: unknown }).code;
  return typeof code === 'string' && code !== '' ? code : undefined;
}
