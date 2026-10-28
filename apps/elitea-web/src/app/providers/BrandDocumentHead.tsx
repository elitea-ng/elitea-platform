import { useEffect, useInsertionEffect } from 'react';

import { type BrandPack, FONT_FACE_STYLE_ATTRIBUTE, fontFaceStylesheet, resolveBrandAsset } from '@/shared/brand';

import { getAppBasename } from './basename';

export interface BrandDocumentHeadProps {
  readonly pack: BrandPack;
}

/**
 * The `<head>` half of the brand pack (ADR-0024 WP3). Renders nothing.
 *
 *  - `@font-face`: `pack.typography.fontFaces` becomes ONE
 *    `<style data-el-fonts>` element, written in an insertion effect so the
 *    rules exist before the theme's own stylesheets are laid out. Idempotent:
 *    the element is found by attribute, never held in module state (R-S2),
 *    and removed when the pack declares no face.
 *  - favicon: when the served pack states a custom `assets.favicon`
 *    (`shared/brand/assets.ts`), the `<link rel="icon">` index.html ships is
 *    repointed at it — or created, for a document that had none. The default
 *    pack leaves the static tag untouched, so a no-pack deployment keeps the
 *    compiled mark.
 *
 * Both effects key on the pack object, which `AppProviders` holds in state
 * for the document's lifetime, so they run once per mount in practice.
 */
export function BrandDocumentHead({ pack }: BrandDocumentHeadProps): null {
  useInsertionEffect(() => {
    // One cacheable URL per face: `./brand/fonts/…` resolved against the
    // app's absolute basename (`/app/`) instead of each deep route's
    // directory. Without an absolute basename (admin, desktop) the build
    // base decides (shared/brand/fontFaces.ts).
    const basename = getAppBasename();
    const css = fontFaceStylesheet(pack, basename.startsWith('/') && basename !== '/' ? basename : undefined);
    const existing = document.head.querySelector<HTMLStyleElement>(`style[${FONT_FACE_STYLE_ATTRIBUTE}]`);
    if (css === '') {
      existing?.remove();
      return;
    }
    const style = existing ?? document.createElement('style');
    style.setAttribute(FONT_FACE_STYLE_ATTRIBUTE, '');
    if (style.textContent !== css) style.textContent = css;
    if (existing === null) document.head.append(style);
  }, [pack]);

  useEffect(() => {
    const favicon = resolveBrandAsset('favicon', pack);
    if (!favicon.custom || favicon.url === undefined) return;
    const link =
      document.head.querySelector<HTMLLinkElement>('link[rel="icon"]') ??
      document.head.appendChild(Object.assign(document.createElement('link'), { rel: 'icon' }));
    if (link.getAttribute('href') !== favicon.url) link.setAttribute('href', favicon.url);
    // The static tag declares `type="image/svg+xml"`; a custom favicon may be
    // any format, and browsers sniff correctly when no type is stated.
    link.removeAttribute('type');
  }, [pack]);

  return null;
}
