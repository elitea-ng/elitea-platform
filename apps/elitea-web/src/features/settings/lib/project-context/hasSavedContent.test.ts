import { describe, expect, it } from 'vitest';

import { hasSavedProjectContext } from './hasSavedContent';

describe('hasSavedProjectContext', () => {
  it('is true only for a non-empty saved context', () => {
    expect(hasSavedProjectContext({ content: 'Our team ships on Fridays.', enabled: true })).toBe(true);
  });

  it('treats whitespace as no context, like the reference', () => {
    expect(hasSavedProjectContext({ content: '   \n\t ' })).toBe(false);
    expect(hasSavedProjectContext({ content: '' })).toBe(false);
  });

  it('is false for every not-yet-loaded or malformed shape rather than throwing', () => {
    // A failed read reaches this with `undefined`; the caller shows its error
    // branch first, but this must not be what decides that.
    expect(hasSavedProjectContext(undefined)).toBe(false);
    expect(hasSavedProjectContext(null)).toBe(false);
    expect(hasSavedProjectContext({})).toBe(false);
    expect(hasSavedProjectContext({ content: 42 })).toBe(false);
    expect(hasSavedProjectContext('a string')).toBe(false);
  });

  it('ignores `enabled` — a turned-off context is still a saved one', () => {
    // The disabled banner is the editor's job. Sending a project with saved
    // text back to the "Still no Project Context" screen because someone
    // flipped the switch would hide content the project actually has.
    expect(hasSavedProjectContext({ content: 'text', enabled: false })).toBe(true);
  });
});
