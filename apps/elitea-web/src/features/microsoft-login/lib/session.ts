/**
 * What the credential form shows about a finished sign-in: the account, the tenant and
 * when the access token ends. Display facts only. The access token itself is kept by the
 * caller in the store runs read (`features/mcps`' token store, keyed by credential and
 * authority); this file never holds it.
 *
 * Kept in session storage through the one sanctioned wrapper, so a logout sweeps it with
 * the rest of the `el.` namespace.
 */
import { createStorage } from '@/shared/lib/storage';

import type { MicrosoftLoginRedeemed } from '../api/microsoftLoginApi';

const STORAGE_KEY = 'microsoft-login.sessions';

export interface LoginSession {
  readonly account: string;
  readonly tenant: string;
  /** ISO time the access token ends. */
  readonly expiresAt: string;
  /** `custom`, or the Default app id that signed in. */
  readonly app: string;
  /** The key the access token is stored under, so a reloaded form finds it again ('' when none could be named). */
  readonly tokenKey: string;
}

type SessionMap = Readonly<Record<string, LoginSession>>;

function isSession(value: unknown): value is LoginSession {
  if (typeof value !== 'object' || value === null) return false;
  const record = value as Readonly<Record<string, unknown>>;
  return ['account', 'tenant', 'expiresAt', 'app', 'tokenKey'].every((key) => typeof record[key] === 'string');
}

function readAll(): SessionMap {
  try {
    const raw = createStorage('session').getJSON<unknown>(STORAGE_KEY);
    if (typeof raw !== 'object' || raw === null) return {};
    return Object.fromEntries(Object.entries(raw).filter((entry): entry is [string, LoginSession] => isSession(entry[1])));
  } catch {
    return {};
  }
}

function writeAll(map: SessionMap): void {
  try {
    createStorage('session').setJSON(STORAGE_KEY, map);
  } catch {
    // Storage unavailable (a private window): the form shows the sign-in for this page view only.
  }
}

/** The remembered sign-in of a credential, unless its token has ended. */
export function readSession(credentialUuid: string | undefined, now: number = Date.now()): LoginSession | undefined {
  if (credentialUuid === undefined || credentialUuid === '') return undefined;
  const session = readAll()[credentialUuid];
  if (session === undefined) return undefined;
  return Date.parse(session.expiresAt) > now ? session : undefined;
}

export function sessionOf(redeemed: MicrosoftLoginRedeemed, tokenKey: string): LoginSession {
  return {
    account: redeemed.account_name,
    tenant: redeemed.tenant_id,
    expiresAt: redeemed.expires_at,
    app: redeemed.app,
    tokenKey,
  };
}

export function rememberSession(credentialUuid: string, session: LoginSession): void {
  writeAll({ ...readAll(), [credentialUuid]: session });
}

export function forgetSession(credentialUuid: string): void {
  const { [credentialUuid]: _removed, ...rest } = readAll();
  writeAll(rest);
}
