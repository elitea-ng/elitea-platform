/**
 * The caller's interface-language preference, stored server-side so the web,
 * desktop and mobile clients show the same language for the same user
 * (`GET`/`PUT /social/author/language`,
 * services/elitea-main/internal/api/v2/social/language.go).
 *
 * `localStorage['el.lang']` (`shared/i18n/locales.ts`) stays the boot and
 * pre-auth cache: the language is applied before any request can answer
 * (`app/applyStartLocale.ts`). Once signed in, `app/LanguageSync` reads the
 * server value and, when it differs from the language on screen, saves it
 * locally and reloads — the server wins.
 *
 * A choice the server has not confirmed is UNSYNCED, kept as `el.lang.unsynced`
 * from the moment it is made until its PUT is answered (or refused for good,
 * a 4xx). While it is set, the server's value is stale: the sync pushes the
 * choice again instead of applying the read. Both calls are `background`
 * requests, so a 401 on them does not open the re-auth window, and neither
 * throws: a language that did not sync is not worth a toast.
 *
 * Calls `eliteaFetch` directly for the `background` flag, which the generated
 * functions cannot pass. Loaded only through a dynamic `import()`.
 */
import { isLocale, type Locale } from '@/shared/i18n';
import { createStorage } from '@/shared/lib/storage';

import { EliteaApiError, eliteaFetch } from './generated/mutator';
import { getGetCurrentAuthorLanguageUrl } from './generated/social/social';

const UNSYNCED_KEY = 'lang.unsynced';

/**
 * The stored tag, `null` when the user has never chosen one, or `undefined`
 * when the read failed (the caller then leaves the local choice alone).
 */
export async function fetchLanguagePreference(): Promise<string | null | undefined> {
  try {
    const { data } = await eliteaFetch<{ data: { language?: unknown } }>(
      getGetCurrentAuthorLanguageUrl(),
      { method: 'GET' },
      { background: true },
    );
    if (data.language === null) return null;
    return typeof data.language === 'string' ? data.language : undefined;
  } catch {
    return undefined;
  }
}

/** The choice no PUT has confirmed yet, if any. */
export function unsyncedLanguage(): Locale | undefined {
  try {
    const value = createStorage('local').get(UNSYNCED_KEY);
    return isLocale(value) ? value : undefined;
  } catch {
    return undefined;
  }
}

function writeUnsynced(locale: Locale | undefined): void {
  try {
    const storage = createStorage('local');
    if (locale === undefined) storage.remove(UNSYNCED_KEY);
    else storage.set(UNSYNCED_KEY, locale);
  } catch {
    // Storage refused: the choice is not retried after the reload.
  }
}

/** A 4xx other than 408 / 429 will not change on a retry. */
function isPermanent(error: unknown): boolean {
  if (!(error instanceof EliteaApiError)) return false;
  const { failure } = error;
  return (
    (failure.kind === 'http' || failure.kind === 'auth') &&
    failure.status >= 400 &&
    failure.status < 500 &&
    failure.status !== 408 &&
    failure.status !== 429
  );
}

/**
 * Stores `locale`; resolves `true` once the server has it. The choice is
 * marked unsynced first, and the mark is cleared by the server's answer or a
 * permanent refusal (so the server's value applies again then).
 */
export async function saveLanguagePreference(locale: Locale): Promise<boolean> {
  writeUnsynced(locale);
  try {
    await eliteaFetch(
      getGetCurrentAuthorLanguageUrl(),
      { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ language: locale }) },
      { background: true },
    );
    if (unsyncedLanguage() === locale) writeUnsynced(undefined);
    return true;
  } catch (error) {
    if (isPermanent(error) && unsyncedLanguage() === locale) writeUnsynced(undefined);
    return false;
  }
}

export type LanguageSyncAction =
  | { readonly kind: 'none' }
  /** Make `locale` the language: save it locally and reload. */
  | { readonly kind: 'apply'; readonly locale: Locale }
  /** Send `locale` to the server. */
  | { readonly kind: 'push'; readonly locale: Locale };

export interface LanguageSyncInput {
  /** What the read answered: a tag, `null` (never chosen) or `undefined` (failed). */
  readonly server: string | null | undefined;
  /** The language on screen. */
  readonly current: Locale;
  /** The person's own saved choice on this device, if any. */
  readonly stored: Locale | undefined;
  /** A choice the server has not confirmed. */
  readonly unsynced: Locale | undefined;
}

/**
 * What to do once the server has answered. The server wins over the local
 * cache when it has a value this client ships; an unconfirmed local choice
 * and a never-chosen server value push the local choice up instead.
 */
export function decideLanguageSync({ server, current, stored, unsynced }: LanguageSyncInput): LanguageSyncAction {
  if (unsynced !== undefined) return { kind: 'push', locale: unsynced };
  if (server === undefined) return { kind: 'none' };
  if (server === null) return stored !== undefined ? { kind: 'push', locale: stored } : { kind: 'none' };
  // A tag this build has no catalogue for (another client's language): keep the local one.
  if (!isLocale(server)) return { kind: 'none' };
  return server === current ? { kind: 'none' } : { kind: 'apply', locale: server };
}
