/**
 * Public surface of the i18n shim (spec N3, §3.6/R-T3, §2.5, §9.3 unit S8).
 * See `./README.md` for the extraction convention Wave-2 units follow.
 *
 * `t(key, fallback)` is the ONLY way user-visible copy may appear in JSX
 * (R-T3, enforced live by `i18next/no-literal-string` in `.oxlintrc.json`),
 * and since issue #45 this barrel is the ONLY module that exports it — the
 * pre-S8 interim stub `src/shared/ui/lib/t.ts` was migrated away and
 * deleted. Do not narrow the two-required-argument shape; widening (as this
 * module already does with the optional third `options` argument) is safe.
 */
export { t } from './t';
/** @public Wave-1 surface — the type call sites annotate a passed-through `t` with (e.g. a component prop). */
export type { TFunction } from './t';

/** @public Wave-1 surface — R2's provider composition wraps the app tree in this. */
export { I18nProvider } from './I18nProvider';

/**
 * @public Wave-1 surface — escape hatch for `i18n.language` / `i18n.changeLanguage` /
 * `i18n.on('missingKey', …)` in tests. Call `.t()` on it directly only if you
 * specifically need to bypass the `fallback`-required contract; everywhere
 * else, use `t()` above.
 */
export { i18n } from './i18n';

/** The shipped languages and switching between them (`./locales.ts`). */
export { LOCALES, addCatalogues, applyLocale, currentLocale, registerCatalogueLoader, saveLocale, storedLocale } from './locales';
export type { Catalogue, Locale, TranslatedLocale } from './locales';
