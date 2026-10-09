import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { describe, expect, it } from 'vitest';
import { z } from 'zod';

import './zodJitless';

describe('zodJitless', () => {
  it('turns off the eval-based JIT', () => {
    expect(z.config().jitless).toBe(true);
  });

  // ES modules evaluate in import order, so the first import is the only
  // position that runs before every schema-defining module.
  it.each(['src/app/main.tsx', 'src/entries/admin/main.tsx'])('%s imports it first', (entry) => {
    const source = readFileSync(resolve(__dirname, '../../..', entry), 'utf8');
    const firstImport = /^import\s[^;]*;/m.exec(source)?.[0];
    expect(firstImport).toBe("import '@/shared/config/zodJitless';");
  });
});
