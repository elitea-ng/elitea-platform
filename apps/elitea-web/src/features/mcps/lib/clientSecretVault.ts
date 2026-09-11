/**
 * In-memory holder for OAuth `client_secret` values (issue #177).
 *
 * ## Why a client secret must not be persisted at all
 *
 * `storage.ts` used to write the `client_secret` into `el.mcp.credentials`
 * and into each `el.mcp.tokens` record. That is strictly worse than storing
 * an access token: a token expires, a client secret generally does not, and
 * it authenticates the APPLICATION rather than one session.
 *
 * ## Why this module holds it instead of encrypting it
 *
 * Client-side encryption of a secret the client also holds is theatre. An
 * attacker with script execution reads the key out of the bundle (option 1)
 * or calls the decrypt oracle the page itself provides (option 2), so the
 * ciphertext buys nothing while reading as protection. That attacker is not
 * hypothetical here: a critical admin-page XSS (`go/unsafe-quoting`) was
 * fixed in this repo.
 *
 * So the secret is never written to a Storage area. It lives in this map,
 * whose lifetime is the lifetime of the document:
 *
 *  - a reload and a new tab each start with an empty map, and the navigation
 *    `performLogout()` makes ends the document that holds one;
 *  - it is not a Storage key, so the raw-key class of leak this repo has hit
 *    before (a key outside `el.*` that `clearNamespace()` cannot sweep) does
 *    not apply — moving a secret from one Storage key to another would NOT
 *    have been a fix, and this is not that;
 *  - `storage.ts`'s `logout()` and `removeSavedCredentials()` forget it
 *    eagerly, so signing one MCP server out does not leave its secret behind
 *    in a document that keeps running.
 *
 * ## Reload ownership
 *
 * Main owns newly registered confidential clients and returns an opaque
 * reference. Refresh uses that reference after reload, without this map.
 * Stored toolkit clients also resolve their secret server-side. Only legacy
 * or manually supplied secrets depend on this document's lifetime; those
 * flows may require authorization again after reload. Never persist a secret
 * to work around that limitation or substitute another client's secret.
 */

/**
 * Which record the secret belongs to. The two are separate on purpose:
 * `credential` is the value the user asked to save for the auth modal to
 * prefill, `token` is the value a grant was made with (possibly issued by
 * Dynamic Client Registration). They were separate Storage records before
 * this module existed, and one must not overwrite the other.
 */
import type { SetAccessTokenOAuthMeta, StoredMcpToken } from './types';

export type ClientSecretScope = 'credential' | 'token';

/** Any record shape that used to carry a persisted `client_secret`. */
export interface ClientSecretBearer {
  client_secret?: string | undefined;
}

const secrets = new Map<string, string>();

function vaultKey(scope: ClientSecretScope, storageKey: string): string {
  return `${scope}:${storageKey}`;
}

/** Holds `secret` for this scope/key; an empty or absent value forgets instead. */
export function rememberClientSecret(scope: ClientSecretScope, storageKey: string, secret: string | null | undefined): void {
  if (!secret) {
    secrets.delete(vaultKey(scope, storageKey));
    return;
  }
  secrets.set(vaultKey(scope, storageKey), secret);
}

function recallClientSecret(scope: ClientSecretScope, storageKey: string): string | undefined {
  return secrets.get(vaultKey(scope, storageKey));
}

export function forgetClientSecret(scope: ClientSecretScope, storageKey: string): void {
  secrets.delete(vaultKey(scope, storageKey));
}

/** A new client must not inherit another client's reference or document-held secret. */
export function rememberGrantClient(storageKey: string, metadata: SetAccessTokenOAuthMeta, existing: Partial<StoredMcpToken>): string | undefined {
  const changedClient = metadata.client_id !== undefined && metadata.client_id !== existing.client_id;
  const reference = metadata.client_reference ?? (changedClient || metadata.used_dcr === false ? undefined : existing.client_reference);
  const secret = reference ? undefined : metadata.client_secret ?? (changedClient ? undefined : recallClientSecret('token', storageKey));
  rememberClientSecret('token', storageKey, secret);
  return reference;
}

/**
 * Reunites a record read back from Storage with its held secret, so every
 * reader of `getTokenInfo()`/`getSavedCredentials()` keeps the shape it had.
 * A record whose secret this document never held comes back unchanged — which
 * is exactly what a reader gets after a reload.
 */
export function withRecalledSecret<T extends ClientSecretBearer>(scope: ClientSecretScope, storageKey: string, record: T | null): T | null {
  if (record === null) return null;
  const secret = recallClientSecret(scope, storageKey);
  return secret === undefined ? record : { ...record, client_secret: secret };
}

/**
 * Removes `client_secret` from every record on its way to Storage.
 *
 * This is the structural half of the fix and the reason it cannot regress
 * quietly: `storage.ts` writes tokens and credentials through exactly two
 * functions, both of which call this one, so no future field-copying code
 * path can put a secret back on disk by accident.
 */
export function stripClientSecrets<T extends ClientSecretBearer>(records: Record<string, T>): Record<string, T> {
  const safe: Record<string, T> = {};
  for (const [key, record] of Object.entries(records)) {
    if (record.client_secret === undefined) {
      safe[key] = record;
      continue;
    }
    const copy = { ...record };
    delete copy.client_secret;
    safe[key] = copy;
  }
  return safe;
}
