// The one reader of brand manifests (ADR-0052 decision 2).
//
// Every generator (mobile app.config.ts, the desktop tauri.brand.json writer,
// the web default-pack build) and brand-check read a brand through this
// module. Nothing else opens a brand.json.
//
//   ELITEA_BRAND unset          → <repo>/brand/elitea
//   ELITEA_BRAND=sample         → <repo>/brand/sample (a bare id: a folder under brand/)
//   ELITEA_BRAND=../my/brand    → that folder (a path: relative to the current
//                                 directory, or absolute), e.g. a brand repository
//
// Node's standard library only.

import { existsSync, readFileSync, statSync } from 'node:fs';
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { BRAND_ID, RESERVED_IDS, validateManifest } from './schema.mjs';

/** The repository root (this file is scripts/brand/load.mjs). */
export const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../..');

/** The folder that holds the repository's brands and the brand guide site. */
export const BRANDS_DIR = join(REPO_ROOT, 'brand');

/** The brand built when ELITEA_BRAND is unset: EPAM's ELITEA (owner decision 4). */
export const DEFAULT_BRAND_ID = 'elitea';

/** The environment variable that selects the brand. */
export const BRAND_ENV = 'ELITEA_BRAND';

export class BrandError extends Error {
  constructor(message, problems = []) {
    super(problems.length ? `${message}\n${problems.map((p) => `  - ${p}`).join('\n')}` : message);
    this.name = 'BrandError';
    this.problems = problems;
  }
}

/**
 * The absolute brand folder that `value` (an ELITEA_BRAND value) names.
 * Unset or empty → brand/elitea. A bare brand id → brand/<id>. Anything with
 * a path separator or a leading "." → a path from `cwd`.
 */
export function resolveBrandDir(value, { cwd = process.cwd() } = {}) {
  const v = (value ?? '').trim();
  if (v === '') return join(BRANDS_DIR, DEFAULT_BRAND_ID);
  if (!v.includes('/') && !v.includes('\\') && !v.startsWith('.')) {
    if (RESERVED_IDS.includes(v)) throw new BrandError(`${BRAND_ENV}=${v}: "${v}" is a brand guide folder, not a brand (reserved: ${RESERVED_IDS.join(', ')})`);
    if (!BRAND_ID.test(v)) throw new BrandError(`${BRAND_ENV}=${v}: not a brand id and not a path`);
    return join(BRANDS_DIR, v);
  }
  return isAbsolute(v) ? v : resolve(cwd, v);
}

/**
 * Read and validate the brand in `dir`. Returns
 * `{ dir, id, manifest, assetsDir, path(rel), has(rel), readText(rel), relativeTo(from, rel) }`:
 * `path` is absolute; `has` says whether the file exists; `relativeTo` gives the
 * file as a `/`-separated path relative to the directory `from`, so a generated
 * config holds no machine path; throws BrandError with every
 * problem when the manifest is missing, unparsable or invalid, or when the
 * folder's name is not the manifest's id.
 */
export function loadBrandFrom(dir) {
  if (!existsSync(dir) || !statSync(dir).isDirectory()) throw new BrandError(`brand folder not found: ${dir}`);
  const file = join(dir, 'brand.json');
  if (!existsSync(file)) throw new BrandError(`no brand.json in ${dir}`);
  let manifest;
  try {
    manifest = JSON.parse(readFileSync(file, 'utf8'));
  } catch (err) {
    throw new BrandError(`${file}: not valid JSON (${err.message})`);
  }
  const problems = validateManifest(manifest);
  if (problems.length === 0 && basename(dir) !== manifest.id) {
    problems.push(`id: "${manifest.id}" must match the folder name "${basename(dir)}"`);
  }
  if (problems.length) throw new BrandError(`${file} is not a valid brand manifest:`, problems);
  return Object.freeze({
    dir,
    id: manifest.id,
    manifest: deepFreeze(manifest),
    assetsDir: join(dir, 'assets'),
    path: (rel) => join(dir, rel),
    has: (rel) => existsSync(join(dir, rel)),
    readText: (rel) => readFileSync(join(dir, rel), 'utf8'),
    relativeTo: (from, rel) => {
      const p = relative(from, join(dir, rel)).split(sep).join('/');
      return p.startsWith('.') ? p : `./${p}`;
    },
  });
}

/**
 * The selected brand: ELITEA_BRAND from `env` (default process.env),
 * resolved against `cwd`, read and validated.
 */
export function loadBrand({ env = process.env, cwd = process.cwd() } = {}) {
  return loadBrandFrom(resolveBrandDir(env[BRAND_ENV], { cwd }));
}

function deepFreeze(v) {
  if (v && typeof v === 'object') {
    for (const k of Object.keys(v)) deepFreeze(v[k]);
    Object.freeze(v);
  }
  return v;
}
