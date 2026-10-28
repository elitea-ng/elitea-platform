#!/usr/bin/env node
/**
 * Derives every committed copy of the Elitea artwork from the one canonical
 * source in this package:
 *
 *   src/brand/logo-mark.svg   the LE-03 mark (four flat layers, viewBox 0 0 372 372)
 *   src/brand/logo-full.svg   the lockup (the mark plus "Elitea AI" outlined in Sora 700)
 *   src/brand/tokens.json     the design tokens (brand guide v3)
 *
 * Written (all committed; edit the canonical source and re-run, never edit a
 * derived copy):
 *
 *   libs/ts/client-core/src/brand/mark.gen.ts            the mark as a string (mobile <EliteaMark>)
 *   libs/ts/client-core/src/brand/tokens.gen.ts          the tokens as typed data (web, desktop, mobile)
 *   apps/elitea-web/public/brand/logo-mark.svg           the default pack's assets.logoMark
 *   apps/elitea-web/public/brand/favicon.svg             the default pack's assets.favicon
 *   apps/elitea-web/public/brand/logo-full.svg           the default pack's assets.logoFull
 *   apps/elitea-web/src/shared/ui/icons/svg/logo-mark-icon.svg   <LogoMarkIcon>
 *   apps/elitea-web/src/shared/ui/icons/svg/logo-icon.svg        <LogoIcon>
 *
 * These are the product DEFAULTS. ADR-0024 runtime branding is unchanged: a
 * workspace brand pack replaces the artwork at run time.
 *
 * Usage:
 *   node libs/ts/client-core/scripts/gen-brand.mjs           write the copies
 *   node libs/ts/client-core/scripts/gen-brand.mjs --check   fail when one drifted
 */
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const PACKAGE_DIR = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const REPO_DIR = resolve(PACKAGE_DIR, '../../..');

const MARK_SOURCE = join(PACKAGE_DIR, 'src/brand/logo-mark.svg');
const FULL_SOURCE = join(PACKAGE_DIR, 'src/brand/logo-full.svg');
const TOKENS_SOURCE = join(PACKAGE_DIR, 'src/brand/tokens.json');

const SVG_OPEN = /^<svg\b[^>]*>/;

/** The mark's root tag, which must carry exactly this viewBox and no size. */
const MARK_VIEWBOX = '0 0 372 372';

/** The app-icon tile behind the favicon: Ink, on the icon pack's 512 grid, corner radius 114. */
const TILE = '<rect width="512" height="512" rx="114" fill="#0B0B18"/>';

/**
 * The LE-03 icon pack's four variants (brand/assets/icon-pack/README.md):
 * the tile colour, and the mark colour, or null to keep the mark's own colours.
 */
const ICON_VARIANTS = [
  { name: 'colored-light', bg: '#FFFFFF', mark: null },
  { name: 'colored-dark', bg: '#0B0B18', mark: null },
  { name: 'bw', bg: '#FFFFFF', mark: '#000000' },
  { name: 'inverted', bg: '#0B0B18', mark: '#FFFFFF' },
];

/**
 * The icon pack's vector sources, one set per variant, measured off the
 * approved PNGs: the 372-unit mark sits at inset 70 on a 512 grid (macOS
 * rounds that tile at rx 114, every other square format is full bleed), the
 * Android adaptive foreground carries the mark 266.67 wide, centred on a
 * transparent 432 canvas, and the social card centres
 * a 320 mark on 1280x640. brand/scripts/render-icon-pack.mjs rasterises them.
 */
function iconSources(markBody) {
  const svg = (w, h, inner) => `<svg viewBox="0 0 ${w} ${h}" width="${w}" height="${h}" fill="none" xmlns="http://www.w3.org/2000/svg">${inner}</svg>\n`;
  const out = [];
  for (const v of ICON_VARIANTS) {
    const body = v.mark === null ? markBody : markBody.replace(/fill="#[0-9A-Fa-f]{6}"/g, `fill="${v.mark}"`);
    const n = (x) => +x.toFixed(4);
    const at = (x, y, size) => `<g transform="translate(${n(x)} ${n(y)}) scale(${n(size / 372)})">${body}</g>`;
    const dir = `brand/assets/icon-pack/src/${v.name}`;
    const file = (kind) => `${dir}/elitea-${v.name}-${kind}.svg`;
    out.push(
      { path: file('square'), content: svg(512, 512, `<rect width="512" height="512" fill="${v.bg}"/>${at(70, 70, 372)}`) },
      { path: file('macos'), content: svg(512, 512, `<rect width="512" height="512" rx="114" fill="${v.bg}"/>${at(70, 70, 372)}`) },
      { path: file('android-foreground'), content: svg(432, 432, at(248 / 3, 248 / 3, 800 / 3)) },
      { path: file('android-background'), content: svg(432, 432, `<rect width="432" height="432" fill="${v.bg}"/>`) },
      { path: file('social'), content: svg(1280, 640, `<rect width="1280" height="640" fill="${v.bg}"/>${at(480, 160, 320)}`) },
    );
  }
  return out;
}

/**
 * Renders every derived copy from the canonical artwork.
 *
 * @param {string} markSvg the canonical mark (one line, no width/height)
 * @param {string} fullSvg the canonical wordmark
 * @returns {Array<{path: string, content: string}>} paths relative to the repository root
 */
