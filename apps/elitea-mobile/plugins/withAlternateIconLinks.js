// @ts-check
const { withAndroidManifest } = require('expo/config-plugins');

/**
 * Android: the alternate app icon ("Zefir Night", expo-alternate-app-icons) is an
 * `<activity-alias>` of `.MainActivity`. Switching to it DISABLES the `.MainActivity` component
 * and enables the alias, and a disabled component no longer receives intents. Without this
 * plugin the alias carries only MAIN/LAUNCHER, so with the workspace icon on, Android would
 * stop delivering `agentzefir://` connect links and the native sign-in redirect
 * (`ca.zefir.agent:/oauth/callback`): sign-in could not complete.
 *
 * This copies every non-launcher intent filter of `.MainActivity` (the VIEW / BROWSABLE scheme
 * filters) onto each alias that targets it, so the app answers the same links whichever icon is
 * on. Listed BEFORE expo-alternate-app-icons in app.config.ts: manifest mods run in reverse
 * registration order, so this one runs after the alias exists.
 */

/** @param {any} filter */
function isLauncher(filter) {
  const actions = (filter.action ?? []).map((/** @type {any} */ a) => a.$?.['android:name']);
  const categories = (filter.category ?? []).map((/** @type {any} */ c) => c.$?.['android:name']);
  return actions.includes('android.intent.action.MAIN') && categories.includes('android.intent.category.LAUNCHER');
}

/** @param {any} filter */
function key(filter) {
  return JSON.stringify(filter);
}

/**
 * Copies `.MainActivity`'s link filters onto its aliases (idempotent). Exported for the tests.
 * @param {any} manifest the parsed AndroidManifest (`config.modResults`)
 */
function copyLinkFiltersToAliases(manifest) {
  const application = manifest?.manifest?.application?.[0];
  if (!application) return manifest;
  const main = (application.activity ?? []).find((/** @type {any} */ a) => a.$?.['android:name'] === '.MainActivity');
  if (!main) return manifest;
  const links = (main['intent-filter'] ?? []).filter((/** @type {any} */ f) => !isLauncher(f));
  for (const alias of application['activity-alias'] ?? []) {
    if (alias.$?.['android:targetActivity'] !== '.MainActivity') continue;
    const filters = alias['intent-filter'] ?? [];
    const have = new Set(filters.map(key));
    for (const link of links) if (!have.has(key(link))) filters.push(JSON.parse(JSON.stringify(link)));
    alias['intent-filter'] = filters;
  }
  return manifest;
}

/** @type {import('expo/config-plugins').ConfigPlugin} */
const withAlternateIconLinks = (config) =>
  withAndroidManifest(config, (next) => {
    next.modResults = copyLinkFiltersToAliases(next.modResults);
    return next;
  });

module.exports = withAlternateIconLinks;
module.exports.copyLinkFiltersToAliases = copyLinkFiltersToAliases;
