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
const { join, relative, resolve, sep } = require('node:path') as {
  join(...p: string[]): string;
  relative(from: string, to: string): string;
  resolve(...p: string[]): string;
  sep: string;
};
const { existsSync } = require('node:fs') as { existsSync(p: string): boolean };
const { pathToFileURL } = require('node:url') as { pathToFileURL(p: string): { href: string } };
declare const __dirname: string;
declare const process: { env: Record<string, string | undefined>; execPath: string };

const LOADER = resolve(__dirname, '../../../../scripts/brand/load.mjs');

/** The repository root, as load.mjs exports it. */
export const REPO_ROOT = resolve(__dirname, '../../../..');

interface LoadedBrand {
  dir: string;
  id: string;
  manifest: Record<string, unknown>;
  assetsDir: string;
  path(rel: string): string;
  has(rel: string): boolean;
  relativeTo(from: string, rel: string): string;
}

export function loadBrand({ env = process.env }: { env?: Record<string, string | undefined> } = {}): LoadedBrand {
  const script = `import { loadBrand } from ${JSON.stringify(pathToFileURL(LOADER).href)};
const b = loadBrand();
process.stdout.write(JSON.stringify({ dir: b.dir, id: b.id, manifest: b.manifest, assetsDir: b.assetsDir }));`;
  const out = execFileSync(process.execPath, ['--input-type=module', '-e', script], { env, encoding: 'utf8' });
  const brand = JSON.parse(out) as Omit<LoadedBrand, 'path' | 'has' | 'relativeTo'>;
  // The same three helpers as load.mjs, over the folder the real loader resolved.
  return {
    ...brand,
    path: (rel: string) => join(brand.dir, rel),
    has: (rel: string) => existsSync(join(brand.dir, rel)),
    relativeTo: (from: string, rel: string) => {
      const p = relative(from, join(brand.dir, rel)).split(sep).join('/');
      return p.startsWith('.') ? p : `./${p}`;
    },
  };
}