export function renderBrandArtifacts(markSvg, fullSvg, tokensJson = '{}') {
  const mark = markSvg.trim();
  const open = SVG_OPEN.exec(mark)?.[0];
  if (!open) throw new Error('logo-mark.svg: no <svg> root tag');
  const head = `<svg viewBox="${MARK_VIEWBOX}"`;
  if (!open.startsWith(head)) throw new Error(`logo-mark.svg: the root tag must start with ${head}`);
  if (/\s(width|height)=/.test(open)) throw new Error('logo-mark.svg: the canonical mark carries no width or height; each copy sets its own');
  if (/[\n'\\]/.test(mark)) throw new Error('logo-mark.svg: keep the canonical mark on one line, with no single quote or backslash');
  const body = mark.slice(open.length);
  const sized = (attrs) => `${head} ${attrs}${open.slice(head.length)}${body}`;
  // The favicon is the app-icon tile: the mark on Ink, inset 70 units on the
  // icon pack's 512 grid, so it reads on light and dark browser chrome alike.
  const favicon = `<svg viewBox="0 0 512 512" width="512" height="512" fill="none" xmlns="http://www.w3.org/2000/svg">${TILE}<g transform="translate(70 70)">${body.replace(/<\/svg>$/, '')}</g></svg>`;
  const webMark = `${sized('width="100%" height="100%"')}\n`;
  const [w, h] = MARK_VIEWBOX.split(' ').slice(2).map(Number);
  const markTs = `/**
 * GENERATED by libs/ts/client-core/scripts/gen-brand.mjs from src/brand/logo-mark.svg.
 * Do not edit: change the canonical SVG and re-run the generator.
 *
 * The Elitea logo mark as markup, without a size (the renderer sets it).
 * It is the product default; a workspace brand pack replaces it at run time.
 */
export const ELITEA_MARK_SVG =
  '${mark}';

/** The mark's viewBox. */
export const ELITEA_MARK_VIEWBOX = '${MARK_VIEWBOX}';

/** Width over height of the mark (${w} x ${h}). */
export const ELITEA_MARK_ASPECT = ${w} / ${h};
`;
  const full = fullSvg.endsWith('\n') ? fullSvg : `${fullSvg}\n`;
  const lockupIn = (color) => (color === undefined ? full : full.replace(/currentColor/g, color));
  const tokens = JSON.parse(tokensJson);
  delete tokens.$comment;
  const tokensTs = `/**
 * GENERATED by libs/ts/client-core/scripts/gen-brand.mjs from src/brand/tokens.json.
 * Do not edit: change the JSON and re-run the generator.
 *
 * The Elitea AI design tokens (brand guide v3, https://elitea-ng.github.io/elitea-platform/brand/).
 * They are the product defaults; a workspace brand pack (ADR-0024) replaces them at run time.
 */
export const BRAND_TOKENS = ${JSON.stringify(tokens, null, 2)} as const;
`;
  return [
    { path: 'libs/ts/client-core/src/brand/mark.gen.ts', content: markTs },
    { path: 'libs/ts/client-core/src/brand/tokens.gen.ts', content: tokensTs },
    { path: 'apps/elitea-web/public/brand/logo-mark.svg', content: webMark },
    { path: 'apps/elitea-web/public/brand/favicon.svg', content: `${favicon}\n` },
    { path: 'apps/elitea-web/public/brand/logo-full.svg', content: full },
    { path: 'apps/elitea-web/src/shared/ui/icons/svg/logo-mark-icon.svg', content: webMark },
    { path: 'apps/elitea-web/src/shared/ui/icons/svg/logo-icon.svg', content: full },
    // Fixed-colour lockups for places that cannot set currentColor (an <img> in
    // a GitHub README or org profile): the wordmark in each scheme's text colour.
    { path: 'brand/assets/logo/elitea-lockup-on-dark.svg', content: lockupIn(tokens.schemes?.dark?.text) },
    { path: 'brand/assets/logo/elitea-lockup-on-light.svg', content: lockupIn(tokens.schemes?.light?.text) },
    ...iconSources(body.replace(/<\/svg>$/, '')),
  ];
}

function readOrNull(path) {
  try {
    return readFileSync(path, 'utf8');
  } catch {
    return null;
  }
}

function main(argv) {
  const check = argv.includes('--check');
  const unknown = argv.filter((arg) => arg !== '--check');
  if (unknown.length > 0) {
    console.error(`gen-brand: unknown argument(s): ${unknown.join(' ')}`);
    return 2;
  }
  const artifacts = renderBrandArtifacts(readFileSync(MARK_SOURCE, 'utf8'), readFileSync(FULL_SOURCE, 'utf8'), readFileSync(TOKENS_SOURCE, 'utf8'));
  if (artifacts.length === 0) {
    console.error('gen-brand: no artifact rendered, so the check has no subject');
    return 1;
  }
  if (!check) {
    for (const { path, content } of artifacts) {
      mkdirSync(dirname(join(REPO_DIR, path)), { recursive: true });
      writeFileSync(join(REPO_DIR, path), content, 'utf8');
      console.log(`  wrote ${path}`);
    }
    return 0;
  }
  const drifted = artifacts.filter(({ path, content }) => readOrNull(join(REPO_DIR, path)) !== content);
  if (drifted.length > 0) {
    console.error('gen-brand: --check FAIL. These copies differ from the canonical artwork:');
    for (const { path } of drifted) console.error(`  ${path}`);
    console.error(`  command: node ${relative(process.cwd(), fileURLToPath(import.meta.url))}`);
    return 1;
  }
  console.log(`gen-brand: --check OK: ${artifacts.length} copies match ${relative(REPO_DIR, MARK_SOURCE)}, ${relative(REPO_DIR, FULL_SOURCE)} and ${relative(REPO_DIR, TOKENS_SOURCE)}.`);
  return 0;
}

// Not `import.meta.main`: that needs Node 24.2, and `engines` admits any 24.
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) process.exitCode = main(process.argv.slice(2));
