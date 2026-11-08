/**
 * Configured i18next instance (spec N3, §2.5, §9.3 unit S8).
 *
 * A single `en` bundle (`./en.json`), no HTTP backend, no language
 * detector: the old app never shipped a language switcher and N3 keeps
 * shipping *translations* out of scope. What IS in scope is the extraction
 * *discipline* this instance makes real — every user-visible string in JSX
 * must flow through `t()` (`./t.ts`) instead of being written inline — so
 * adding a second locale later is a config change here (a new `resources`
 * entry + a language selector somewhere in the app), not a second rewrite.
 *
 * `resources` is passed inline (no backend plugin), so `.init()` populates
 * the resource store synchronously even though it returns a `Promise` —
 * `t()` is safe to call the instant this module has evaluated, including
 * from other modules' top-level code. The `.catch()` below only guards
 * against a future regression (e.g. someone adding an async backend without
 * updating this comment), not today's real failure mode.
 */
import i18next from 'i18next';
import { initReactI18next } from 'react-i18next';

import { productName } from '@/shared/brand/productName';

import en from './en.json';

export const DEFAULT_LOCALE = 'en';
export const DEFAULT_NAMESPACE = 'translation';

/**
 * Missing-key policy: unlike the common "warn only in dev" convention, this
 * fires in every environment. `en.json` is expected to hold every key that
 * ships — R-T3's lint rule plus the extraction step that seeds this file
 * are what keep that true — so a missing key at runtime is a real defect
 * signal (a key that was never extracted, or a typo), not routine noise to
 * silence. It never blocks rendering: `t()` (`./t.ts`) still returns
 * `fallback`, so a missing key degrades to the fallback copy, never to a
 * blank string or the bare key.
 */
export function warnOnMissingKey(
  _languages: readonly string[],
  namespace: string,
  key: string,
  fallbackValue: string,
): void {
  console.warn(
    `[shared/i18n] missing key "${key}" in namespace "${namespace}" — add it to src/shared/i18n/en.json. Rendering fallback "${fallbackValue}" for now.`,
  );
}

/**
 * Exported so `i18n.test.ts` can exercise it directly, the same way
 * `warnOnMissingKey` above is exercised through `t()`'s RED case — this
 * repo's convention (see `.oxlintrc.json`'s note on the type-aware rules
 * that stay off) is to test defensive code, not delete it to dodge a
 * coverage gap.
 */
export function logInitFailure(error: unknown): void {
  console.error('[shared/i18n] i18next.init() rejected', error);
}

i18next
  .use(initReactI18next)
  .init({
    lng: DEFAULT_LOCALE,
    fallbackLng: DEFAULT_LOCALE,
    defaultNS: DEFAULT_NAMESPACE,
    ns: [DEFAULT_NAMESPACE],
    resources: {
      [DEFAULT_LOCALE]: { [DEFAULT_NAMESPACE]: en },
    },
    interpolation: {
      // React already escapes interpolated values when it renders text
      // nodes; double-escaping here would turn "AT&T" into "AT&amp;T".
      escapeValue: false,
      // ADR-0052 W3.2: the catalogues name the product as `{{product}}`, never
      // a literal, so a brand (build-time default pack) or a served pack
      // (channel C) renames it everywhere. Read once: the served pack is
      // loaded before the bundle (index.html) and does not change while the
      // page runs. A call that passes its own `product` wins.
      defaultVariables: { product: productName() },
    },
    returnEmptyString: false,
    saveMissing: true,
    missingKeyHandler: warnOnMissingKey,
  })
  .catch(logInitFailure);

/** The live, configured instance — `./I18nProvider.tsx` binds React's context to this. */
export const i18n = i18next;
