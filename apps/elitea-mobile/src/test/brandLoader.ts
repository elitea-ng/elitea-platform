/**
 * jest: jest's module runtime cannot load the ES module `scripts/brand/load.mjs`
 * that app.config.ts requires. package.json's moduleNameMapper sends that path
 * here instead. This runs the REAL loader in a child node process (which loads
 * ES modules natively) and returns what it read, so tests see exactly the
 * brand an `expo config` run would, with no second implementation.
 */
// Typed requires, as in nodeSqlite.ts: the app's tsconfig carries no node types.
const { execFileSync } = require('node:child_process') as {
  execFileSync(file: string, args: string[], options: { env: Record<string, string | undefined>; encoding: 'utf8' }): string;
};
const { join, resolve } = require('node:path') as { join(...p: string[]): string; resolve(...p: string[]): string };
const { pathToFileURL } = require('node:url') as { pathToFileURL(p: string): { href: string } };
declare const __dirname: string;
declare const process: { env: Record<string, string | undefined>; execPath: string };

const LOADER = resolve(__dirname, '../../../../scripts/brand/load.mjs');

interface LoadedBrand {
  dir: string;
  id: string;
  manifest: Record<string, unknown>;
  assetsDir: string;
  path(rel: string): string;
}

export function loadBrand({ env = process.env }: { env?: Record<string, string | undefined> } = {}): LoadedBrand {
  const script = `import { loadBrand } from ${JSON.stringify(pathToFileURL(LOADER).href)};
const b = loadBrand();
process.stdout.write(JSON.stringify({ dir: b.dir, id: b.id, manifest: b.manifest, assetsDir: b.assetsDir }));`;
  const out = execFileSync(process.execPath, ['--input-type=module', '-e', script], { env, encoding: 'utf8' });
  const brand = JSON.parse(out) as Omit<LoadedBrand, 'path'>;
  return { ...brand, path: (rel: string) => join(brand.dir, rel) };
}
