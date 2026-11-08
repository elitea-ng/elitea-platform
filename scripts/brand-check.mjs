#!/usr/bin/env node
// brand-check: validate one or more brand folders (ADR-0052 decisions 2 and 11).
//
//   node scripts/brand-check.mjs                     the brand ELITEA_BRAND selects (default brand/elitea)
//   node scripts/brand-check.mjs brand/elitea brand/sample
//   node scripts/brand-check.mjs --json brand/sample  machine-readable report
//
// For each folder it checks:
//   - the manifest (scripts/brand/schema.mjs): ids, schemes, URLs, host names, relations;
//   - every asset of the asset table: present, the right type, every size,
//     alpha where the platform needs it (or forbids it);
//   - the contrast of brand.hue on the light and dark backgrounds, and of its label;
//   - only the files the format names (brand.json, assets/, pack.json, firebase/, store/, README.md);
//   - for any brand other than `elitea`: no asset byte-identical to brand/elitea's,
//     and no name, id, scheme, domain or URL that brand/elitea uses.
// With two or more folders it also checks that no two share an id or identifier.
//
// The report marks the permanent fields (bundle id, App Group, desktop
// identifier). Exit 0 when every brand passes, 1 when one fails, 2 on usage.
// Node's standard library only.

import { createHash } from 'node:crypto';
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { icnsSizes, icoSizes, isSvg, pngInfo } from './brand/images.mjs';
import { BRAND_ENV, BRANDS_DIR, BrandError, DEFAULT_BRAND_ID, loadBrandFrom, REPO_ROOT, resolveBrandDir } from './brand/load.mjs';
import { ASSETS, contrastFindings, fieldValue, FIREBASE_FILES, OPTIONAL_ENTRIES, PERMANENT_FIELDS } from './brand/schema.mjs';

/** Manifest fields a non-ELITEA brand may not share with brand/elitea. */
export const DISTINCT_FIELDS = Object.freeze([
  'productName',
  'shortName',
  'mobile.bundleId',
  'mobile.oauthRedirectScheme',
  'mobile.appGroup',
  'mobile.widgetBundleId',
  'desktop.identifier',
  'linkDomain',
  'webUrl',
  'privacyUrl',
  'supportUrl',
  'supportEmail',
]);

/** Identifier fields that must be unique across every brand checked together. */
const CROSS_BRAND_IDS = ['id', 'mobile.bundleId', 'mobile.appGroup', 'mobile.widgetBundleId', 'desktop.identifier'];

const sha256 = (buf) => createHash('sha256').update(buf).digest('hex');

function listFiles(dir, base = dir) {
  if (!existsSync(dir)) return [];
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => (e.isDirectory() ? listFiles(join(dir, e.name), base) : [relative(base, join(dir, e.name)).split('\\').join('/')]));
}

/** Problems with one asset of the table, given its bytes. */
export function checkAsset(spec, buf) {
  const out = [];
  const where = `assets/${spec.path}`;
  if (spec.kind === 'png') {
    const info = pngInfo(buf);
    if (!info) return [`${where}: not a PNG`];
    if (info.width !== spec.size || info.height !== spec.size) out.push(`${where}: is ${info.width}x${info.height}, must be ${spec.size}x${spec.size} (${spec.use})`);
    if (spec.alpha === true && !info.alpha) out.push(`${where}: needs an alpha channel (${spec.use})`);
    if (spec.alpha === false && info.alpha) out.push(`${where}: must be opaque, with no alpha channel (${spec.use})`);
  } else if (spec.kind === 'ico' || spec.kind === 'icns') {
    const sizes = spec.kind === 'ico' ? icoSizes(buf) : icnsSizes(buf);
    if (!sizes) return [`${where}: not a valid .${spec.kind} file`];
    const missing = spec.sizes.filter((s) => !sizes.includes(s));
    if (missing.length) out.push(`${where}: missing size(s) ${missing.join(', ')} (has ${sizes.map((s) => (typeof s === 'number' ? s : `${s.width}x${s.height}`)).join(', ')})`);
  } else if (spec.kind === 'svg') {
    if (!isSvg(buf.toString('utf8'))) out.push(`${where}: not an SVG document with a viewBox`);
  } else if (spec.kind === 'json') {
    try {
      JSON.parse(buf.toString('utf8'));
    } catch {
      out.push(`${where}: not valid JSON`);
    }
  }
  return out;
}

