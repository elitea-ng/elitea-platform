import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vitest/config';

/**
 * Dedicated vitest config for the R-M5 RED/GREEN fixture runs driven by
 * scripts/check-gates-selftest.mjs. The fixtures live OUTSIDE src/ (they are
 * deliberately-failing tests and must never run in the real suite), but they
 * boot the REAL src/test/setup.ts so what is proven is the production fence,
 * not a copy.
 */
const APP_DIR = resolve(dirname(fileURLToPath(import.meta.url)), '../..');

export default defineConfig({
  // Booting the REAL src/test/setup.ts means honouring the real `@/*` → `./src/*`
  // alias from tsconfig.json. Without it, any `@/`-prefixed import setup.ts grows
  // fails the whole fixture run with ERR_MODULE_NOT_FOUND, which reads as "the
  // rule's failing fixture passed" and reds the gate for an unrelated reason —
  // exactly what `@/shared/lib/webstorage.testshim` did when setup.ts began
  // importing it.
  resolve: {
    alias: { '@': resolve(APP_DIR, 'src') },
  },
  test: {
    environment: 'node',
    setupFiles: [resolve(APP_DIR, 'src/test/setup.ts')],
    include: ['tools/lint-rules/fixtures/msw/*.test.ts'],
  },
});
