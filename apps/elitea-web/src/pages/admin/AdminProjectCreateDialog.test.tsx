import { describe, expect, it } from 'vitest';

import { sanitizeEmail } from './AdminProjectCreateDialog';

/* onetest: elitea_issues: #4626 — hidden Unicode format characters (Word Joiner, zero-width
 * space/joiner/non-joiner, BOM, soft hyphen) copied alongside an email address are stripped
 * before the address is used for admin matching; `.trim()` alone does not touch them, since none
 * of them are whitespace. */
describe('sanitizeEmail (#4626)', () => {
  it('strips a Word Joiner (U+2060) hidden inside the address', () => {
    expect(sanitizeEmail('alice⁠@example.com')).toBe('alice@example.com');
  });

  it('strips a zero-width space, zero-width non-joiner/joiner, BOM, and soft hyphen', () => {
    expect(sanitizeEmail('a​l‌i‍c﻿e­@example.com')).toBe('alice@example.com');
  });

  it('still trims ordinary surrounding whitespace', () => {
    expect(sanitizeEmail('  bob@example.com  ')).toBe('bob@example.com');
  });

  it('leaves an already-clean address untouched', () => {
    expect(sanitizeEmail('carol@example.com')).toBe('carol@example.com');
  });
});