/** The files and folders at the top of a brand folder that the format does not name. */
function strayEntries(dir) {
  const known = new Set(['brand.json', 'assets', ...OPTIONAL_ENTRIES]);
  return readdirSync(dir).filter((name) => !known.has(name) && !IGNORED.has(name));
}

/** Files the OS writes on its own; never part of a brand. */
const IGNORED = new Set(['.DS_Store', 'Thumbs.db']);

/**
 * Check one brand folder. Returns
 * `{ dir, id, ok, errors: string[], contrast, permanent: [{ field, value, why }] }`.
 * `reference` is brand/elitea's loaded brand and asset hashes, or null.
 */
export function checkBrand(dir, { reference = null } = {}) {
  const report = { dir, id: null, ok: false, errors: [], contrast: [], permanent: [] };
  let brand;
  try {
    brand = loadBrandFrom(dir);
  } catch (err) {
    if (!(err instanceof BrandError)) throw err;
    report.errors.push(...(err.problems.length ? err.problems : [err.message]));
    return report;
  }
  const { manifest } = brand;
  report.id = brand.id;
  const errors = report.errors;

  for (const name of strayEntries(dir)) errors.push(`${name}: not part of the brand folder format (brand.json, assets/, ${OPTIONAL_ENTRIES.join(', ')})`);
  const fb = join(dir, 'firebase');
  if (existsSync(fb)) for (const f of listFiles(fb)) if (!FIREBASE_FILES.includes(f) && !IGNORED.has(f.split('/').pop())) errors.push(`firebase/${f}: firebase/ holds only ${FIREBASE_FILES.join(' and ')}`);
  if (existsSync(join(dir, 'pack.json'))) {
    try {
      const pack = JSON.parse(readFileSync(join(dir, 'pack.json'), 'utf8'));
      if (pack === null || typeof pack !== 'object' || Array.isArray(pack)) errors.push('pack.json: must be a JSON object (a web brand pack)');
    } catch {
      errors.push('pack.json: not valid JSON');
    }
  }

  const cert = manifest.updates.codeSigningCertificate;
  if (cert !== undefined && !(existsSync(join(dir, cert)) && statSync(join(dir, cert)).isFile())) errors.push(`updates.codeSigningCertificate: ${cert} not found in the brand folder`);

  // Assets.
  const hashes = new Map();
  for (const spec of ASSETS) {
    const file = join(brand.assetsDir, spec.path);
    if (!existsSync(file) || !statSync(file).isFile()) {
      if (!spec.optional) errors.push(`assets/${spec.path}: missing (${spec.use})`);
      continue;
    }
    const buf = readFileSync(file);
    errors.push(...checkAsset(spec, buf));
    hashes.set(spec.path, sha256(buf));
  }
  const iconDoc = join(brand.assetsDir, 'ios/app.icon');
  if (existsSync(join(iconDoc, 'icon.json'))) {
    let doc = {};
    try {
      doc = JSON.parse(readFileSync(join(iconDoc, 'icon.json'), 'utf8'));
    } catch {
      // checkAsset has already reported it.
    }
    const images = (doc.groups ?? []).flatMap((g) => g.layers ?? []).map((l) => l['image-name']).filter(Boolean);
    for (const img of images) {
      const p = join(iconDoc, 'Assets', img);
      if (!existsSync(p)) errors.push(`assets/ios/app.icon/Assets/${img}: named by icon.json but missing`);
      else hashes.set(`ios/app.icon/Assets/${img}`, sha256(readFileSync(p)));
    }
  }

  // Contrast.
  report.contrast = contrastFindings(manifest);
  for (const c of report.contrast) if (!c.ok) errors.push(`brand.hue: contrast ${c.ratio}:1 for ${c.pair} is below ${c.min}:1`);

  // A brand other than ELITEA may not reuse its assets or its names.
  if (reference && brand.id !== DEFAULT_BRAND_ID) {
    const refByHash = new Map([...reference.hashes].map(([p, h]) => [h, p]));
    for (const [p, h] of hashes) if (refByHash.has(h)) errors.push(`assets/${p}: identical to brand/${DEFAULT_BRAND_ID}/assets/${refByHash.get(h)}; a brand uses its own artwork`);
    for (const field of DISTINCT_FIELDS) {
      const mine = fieldValue(manifest, field);
      const theirs = fieldValue(reference.manifest, field);
      if (mine != null && mine === theirs) errors.push(`${field}: "${mine}" is brand/${DEFAULT_BRAND_ID}'s; a brand uses its own`);
    }
    const theirSchemes = new Set(reference.manifest.mobile.connectSchemes);
    for (const s of manifest.mobile.connectSchemes) if (theirSchemes.has(s)) errors.push(`mobile.connectSchemes: "${s}" is brand/${DEFAULT_BRAND_ID}'s; a brand uses its own`);
    const mineName = manifest.productName.toLowerCase();
    for (const word of [reference.manifest.shortName, reference.manifest.productName]) {
      if (new RegExp(`\\b${word.toLowerCase().replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}\\b`).test(mineName)) errors.push(`productName: "${manifest.productName}" contains brand/${DEFAULT_BRAND_ID}'s name "${word}"`);
    }
  }

  report.permanent = Object.entries(PERMANENT_FIELDS).map(([field, why]) => ({ field, value: fieldValue(manifest, field), why }));
  report.hashes = hashes;
  report.manifest = manifest;
  report.ok = errors.length === 0;
  return report;
}

