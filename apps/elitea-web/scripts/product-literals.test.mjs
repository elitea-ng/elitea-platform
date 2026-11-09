// The whole-tree half of the product-literal guard (ADR-0052 W3.3): every
// source file of the app, parsed. Kept out of scripts/lib/ so the coverage
// job (which instruments scripts/lib) does not parse 6,000 files under v8.
import { readFileSync, readdirSync, statSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

import { PRODUCT_LITERAL_ALLOWED, productLiterals } from './lib/product-literals.mjs';

const APP_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

/** Every non-test source file of the app, posix and app-relative. */
function sourceFiles(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const full = path.join(dir, name);
    if (statSync(full).isDirectory()) {
      if (name !== 'generated' && name !== '__tests__') sourceFiles(full, out);
    } else if (/\.tsx?$/.test(name) && !/\.(test|stories|d)\.tsx?$/.test(name) && !name.endsWith('.d.ts')) {
      out.push(path.relative(APP_ROOT, full).split(path.sep).join('/'));
    }
  }
  return out;
}

describe('the web client names no product in a string (ADR-0052 W3.3)', { timeout: 60_000 }, () => {
  const files = sourceFiles(path.join(APP_ROOT, 'src'));

  it('reads the source tree', () => {
    expect(files.length).toBeGreaterThan(1000);
  });

  it('uses productName() or {{product}} everywhere outside the allow-list', () => {
    const found = files
      .filter((file) => !PRODUCT_LITERAL_ALLOWED.has(file))
      .flatMap((file) =>
        productLiterals(readFileSync(path.join(APP_ROOT, file), 'utf8'), { jsx: file.endsWith('x') }).map(
          ({ line, text }) => `${file}:${line}: ${text.slice(0, 80)}`,
        ),
      );
    expect(found).toEqual([]);
  });

  it('allows only files that exist and still name it', () => {
    for (const file of PRODUCT_LITERAL_ALLOWED.keys()) {
      const text = readFileSync(path.join(APP_ROOT, file), 'utf8');
      expect(productLiterals(text, { jsx: file.endsWith('x') }), file).not.toEqual([]);
    }
  });
});
