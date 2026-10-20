// A React Native Modal is its own native window, drawn above LockGate's cover:
// a sheet opened through it would stay usable on top of the lock screen (SEC-1).
// Every sheet goes through src/security/lock/LockAwareModal.tsx instead.
// Run with `npm run test:scripts` (node:test).
import assert from 'node:assert/strict';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';

const src = fileURLToPath(new URL('../src', import.meta.url));
const ALLOWED = 'security/lock/LockAwareModal.tsx';

function sources(dir) {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) return sources(path);
    return /\.tsx?$/.test(name) && !/\.test\.tsx?$/.test(name) ? [path] : [];
  });
}

describe('LockAwareModal', () => {
  it("no other source imports react-native's Modal", () => {
    const offenders = sources(src)
      .filter((path) => relative(src, path) !== ALLOWED)
      .filter((path) => /import\s*\{[^}]*\bModal\b[^}]*\}\s*from\s*'react-native'/.test(readFileSync(path, 'utf8')))
      .map((path) => relative(src, path));
    assert.deepEqual(offenders, []);
  });

  it('the allowed file exists (the check would pass vacuously otherwise)', () => {
    assert.match(readFileSync(join(src, ALLOWED), 'utf8'), /import \{ Modal, type ModalProps \} from 'react-native'/);
  });
});