/** brand/elitea's manifest and asset hashes, for the reuse check; null when it does not load. */
export function loadReference(dir = join(BRANDS_DIR, DEFAULT_BRAND_ID)) {
  const report = checkBrand(dir);
  return report.manifest ? { dir: resolve(dir), report, manifest: report.manifest, hashes: report.hashes } : null;
}

/** Problems across brands checked together: a shared id or identifier. */
export function crossBrandErrors(reports) {
  const errors = [];
  const seen = new Map();
  for (const r of reports) {
    if (!r.manifest) continue;
    for (const field of CROSS_BRAND_IDS) {
      const v = fieldValue(r.manifest, field);
      const key = `${field}=${v}`;
      if (seen.has(key)) errors.push(`${field}: "${v}" is used by both ${seen.get(key)} and ${r.dir}`);
      else seen.set(key, r.dir);
    }
  }
  return errors;
}

/** Check `dirs` (absolute); returns `{ ok, reports, cross }`. */
export function run(dirs, { referenceDir } = {}) {
  const reference = loadReference(referenceDir);
  // brand/elitea is checked once: its reference report is reused when it is also a target.
  const reports = dirs.map((dir) => (reference && resolve(dir) === reference.dir ? reference.report : checkBrand(dir, { reference })));
  const cross = crossBrandErrors(reports);
  const needsReference = reports.some((r) => r.id && r.id !== DEFAULT_BRAND_ID);
  if (needsReference && !reference) cross.push(`brand/${DEFAULT_BRAND_ID} does not load, so the asset and name reuse check could not run`);
  return { ok: reports.every((r) => r.ok) && cross.length === 0, reports, cross };
}

function formatReport({ reports, cross }) {
  const lines = [];
  for (const r of reports) {
    const shown = relative(process.cwd(), r.dir) || '.';
    lines.push(`${r.ok ? 'PASS' : 'FAIL'} ${shown}${r.id ? ` (${r.id})` : ''}`);
    for (const e of r.errors) lines.push(`  ✗ ${e}`);
    for (const c of r.contrast) lines.push(`  ${c.ok ? '✓' : '✗'} contrast ${c.ratio}:1 (≥ ${c.min}:1) ${c.pair}`);
    for (const p of r.permanent) lines.push(`  ⚑ PERMANENT ${p.field} = ${p.value}: ${p.why}; never change it after the brand's first upload`);
  }
  for (const e of cross) lines.push(`FAIL ${e}`);
  return lines.join('\n');
}

function main(argv) {
  const json = argv.includes('--json');
  const args = argv.filter((a) => a !== '--json');
  if (args.some((a) => a.startsWith('-'))) {
    console.error('usage: node scripts/brand-check.mjs [--json] [brand-folder ...]');
    return 2;
  }
  let dirs;
  try {
    dirs = args.length ? args.map((a) => resolve(a)) : [resolveBrandDir(process.env[BRAND_ENV])];
  } catch (err) {
    console.error(err.message);
    return 2;
  }
  const result = run(dirs);
  if (json) {
    const strip = ({ hashes, manifest, ...r }) => ({ ...r, dir: relative(REPO_ROOT, r.dir) || '.' });
    console.log(JSON.stringify({ ok: result.ok, reports: result.reports.map(strip), cross: result.cross }, null, 2));
  } else {
    console.log(formatReport(result));
  }
  return result.ok ? 0 : 1;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) process.exitCode = main(process.argv.slice(2));
