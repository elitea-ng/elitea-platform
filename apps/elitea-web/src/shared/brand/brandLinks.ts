/**
 * Brand-derived outbound links (ADR-0024 decision 8, WP8).
 *
 * Every documentation URL and support address the app shows a user comes
 * from here, so a tenant pack that states `product.docsUrl`,
 * `product.supportUrl` or `product.supportEmail` re-points all of them with
 * no rebuild. The resolution order is the same for each field:
 *
 *   1. the served pack (channel C, `resolveBrandPack()`), when it states it;
 *   2. the compiled default pack (channel A, the build's brand), when it
 *      states it;
 *   3. for the docs origin only, the literal the app shipped with before the
 *      pack carried the field. A support address has no literal (ADR-0052
 *      W3.5): with neither pack stating one, the app shows no support link,
 *      never another brand's mailbox.
 *
 * The docs literal may appear only here: the theme gate
 * (`scripts/theme-gate.mjs` check 8) fails any sub-application screen that
 * writes `docs.elitea.ai` itself.
 *
 * Pure. Every helper takes the pack as an optional argument so a caller that
 * already holds one (the provider tree, a `useMemo`) does not pay for a
 * second `resolveBrandPack()` — which trial-builds a theme when channel C is
 * populated — and so tests can pass a pack without touching `window`.
 */
import { resolveBrandPack } from './channelC';
import type { BrandPack } from './schema';
import { DEFAULT_BRAND_PACK } from './tokens';

/**
 * The pre-pack documentation origin. Fallback 3 for `docsLink`, reachable
 * only when BOTH the served pack and the compiled default omit
 * `product.docsUrl` outright — the compiled default always states it
 * (embedded-docs programme), so this is a defensive floor, not a path any
 * shipped configuration takes today. Module-private: no test imports it
 * directly any more (brandLinks.test.ts exercises this tier by constructing
 * a pack object with no `docsUrl` field, not by asserting the literal), so
 * an `export` here has no consumer to serve (knip's dead-code gate, #528).
 */
const FALLBACK_DOCS_URL = 'https://docs.elitea.ai';

function trimSlashes(value: string, side: 'leading' | 'trailing'): string {
  return side === 'leading' ? value.replace(/^\/+/, '') : value.replace(/\/+$/, '');
}

/** The documentation origin, without a trailing slash. */
export function docsBaseUrl(pack: BrandPack = resolveBrandPack()): string {
  const base = pack.product.docsUrl ?? DEFAULT_BRAND_PACK.product.docsUrl ?? FALLBACK_DOCS_URL;
  return trimSlashes(base, 'trailing');
}

/**
 * A documentation page under the brand's docs origin. `pathSuffix` is the
 * page path (`integrations/apps/wikis`); leading slashes are tolerated and
 * an empty suffix returns the origin itself.
 */
export function docsLink(pathSuffix: string, pack: BrandPack = resolveBrandPack()): string {
  const suffix = trimSlashes(pathSuffix, 'leading');
  const base = docsBaseUrl(pack);
  return suffix.length === 0 ? base : `${base}/${suffix}`;
}

/** The support mailbox the app names to a user; `undefined` when no pack states one (show no link). */
export function supportEmail(pack: BrandPack = resolveBrandPack()): string | undefined {
  return pack.product.supportEmail ?? DEFAULT_BRAND_PACK.product.supportEmail;
}

/**
 * The support destination as a link target: the pack's `supportUrl` when it
 * states one, else a `mailto:` to `supportEmail()`, else `undefined` (show no
 * link).
 *
 * @public Wave-1 surface — the help-center and request-access flows link to it once they carry a contact action.
 */
export function supportUrl(pack: BrandPack = resolveBrandPack()): string | undefined {
  const url = pack.product.supportUrl ?? DEFAULT_BRAND_PACK.product.supportUrl;
  if (url !== undefined) return url;
  const email = supportEmail(pack);
  return email === undefined ? undefined : `mailto:${email}`;
}
