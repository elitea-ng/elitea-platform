/**
 * Applies the signed-in user's server-stored language
 * (`shared/api/languagePreference`) so the web, desktop and mobile clients
 * agree. Mounted once a session user is known, keyed on that user.
 *
 * The language is applied before the app's modules load
 * (`./applyStartLocale`) from the `el.lang` cache, so there is no flash for a
 * returning user. This runs after sign-in: when the server's value differs
 * from the language on screen the server wins — it is saved into the cache
 * and the page reloads (`t()` is not reactive, `shared/i18n/locales.ts`). A
 * user with no server value but a local choice has the choice seeded up; an
 * unconfirmed local choice is pushed again instead of being overwritten.
 *
 * Lazy-loaded by `App`, together with the API module.
 */
import { useEffect } from 'react';

import { decideLanguageSync, fetchLanguagePreference, saveLanguagePreference, unsyncedLanguage } from '@/shared/api/languagePreference';
import { currentLocale, saveLocale, storedLocale } from '@/shared/i18n';

export interface LanguageSyncProps {
  /** Reloads the page (tests pass a spy). */
  readonly reload?: () => void;
}

function reloadPage(): void {
  window.location.reload();
}

export default function LanguageSync({ reload = reloadPage }: LanguageSyncProps): null {
  useEffect(() => {
    let disposed = false;
    void (async () => {
      const server = await fetchLanguagePreference();
      if (disposed) return;
      const action = decideLanguageSync({ server, current: currentLocale(), stored: storedLocale(), unsynced: unsyncedLanguage() });
      if (action.kind === 'push') {
        await saveLanguagePreference(action.locale);
      } else if (action.kind === 'apply') {
        saveLocale(action.locale);
        reload();
      }
    })();
    return () => {
      disposed = true;
    };
  }, [reload]);

  return null;
}
