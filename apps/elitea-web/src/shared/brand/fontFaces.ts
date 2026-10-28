/**
 * `@font-face` generation from `pack.typography.fontFaces` (ADR-0024 WP3).
 *
 * A pack's `typography.fontFamily` names a family, but before this module
 * nothing declared where it comes from: there was no `@font-face` and no font
 * file, so every deployment silently fell through to the next family in the
 * stack. A pack now lists its self-hosted faces and this module turns
 * them into the one stylesheet the provider injects (`<style data-el-fonts>`,
 * `app/providers/BrandDocumentHead.tsx`).
 *
 * Same-origin ONLY (§3.7, theme-gate check 6): a face whose `url` is not a
 * root-relative or document-relative PATH is dropped and logged, never
 * emitted. That is the whole of this app's font-origin policy, enforced at
 * the one point a URL becomes CSS, so a served pack cannot reintroduce a
 * remote font host by stating one. Protocol-relative (`//host/...`), absolute
 * (`https://...`), `data:` and anything carrying characters that would end
 * the CSS `url()` token early are all rejected by the same predicate.
 *
 * `font-display: swap` on every face: the text paints in the fallback family
 * at once and re-paints when the face arrives. A brand font must never hold
 * first paint hostage.
 */
import type { BrandFontFace, BrandPack } from './schema';

/** The attribute the injected `<style>` carries, so the DOM and the e2e suite can find it. */
export const FONT_FACE_STYLE_ATTRIBUTE = 'data-el-fonts';

const SAME_ORIGIN_PATH_RE = /^(?:\/(?!\/)|\.\/)[^\s"'()\\<>]*$/;
const FONT_WEIGHT_RE = /^(?:normal|bold|[1-9]\d{0,2}(?:\s+[1-9]\d{0,2})?)$/;

/** `true` for a root-relative (`/x`) or document-relative (`./x`) path with no `url()`-breaking characters. */
export function isSameOriginAssetPath(url: string): boolean {
  return SAME_ORIGIN_PATH_RE.test(url);
}

/** Strips everything that could end a CSS string early; a family is a name, not a selector. */
function cssStringLiteral(value: string): string {
  return `"${value.replace(/["\\]/g, '').replace(/\s+/g, ' ').trim()}"`;
}

/**
 * A `./`-relative face URL resolved against an ABSOLUTE build base. The main
 * SPA builds with base `./` and nginx serves `brand/` from any depth
 * (spa.conf), so it keeps the relative form. The admin console builds with
 * base `/admin/app/` and is served by Go with no such alias, so on a nested
 * console route `./brand/…` would resolve one directory too deep.
 */
export function resolveFontUrl(url: string, base: string = import.meta.env.BASE_URL): string {
  if (!url.startsWith('./') || !base.startsWith('/') || base === '/') return url;
  return `${base.endsWith('/') ? base : `${base}/`}${url.slice(2)}`;
}

/** One `@font-face` rule, or `undefined` when the face is refused. */
export function fontFaceRule(face: BrandFontFace, base?: string): string | undefined {
  if (!isSameOriginAssetPath(face.url)) return undefined;
  const declarations = [
    `font-family:${cssStringLiteral(face.family)}`,
    `src:url(${cssStringLiteral(resolveFontUrl(face.url, base))}) format("woff2")`,
    `font-display:swap`,
  ];
  if (face.weight !== undefined && FONT_WEIGHT_RE.test(face.weight.trim())) {
    declarations.push(`font-weight:${face.weight.trim()}`);
  }
  if (face.style !== undefined) declarations.push(`font-style:${face.style}`);
  return `@font-face{${declarations.join(';')};}`;
}

/**
 * The full stylesheet for a pack: one rule per accepted face, refused faces
 * logged once each. An empty string means "inject nothing", which is what
 * a pack with no `fontFaces` produces. The default pack declares the three
 * brand faces (brand guide v3: Manrope, Sora, JetBrains Mono).
 */
export function fontFaceStylesheet(pack: Pick<BrandPack, 'typography'>, base?: string): string {
  const faces = pack.typography.fontFaces ?? [];
  const rules: string[] = [];
  for (const face of faces) {
    const rule = fontFaceRule(face, base);
    if (rule === undefined) {
      // Handled (§3.6): a refused face is a logged omission, not a broken
      // theme — the text keeps rendering in the fallback family.
      // oxlint-disable-next-line no-console -- deliberate boot-time diagnostic; a SILENT drop here would hide a pack that names a foreign font host.
      console.warn(`brand: fontFaces entry for "${face.family}" refused — url must be a same-origin path`, face.url);
      continue;
    }
    rules.push(rule);
  }
  return rules.join('\n');
}
