import { readFileSync, readdirSync, statSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

import { PRODUCT_LITERAL_ALLOWED, productLiterals } from './product-literals.mjs';

const APP_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');

describe('productLiterals', () => {
  it('finds the product name in strings, templates and JSX text', () => {
    const found = productLiterals(
      "const a = 'Welcome to Elitea';\nconst b = `Open ${x} in ELITEA`;\nconst c = <p>Your EliteA workspace</p>;",
      { jsx: true },
    );
    expect(found).toEqual([
      { line: 1, text: 'Welcome to Elitea' },
      { line: 2, text: 'in ELITEA' },
      { line: 3, text: 'Your EliteA workspace' },
    ]);
  });

  it('ignores comments, identifiers, imports and wire names', () => {
    const source = [
      '// Elitea in a comment',
      "import x from '@/widgets/elitea-section';",
      'const EliteaAssistant = 1;',
      "const h = { 'X-Elitea-Wiki-Chat': 1, env: 'ELITEA_AI_PROJECT_ID', url: 'https://docs.elitea.ai', p: 'elitea/x' };",
      'const ok = `${productName()} MCP Client`;',
    ].join('\n');
    expect(productLiterals(source)).toEqual([]);
  });
});

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

describe('the web client names no product in a string (ADR-0052 W3.3)', () => {
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
